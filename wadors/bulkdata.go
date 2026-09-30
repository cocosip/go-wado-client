package wadors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// FetchBulkData fetches Bulk Data (application/octet-stream, a single-part
// stream).
//
// uri may be a BulkDataURI returned by the metadata methods: an absolute
// URL, an absolute path ("/dicomweb/...") or a relative path resolved
// against the client base URL. Note that per PS3.18 the relative form is
// resolved against the metadata request URL; the datasets returned by
// StudyMetadata and friends already carry absolute URLs, so passing them
// through directly is the intended usage.
func (c *Client) FetchBulkData(ctx context.Context, uri string, opts ...RetrieveOption) (io.ReadCloser, error) {
	u, err := wado.ResolveReference(c.base, uri)
	if err != nil {
		return nil, err
	}
	cfg := buildRetrieveCfg(opts)
	accept := cfg.acceptOverride
	if accept == "" {
		accept = "application/octet-stream"
	}
	resp, err := c.do(ctx, u, func(req *http.Request) { req.Header.Set("Accept", accept) })
	if err != nil {
		return nil, err
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		mp, err := newMultipart(resp)
		if err != nil {
			return nil, err
		}
		p, err := mp.Next()
		if err != nil {
			_ = mp.Close()
			return nil, fmt.Errorf("wadors: bulkdata multipart: %w", err)
		}
		return partReadCloser{Reader: p, closer: resp.Body.Close}, nil
	}
	return resp.Body, nil
}

type partReadCloser struct {
	io.Reader
	closer func() error
}

func (rc partReadCloser) Close() error { return rc.closer() }
