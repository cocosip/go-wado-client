// Package wadouri implements the WADO-URI client (the URI Service,
// PS3.18 Chapter 9).
//
// WADO-URI is a single GET: the standard defines no path at all; the
// resource is identified entirely by query parameters (requestType=WADO +
// studyUID/seriesUID/objectUID) and the response is always single-part (a
// DICOM file or a rendered image). The endpoint URL (for example
// .../api/wado/H0001/RIS/wado-uri) is deployment-specific and passed in
// whole at construction.
package wadouri

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// Client is a WADO-URI client bound to a single endpoint (immutable, safe
// for concurrent use).
type Client struct {
	svc *wado.Service
}

// New creates a client; endpoint is the full WADO-URI service URL. Any query
// or fragment component of the endpoint is discarded during normalization —
// fixed private parameters belong in Request.Extra instead.
func New(endpoint string, opts ...wado.Option) (*Client, error) {
	if _, err := wado.ParseBaseURL(endpoint); err != nil {
		return nil, err
	}
	core, err := wado.NewCore(opts...)
	if err != nil {
		return nil, err
	}
	svc, err := wado.NewService(core, endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// NewWithCore creates a client on top of an existing shared core (assembly
// scenarios such as the multi registry).
func NewWithCore(core *wado.Core, endpoint string) (*Client, error) {
	if core == nil {
		return nil, errors.New("wadouri: nil core")
	}
	svc, err := wado.NewService(core, endpoint)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// Fork derives a new client that shares the full assembly and only replaces
// the endpoint (no network activity).
func (c *Client) Fork(endpoint string, opts ...wado.Option) (*Client, error) {
	svc, err := c.svc.Fork(endpoint, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// Endpoint returns the current endpoint URL.
func (c *Client) Endpoint() string { return c.svc.BaseURL() }

// Response is a WADO-URI response: always a single-part stream (a DICOM
// file or a rendered image).
type Response struct {
	ContentType string
	Header      http.Header
	Body        io.ReadCloser
	resp        *http.Response
}

// IsDICOM reports whether the response is a DICOM file
// (application/dicom).
func (r *Response) IsDICOM() bool {
	ct := strings.ToLower(strings.TrimSpace(r.ContentType))
	return strings.HasPrefix(ct, "application/dicom")
}

// Close closes the underlying response. It is safe on the zero value.
func (r *Response) Close() error {
	if r.resp != nil {
		return r.resp.Body.Close()
	}
	if rc, ok := r.Body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// Retrieve performs the retrieval: local validation first (cases where the
// standard mandates a server-side 400 are rejected up front), then the GET.
// An empty req.ContentType selects application/dicom (the Retrieve DICOM
// Instance transaction); a rendered media type such as image/jpeg selects
// the Retrieve Rendered Instance transaction.
func (c *Client) Retrieve(ctx context.Context, req Request) (*Response, error) {
	if err := req.validate(c.svc.Core().CheckUID); err != nil {
		return nil, err
	}
	u := *c.svc.URL()
	u.RawQuery = req.query(c.svc.Core().LegacyParams()).Encode()
	accept := strings.TrimSpace(req.ContentType)
	if accept == "" {
		accept = "application/dicom"
	}
	resp, err := c.svc.Do(ctx, http.MethodGet, &u, func(hreq *http.Request) {
		hreq.Header.Set("Accept", accept)
	})
	if err != nil {
		return nil, err
	}
	return &Response{
		ContentType: resp.Header.Get("Content-Type"),
		Header:      resp.Header.Clone(),
		Body:        resp.Body,
		resp:        resp,
	}, nil
}
