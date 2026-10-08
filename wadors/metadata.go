package wadors

import (
	"bufio"
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
// read and parsed only as the caller advances, and each top-level JSON array
// is decoded element by element, so peak memory stays at a single dataset
// instead of the whole study. Close must be called when done.
type MetadataStream struct {
	mp    *Multipart
	base  *url.URL
	items *partItems // streaming item cursor of the current part
}

// Next returns the next dataset; it returns io.EOF when the response is
// exhausted. Relative BulkDataURIs are resolved against the metadata request
// URL per PS3.18.
func (s *MetadataStream) Next() (*dataset.Dataset, error) {
	for {
		if s.items != nil {
			raw, ok, err := s.items.next()
			if err != nil {
				return nil, err
			}
			if ok {
				return metadataDataset(raw, s.base)
			}
			s.items = nil
		}
		p, err := s.mp.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		s.items, err = newPartItems(p)
		if err != nil {
			return nil, err
		}
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

// partItems streams the dataset items of one metadata response part: the
// elements of a top-level JSON array (the conformant form — PS3.18 Annex F
// encodes multiple instances as one array) or the single bare object some
// servers send per multipart part. Array elements are decoded one at a time,
// so peak memory stays at a single item no matter how long the array is and
// the first item is available before the part has fully arrived.
type partItems struct {
	dec   *json.Decoder
	array bool
	done  bool
}

// newPartItems classifies the part by its first non-whitespace byte and
// wraps it in a streaming decoder. An empty part yields no items.
func newPartItems(r io.Reader) (*partItems, error) {
	br := bufio.NewReader(r)
	first, err := firstNonSpace(br)
	if errors.Is(err, io.EOF) {
		return &partItems{done: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if first != '[' && first != '{' {
		return nil, fmt.Errorf("wadors: decode metadata: unexpected character %q", first)
	}
	pi := &partItems{
		dec:   json.NewDecoder(io.MultiReader(bytes.NewReader([]byte{first}), br)),
		array: first == '[',
	}
	if pi.array {
		// Consume the opening bracket so More()/Decode() address the array
		// elements, not the array as a single value.
		if _, err := pi.dec.Token(); err != nil {
			return nil, fmt.Errorf("wadors: decode metadata: %w", err)
		}
	}
	return pi, nil
}

// next returns the next raw item of the part; ok is false once the part is
// exhausted.
func (pi *partItems) next() (raw json.RawMessage, ok bool, err error) {
	if pi.done {
		return nil, false, nil
	}
	if !pi.array {
		pi.done = true
		if err := pi.dec.Decode(&raw); err != nil {
			return nil, false, fmt.Errorf("wadors: decode metadata: %w", err)
		}
		if err := requireDecEOF(pi.dec); err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	if !pi.dec.More() {
		pi.done = true
		// Consume the closing bracket, then reject trailing content like a
		// plain json.Unmarshal of the whole part would.
		if _, err := pi.dec.Token(); err != nil {
			return nil, false, fmt.Errorf("wadors: decode metadata: %w", err)
		}
		if err := requireDecEOF(pi.dec); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := pi.dec.Decode(&raw); err != nil {
		return nil, false, fmt.Errorf("wadors: decode metadata: %w", err)
	}
	return raw, true, nil
}

// requireDecEOF verifies the decoder is exhausted after a complete
// top-level value, mirroring the trailing-content strictness of
// json.Unmarshal.
func requireDecEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("wadors: decode metadata: trailing content after top-level value")
	}
	return nil
}

// firstNonSpace returns the first byte of br that is not JSON whitespace.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return b, nil
		}
	}
}

// resolveBulkDataURIsJSON rewrites relative BulkDataURI values to absolute
// URLs at the JSON level. PS3.18 resolves the relative form against the URL
// of the metadata request that produced it — this is WADO-specific URL
// rewriting; every aspect of interpreting dicom+json stays with go-dicom.
func resolveBulkDataURIsJSON(raw json.RawMessage, base *url.URL) (json.RawMessage, error) {
	// Fast paths: items not mentioning the key, and items whose every
	// BulkDataURI is scheme-absolute, skip the decode / walk / re-encode
	// round trip entirely (that pass costs several times the item size and
	// dominates metadata parsing when pixel data is bulk-referenced). Both
	// probes are safe superset filters: false positives just fall through to
	// the walk, which finds nothing to change.
	if !bytes.Contains(raw, []byte("BulkDataURI")) {
		return raw, nil
	}
	if !hasRelativeBulkDataURI(raw) {
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

// hasRelativeBulkDataURI reports whether raw carries a BulkDataURI value
// that is not scheme-absolute — i.e. anything the JSON walk might rewrite.
//
// For each occurrence of the key, the value bytes are classified per RFC
// 3986: in an absolute URI a valid scheme ("http:", "ftp:", ...) always
// precedes the first '/', '?', '#' or end; a conforming relative reference
// can never have one there. Any uncertain shape (odd spacing, escapes, a
// colon that is not a valid scheme) reports true so the authoritative walk
// decides.
func hasRelativeBulkDataURI(raw []byte) bool {
	const key = `"BulkDataURI"`
	for i := 0; i < len(raw); {
		j := bytes.Index(raw[i:], []byte(key))
		if j < 0 {
			return false
		}
		i += j + len(key)
		// Skip JSON whitespace, expect ':', skip whitespace again, expect
		// the opening quote of the value; anything else moves on to the next
		// occurrence (e.g. the word appearing inside another string value).
		k := skipJSONSpace(raw, i)
		if k >= len(raw) || raw[k] != ':' {
			continue
		}
		k = skipJSONSpace(raw, k+1)
		if k >= len(raw) || raw[k] != '"' {
			continue
		}
		k++
		schemeOK := true // value so far is a valid scheme prefix
		firstByte := true
		schemed := false
		for k < len(raw) {
			c := raw[k]
			if c == ':' {
				if schemeOK && !firstByte {
					schemed = true // valid scheme before the colon
				}
				break
			}
			if c == '/' || c == '?' || c == '#' || c == '"' {
				break // path, query, fragment or end of a relative reference
			}
			schemeOK = schemeOK && isSchemeChar(c, firstByte)
			firstByte = false
			k++
		}
		if !schemed {
			// Relative reference, empty value, invalid scheme, or an
			// inconclusive scan: the walk is the authority.
			return true
		}
		// Absolute value; keep scanning for further occurrences.
	}
	return false
}

// skipJSONSpace returns the index of the first byte at or after i that is
// not a JSON whitespace byte.
func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// isSchemeChar reports whether c may appear in a URI scheme, where first
// selects the stricter first-character rule (letters only) of RFC 3986.
func isSchemeChar(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		return !first
	default:
		return false
	}
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
