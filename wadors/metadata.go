package wadors

import (
	"context"
	"encoding/json"
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

// InstanceMetadata retrieves single-instance metadata.
func (c *Client) InstanceMetadata(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RetrieveOption) (*dataset.Dataset, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	body, reqURL, err := c.fetchMetadata(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "metadata"), opts)
	if err != nil {
		return nil, err
	}
	return metadataDataset(body, reqURL)
}

func (c *Client) metadataList(ctx context.Context, u *url.URL, opts []RetrieveOption) ([]*dataset.Dataset, error) {
	body, reqURL, err := c.fetchMetadata(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("wadors: decode metadata: %w", err)
	}
	out := make([]*dataset.Dataset, 0, len(items))
	for _, item := range items {
		ds, err := metadataDataset(item, reqURL)
		if err != nil {
			return nil, err
		}
		out = append(out, ds)
	}
	return out, nil
}

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

// fetchMetadata fetches metadata; it tolerates older servers that wrap
// dicom+json in multipart.
func (c *Client) fetchMetadata(ctx context.Context, u *url.URL, opts []RetrieveOption) ([]byte, *url.URL, error) {
	cfg := buildRetrieveCfg(opts)
	if cfg.transferSyntax != "" {
		if err := c.core.CheckUID("transferSyntax", cfg.transferSyntax); err != nil {
			return nil, nil, err
		}
	}
	resp, err := c.do(ctx, u, func(req *http.Request) {
		accept := cfg.acceptOverride
		if accept == "" {
			accept = "application/dicom+json"
			if cfg.transferSyntax != "" {
				accept += `; transfer-syntax=` + cfg.transferSyntax
			}
		}
		req.Header.Set("Accept", accept)
		if cfg.charset != "" {
			req.Header.Set("Accept-Charset", cfg.charset)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	mp, err := newMultipart(resp)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = mp.Close() }()
	var body []byte
	for p, err := range mp.Parts() {
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, nil, err
		}
		body = append(body, b...)
	}
	return body, resp.Request.URL, nil
}

// resolveBulkDataURIsJSON rewrites relative BulkDataURI values to absolute
// URLs at the JSON level. PS3.18 resolves the relative form against the URL
// of the metadata request that produced it — this is WADO-specific URL
// rewriting; every aspect of interpreting dicom+json stays with go-dicom.
func resolveBulkDataURIsJSON(raw json.RawMessage, base *url.URL) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
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
