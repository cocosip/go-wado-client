// Package qido implements the QIDO-RS client (the Search transaction of the
// Studies Service, PS3.18 §10.6): attribute-based search over studies,
// series and instances returning application/dicom+json datasets, with
// client-driven limit/offset paging (§8.3.4) and the Warning 299
// additional-results signal.
//
// The BaseURL is bound at construction (immutable, safe for concurrent use):
// it is everything before the standard resource path (studies/...) and its
// internal structure is opaque to the library. See Fork for multi-target
// reuse.
package qido

import (
	"errors"

	"github.com/cocosip/go-wado-client"
)

// Client is a QIDO-RS client bound to a single service base URL.
type Client struct {
	svc *wado.Service
}

// New creates a client. Example baseURL:
// "https://gw.example.com/api/wado/H0001/RIS".
func New(baseURL string, opts ...wado.Option) (*Client, error) {
	if _, err := wado.ParseBaseURL(baseURL); err != nil {
		return nil, err
	}
	core, err := wado.NewCore(opts...)
	if err != nil {
		return nil, err
	}
	svc, err := wado.NewService(core, baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// NewWithCore creates a client on top of an existing shared core (assembly
// scenarios such as the multi registry; multiple clients share one
// connection pool).
func NewWithCore(core *wado.Core, baseURL string) (*Client, error) {
	if core == nil {
		return nil, errors.New("qido: nil core")
	}
	svc, err := wado.NewService(core, baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// Fork derives a new client that shares the full assembly and only replaces
// the BaseURL (no network activity).
func (c *Client) Fork(baseURL string, opts ...wado.Option) (*Client, error) {
	svc, err := c.svc.Fork(baseURL, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// BaseURL returns the current base URL.
func (c *Client) BaseURL() string { return c.svc.BaseURL() }

// checkUIDs validates (field, uid) pairs.
func (c *Client) checkUIDs(pairs ...string) error {
	return c.svc.CheckUIDs(pairs...)
}
