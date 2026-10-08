package wadors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cocosip/go-wado-client"
)

// FetchBulkData fetches Bulk Data (one bulk data element as an octet
// stream).
//
// The default Accept is `multipart/related; type="application/octet-stream"`
// — the media type origin servers must support for Bulk Data resources
// (PS3.18 §10.4.4); the single-part `application/octet-stream` form is only
// optional server-side and can be requested via WithAccept. Both reply forms
// are consumed uniformly: the returned stream delivers exactly the one
// element the URI identifies.
//
// uri may be a BulkDataURI returned by the metadata methods: an absolute
// URL, an absolute path ("/dicomweb/...") or a relative path resolved
// against the client base URL. Note that per PS3.18 the relative form is
// resolved against the metadata request URL; the datasets returned by
// StudyMetadata and friends already carry absolute URLs, so passing them
// through directly is the intended usage.
func (c *Client) FetchBulkData(ctx context.Context, uri string, opts ...RetrieveOption) (io.ReadCloser, error) {
	u, err := wado.ResolveReference(c.svc.URL(), uri)
	if err != nil {
		return nil, err
	}
	cfg := buildRetrieveCfg(opts)
	accept := cfg.acceptOverride
	if accept == "" {
		accept = `multipart/related; type="application/octet-stream"`
	}
	resp, err := c.do(ctx, u, func(req *http.Request) { req.Header.Set("Accept", accept) })
	if err != nil {
		return nil, err
	}
	// newMultipart tolerates the single-part reply form as one part, so both
	// server-side forms reduce to "the first (only) part".
	mp, err := newMultipart(resp)
	if err != nil {
		return nil, err
	}
	p, err := mp.Next()
	if err != nil {
		_ = mp.Close()
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("wadors: bulkdata: %w", err)
	}
	return partReadCloser{Reader: p, closer: mp.Close}, nil
}

type partReadCloser struct {
	io.Reader
	closer func() error
}

func (rc partReadCloser) Close() error { return rc.closer() }
