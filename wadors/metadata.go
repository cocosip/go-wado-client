package wadors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/serialization"

	"github.com/cocosip/go-wado-client/internal/dicomjson"
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
	items *dicomjson.Items // streaming item cursor of the current part
}

// Next returns the next dataset; it returns io.EOF when the response is
// exhausted. Relative BulkDataURIs are resolved against the metadata request
// URL per PS3.18.
func (s *MetadataStream) Next() (*dataset.Dataset, error) {
	for {
		if s.items != nil {
			raw, ok, err := s.items.Next()
			if err != nil {
				return nil, fmt.Errorf("wadors: decode metadata: %w", err)
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
		if s.items, err = dicomjson.NewItems(p); err != nil {
			return nil, fmt.Errorf("wadors: decode metadata: %w", err)
		}
	}
}

// Close closes the underlying response.
func (s *MetadataStream) Close() error { return s.mp.Close() }

// metadataDataset parses one metadata item (a bare dataset object; array
// unwrapping happens in the dicomjson item cursor).
func metadataDataset(raw json.RawMessage, reqURL *url.URL) (*dataset.Dataset, error) {
	fixed, err := dicomjson.ResolveBulkDataURIs(raw, reqURL)
	if err != nil {
		return nil, fmt.Errorf("wadors: decode metadata json: %w", err)
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
