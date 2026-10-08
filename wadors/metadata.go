package wadors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/serialization"
)

// StudyMetadata retrieves Study-level metadata (application/dicom+json, one
// item per instance). dicom+json parsing is delegated to go-dicom's
// serialization.FromJSON; relative BulkDataURIs in the returned datasets are
// already resolved to absolute URLs (relative to this request URL).
//
// For very large studies prefer StudyMetadataStream: it keeps peak memory at
// one response part instead of the whole response.
func (c *Client) StudyMetadata(ctx context.Context, studyUID string, opts ...RetrieveOption) ([]*dataset.Dataset, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.metadataList(ctx, c.resourceURL("studies", studyUID, "metadata"), opts)
}

// SeriesMetadata retrieves Series-level metadata.
func (c *Client) SeriesMetadata(ctx context.Context, studyUID, seriesUID string, opts ...RetrieveOption) ([]*dataset.Dataset, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID); err != nil {
		return nil, err
	}
	return c.metadataList(ctx, c.resourceURL("studies", studyUID, "series", seriesUID, "metadata"), opts)
}

// StudyMetadataStream streams Study-level metadata item by item; Close must
// be called when done.
func (c *Client) StudyMetadataStream(ctx context.Context, studyUID string, opts ...RetrieveOption) (*MetadataStream, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.metadataStream(ctx, c.resourceURL("studies", studyUID, "metadata"), opts)
}

// SeriesMetadataStream streams Series-level metadata item by item; Close must
// be called when done.
func (c *Client) SeriesMetadataStream(ctx context.Context, studyUID, seriesUID string, opts ...RetrieveOption) (*MetadataStream, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID); err != nil {
		return nil, err
	}
	return c.metadataStream(ctx, c.resourceURL("studies", studyUID, "series", seriesUID, "metadata"), opts)
}

// InstanceMetadata retrieves single-instance metadata.
func (c *Client) InstanceMetadata(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RetrieveOption) (*dataset.Dataset, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	ms, err := c.metadataStream(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "metadata"), opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ms.Close() }()
	ds, err := ms.Next()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("wadors: instance metadata: expected a single item, got 0")
	}
	if err != nil {
		return nil, err
	}
	if _, err := ms.Next(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("wadors: instance metadata: expected a single item, got more than one")
	}
	return ds, nil
}

func (c *Client) metadataList(ctx context.Context, u *url.URL, opts []RetrieveOption) ([]*dataset.Dataset, error) {
	ms, err := c.metadataStream(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ms.Close() }()
	out := make([]*dataset.Dataset, 0, 16)
	for {
		ds, err := ms.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ds)
	}
}

// MetadataStream is a streaming cursor over a metadata response: parts are
// read and parsed only as the caller advances, so peak memory stays at one
// response part instead of the whole study. Close must be called when done.
type MetadataStream struct {
	mp      *Multipart
	base    *url.URL
	pending []json.RawMessage
}

// Next returns the next dataset; it returns io.EOF when the response is
// exhausted. Relative BulkDataURIs are resolved against the metadata request
// URL per PS3.18.
func (s *MetadataStream) Next() (*dataset.Dataset, error) {
	for {
		if len(s.pending) > 0 {
			item := s.pending[0]
			s.pending = s.pending[1:]
			return metadataDataset(item, s.base)
		}
		p, err := s.mp.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		items, err := metadataPartItems(b)
		if err != nil {
			return nil, err
		}
		s.pending = items
	}
}

// Close closes the underlying response.
func (s *MetadataStream) Close() error { return s.mp.Close() }

// metadataDataset parses one metadata item (a bare dataset object; array
// unwrapping happens in metadataPartItems).
func metadataDataset(raw json.RawMessage, reqURL *url.URL) (*dataset.Dataset, error) {
	fixed, err := resolveBulkDataURIsJSON(raw, reqURL)
	if err != nil {
		return nil, err
	}
	ds, err := serialization.FromJSON(fixed)
	if err != nil {
		return nil, fmt.Errorf("wadors: parse dicom+json: %w", err)
	}
	return ds, nil
}

// metadataStream fetches a metadata resource and returns a streaming cursor
// over its dataset items.
//
// Each part is parsed when reached: a part carrying a JSON array contributes
// its elements, a part carrying a bare object contributes itself. Older
// servers that wrap dicom+json in multipart/related with one dataset per part
// therefore parse correctly instead of yielding invalid concatenated JSON.
// The transfer-syntax RetrieveOption is not emitted here: metadata responses
// are always dicom+json (see WithTransferSyntax).
func (c *Client) metadataStream(ctx context.Context, u *url.URL, opts []RetrieveOption) (*MetadataStream, error) {
	cfg := buildRetrieveCfg(opts)
	applyCharset(u, cfg.charset)
	resp, err := c.do(ctx, u, func(req *http.Request) {
		accept := cfg.acceptOverride
		if accept == "" {
			accept = "application/dicom+json"
		}
		req.Header.Set("Accept", accept)
	})
	if err != nil {
		return nil, err
	}
	mp, err := newMultipart(resp)
	if err != nil {
		return nil, err
	}
	// The final (post-redirect) request URL is the PS3.18 base for relative
	// BulkDataURI resolution.
	return &MetadataStream{mp: mp, base: resp.Request.URL}, nil
}

// metadataPartItems extracts the dataset items of one metadata part: either
// the elements of a JSON array (the conformant form) or the single bare
// object some servers send.
func metadataPartItems(body []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("wadors: decode metadata: %w", err)
		}
		return items, nil
	}
	return []json.RawMessage{json.RawMessage(trimmed)}, nil
}

// resolveBulkDataURIsJSON rewrites relative BulkDataURI values to absolute
// URLs at the JSON level. PS3.18 resolves the relative form against the URL
// of the metadata request that produced it — this is WADO-specific URL
// rewriting; every aspect of interpreting dicom+json stays with go-dicom.
func resolveBulkDataURIsJSON(raw json.RawMessage, base *url.URL) (json.RawMessage, error) {
	// Fast path: items not mentioning the key skip the decode / walk /
	// re-encode round trip entirely (a decode-rewrite-recode pass would
	// otherwise cost several times the response size on large studies). The
	// substring probe is a safe superset filter: if the bytes are absent the
	// key cannot be present, and a false positive (the text inside some other
	// value) just falls through to the walk, which finds nothing to change.
	if !bytes.Contains(raw, []byte("BulkDataURI")) {
		return raw, nil
	}
	var v any
	// UseNumber keeps numeric literals verbatim across the decode / re-encode
	// round trip: DICOM JSON numbers (DS precision, large IS/UL values) must
	// not be perturbed by float64 conversion.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("wadors: decode metadata json: %w", err)
	}
	if !resolveBulkDataValue(v, base) {
		return raw, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("wadors: encode metadata json: %w", err)
	}
	return out, nil
}

// resolveBulkDataValue walks decoded JSON (maps/arrays) rewriting relative
// "BulkDataURI" string values; it reports whether anything changed.
func resolveBulkDataValue(v any, base *url.URL) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "BulkDataURI" {
				if s, ok := val.(string); ok && s != "" {
					if u, err := url.Parse(s); err == nil && !u.IsAbs() {
						if abs := base.ResolveReference(u); abs != nil {
							t[k] = abs.String()
							changed = true
						}
					}
				}
				continue
			}
			if resolveBulkDataValue(val, base) {
				changed = true
			}
		}
	case []any:
		for _, item := range t {
			if resolveBulkDataValue(item, base) {
				changed = true
			}
		}
	}
	return changed
}
