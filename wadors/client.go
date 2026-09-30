// Package wadors implements the WADO-RS client (the Retrieve transaction of
// the Studies Service, PS3.18): Study/Series/Instance retrieval (streaming
// multipart/related), metadata (dicom+json, parsed by go-dicom's
// serialization), frame pixel data, rendered images and Bulk Data.
//
// The BaseURL is bound at construction (immutable, safe for concurrent use):
// it is everything before the standard resource path (studies/...) and its
// internal structure is opaque to the library. See Fork for multi-target
// reuse.
package wadors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cocosip/go-wado-client"
)

// Client is a WADO-RS client bound to a single service base URL.
type Client struct {
	core *wado.Core
	base *url.URL
}

// New creates a client. Example baseURL:
// "https://gw.example.com/api/wado/H0001/RIS".
func New(baseURL string, opts ...wado.Option) (*Client, error) {
	u, err := wado.ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{core: wado.NewCore(opts...), base: u}, nil
}

// NewWithCore creates a client on top of an existing shared core (assembly
// scenarios such as the multi registry; multiple clients share one
// connection pool).
func NewWithCore(core *wado.Core, baseURL string) (*Client, error) {
	if core == nil {
		return nil, errors.New("wadors: nil core")
	}
	u, err := wado.ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{core: core, base: u}, nil
}

// Fork derives a new client that shares the full assembly and only replaces
// the BaseURL (no network activity).
func (c *Client) Fork(baseURL string, opts ...wado.Option) (*Client, error) {
	u, err := wado.ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{core: c.core.Fork(opts...), base: u}, nil
}

// BaseURL returns the current base URL.
func (c *Client) BaseURL() string { return c.base.String() }

// resourceURL appends the standard resource path segments to the base URL.
func (c *Client) resourceURL(elems ...string) *url.URL {
	return c.base.JoinPath(elems...)
}

// do sends a GET and checks the status; non-2xx responses are converted to
// *wado.StatusError (the response body is drained and closed).
func (c *Client) do(ctx context.Context, u *url.URL, setHeader func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("wadors: build request: %w", err)
	}
	if setHeader != nil {
		setHeader(req)
	}
	resp, err := c.core.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, wado.NewStatusError(req, resp)
	}
	return resp, nil
}

// checkUIDs validates (field, uid) pairs.
func (c *Client) checkUIDs(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := c.core.CheckUID(pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}
