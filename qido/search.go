package qido

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cocosip/go-wado-client/internal/multipartx"
)

// MediaTypeDICOMJSON is the default search Accept media type (PS3.18
// §10.6.1: application/dicom+json or multipart/related with
// application/dicom+xml; this client parses dicom+json).
const MediaTypeDICOMJSON = "application/dicom+json"

// SearchStudies searches for studies (PS3.18 §10.6.1, /studies).
func (c *Client) SearchStudies(ctx context.Context, q Query, opts ...SearchOption) (*Results, error) {
	return c.search(ctx, c.svc.ResourceURL("studies"), q, opts)
}

// SearchStudiesStream streams the matching studies dataset by dataset;
// Close must be called when done.
func (c *Client) SearchStudiesStream(ctx context.Context, q Query, opts ...SearchOption) (*ResultsStream, error) {
	return c.searchStream(ctx, c.svc.ResourceURL("studies"), q, opts)
}

// SearchSeries searches the series of one study
// (/studies/{study}/series).
func (c *Client) SearchSeries(ctx context.Context, studyUID string, q Query, opts ...SearchOption) (*Results, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.search(ctx, c.svc.ResourceURL("studies", studyUID, "series"), q, opts)
}

// SearchSeriesStream streams the matching series dataset by dataset.
func (c *Client) SearchSeriesStream(ctx context.Context, studyUID string, q Query, opts ...SearchOption) (*ResultsStream, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.searchStream(ctx, c.svc.ResourceURL("studies", studyUID, "series"), q, opts)
}

// SearchStudyInstances searches the instances of one study
// (/studies/{study}/instances — the relational form; optional per the
// standard, servers may answer 4xx).
func (c *Client) SearchStudyInstances(ctx context.Context, studyUID string, q Query, opts ...SearchOption) (*Results, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.search(ctx, c.svc.ResourceURL("studies", studyUID, "instances"), q, opts)
}

// SearchStudyInstancesStream streams the matching instances dataset by
// dataset.
func (c *Client) SearchStudyInstancesStream(ctx context.Context, studyUID string, q Query, opts ...SearchOption) (*ResultsStream, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.searchStream(ctx, c.svc.ResourceURL("studies", studyUID, "instances"), q, opts)
}

// SearchSeriesInstances searches the instances of one series
// (/studies/{study}/series/{series}/instances).
func (c *Client) SearchSeriesInstances(ctx context.Context, studyUID, seriesUID string, q Query, opts ...SearchOption) (*Results, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID); err != nil {
		return nil, err
	}
	return c.search(ctx, c.svc.ResourceURL("studies", studyUID, "series", seriesUID, "instances"), q, opts)
}

// SearchSeriesInstancesStream streams the matching instances dataset by
// dataset.
func (c *Client) SearchSeriesInstancesStream(ctx context.Context, studyUID, seriesUID string, q Query, opts ...SearchOption) (*ResultsStream, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID); err != nil {
		return nil, err
	}
	return c.searchStream(ctx, c.svc.ResourceURL("studies", studyUID, "series", seriesUID, "instances"), q, opts)
}

// SearchAllSeries searches series across all studies (/series — the
// relational form; optional per the standard, servers may answer 4xx).
func (c *Client) SearchAllSeries(ctx context.Context, q Query, opts ...SearchOption) (*Results, error) {
	return c.search(ctx, c.svc.ResourceURL("series"), q, opts)
}

// SearchAllSeriesStream streams the matching series dataset by dataset.
func (c *Client) SearchAllSeriesStream(ctx context.Context, q Query, opts ...SearchOption) (*ResultsStream, error) {
	return c.searchStream(ctx, c.svc.ResourceURL("series"), q, opts)
}

// SearchAllInstances searches instances across all studies (/instances —
// the relational form; optional per the standard, servers may answer 4xx).
func (c *Client) SearchAllInstances(ctx context.Context, q Query, opts ...SearchOption) (*Results, error) {
	return c.search(ctx, c.svc.ResourceURL("instances"), q, opts)
}

// SearchAllInstancesStream streams the matching instances dataset by
// dataset.
func (c *Client) SearchAllInstancesStream(ctx context.Context, q Query, opts ...SearchOption) (*ResultsStream, error) {
	return c.searchStream(ctx, c.svc.ResourceURL("instances"), q, opts)
}

// search performs a search and aggregates the reply.
func (c *Client) search(ctx context.Context, u *url.URL, q Query, opts []SearchOption) (*Results, error) {
	rs, err := c.searchStream(ctx, u, q, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	datasets, err := rs.collect()
	if err != nil {
		return nil, err
	}
	return &Results{Datasets: datasets, Header: rs.Header()}, nil
}

// searchStream performs a search and returns the streaming cursor over the
// reply. 204 (No Content — no matches) yields an exhausted stream, not an
// error (PS3.18 §10.6.3).
func (c *Client) searchStream(ctx context.Context, u *url.URL, q Query, opts []SearchOption) (*ResultsStream, error) {
	cfg := buildSearchCfg(opts)
	if err := q.validate(c.svc.Core().CheckUID); err != nil {
		return nil, err
	}
	u.RawQuery = q.encodeQuery()
	resp, err := c.svc.Do(ctx, http.MethodGet, u, func(req *http.Request) {
		accept := cfg.accept
		if accept == "" {
			accept = MediaTypeDICOMJSON
		}
		req.Header.Set("Accept", accept)
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return &ResultsStream{header: resp.Header.Clone(), resp: resp, done: true}, nil
	}
	if err := guardXML(resp.Header.Get("Content-Type")); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	parts, err := multipartx.New(resp)
	if err != nil {
		return nil, fmt.Errorf("qido: search response: %w", err)
	}
	// The final (post-redirect) request URL is the PS3.18 base for relative
	// BulkDataURI resolution.
	return &ResultsStream{parts: parts, base: resp.Request.URL, header: resp.Header.Clone()}, nil
}

// encodeQuery renders the RawQuery value: ordered standard parameters, then
// Extra overrides appended (same-name standard parameters are dropped).
// Values are percent-encoded per PS3.18 §8.3.4 ("#, [, ], &, = and all
// non-ASCII shall be percent encoded") with the list comma left bare, the
// form the standard's own examples use (includefield=00081048,00081049 and
// UID List matching).
func (q Query) encodeQuery() string {
	ps := q.params()
	if len(q.Extra) > 0 {
		override := make(map[string]bool, len(q.Extra))
		for k := range q.Extra {
			override[k] = true
		}
		kept := ps[:0]
		for _, p := range ps {
			if !override[p.key] {
				kept = append(kept, p)
			}
		}
		ps = kept
		for _, k := range slices.Sorted(maps.Keys(q.Extra)) {
			for _, v := range q.Extra[k] {
				ps = append(ps, param{k, v})
			}
		}
	}
	var b strings.Builder
	for i, p := range ps {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p.key))
		b.WriteByte('=')
		b.WriteString(queryEscape(p.val))
	}
	return b.String()
}

// queryEscape percent-encodes a parameter value; the list comma stays bare
// (see encodeQuery).
func queryEscape(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "%2C", ",")
}
