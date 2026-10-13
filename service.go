package wado

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Service couples a Core to one service base URL: the middle layer between
// the transport core and the per-transaction clients (wadors / qido /
// wadouri). It owns the URL policy (base URL plus standard resource path
// segments) and the status-code check shared by every transaction; the
// clients hold one Service and stay free of transport concerns.
//
// A Service is immutable and safe for concurrent use; Fork derives a copy
// with a different base URL, sharing the connection pool unless the options
// replace the transport.
type Service struct {
	core *Core
	base *url.URL
}

// NewService binds a base URL to an existing core (assembly scenarios such
// as the multi registry; multiple services share one connection pool).
func NewService(core *Core, baseURL string) (*Service, error) {
	if core == nil {
		return nil, errors.New("wado: nil core")
	}
	u, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Service{core: core, base: u}, nil
}

// Fork derives a new Service that shares the full assembly and only replaces
// the base URL (no network activity).
func (s *Service) Fork(baseURL string, opts ...Option) (*Service, error) {
	u, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	core, err := s.core.Fork(opts...)
	if err != nil {
		return nil, err
	}
	return &Service{core: core, base: u}, nil
}

// BaseURL returns the current base URL.
func (s *Service) BaseURL() string { return s.base.String() }

// URL returns the parsed base URL (used to resolve references such as
// BulkDataURI against the service address).
func (s *Service) URL() *url.URL { return s.base }

// ResourceURL appends the standard resource path segments to the base URL.
func (s *Service) ResourceURL(elems ...string) *url.URL {
	return s.base.JoinPath(elems...)
}

// Core returns the underlying transport core (settings such as
// LegacyParams / Logger and single-value CheckUID).
func (s *Service) Core() *Core { return s.core }

// Do sends the request with the given method and checks the status; non-2xx
// responses are converted to *wado.StatusError (the response body is drained
// and closed). Requests built here are bodyless.
func (s *Service) Do(ctx context.Context, method string, u *url.URL, setHeader func(*http.Request)) (*http.Response, error) {
	return s.do(ctx, method, u, setHeader, true)
}

// DoWithoutCredentials is Do with the auth/request editors skipped; see
// Core.DoWithoutCredentials.
func (s *Service) DoWithoutCredentials(ctx context.Context, method string, u *url.URL, setHeader func(*http.Request)) (*http.Response, error) {
	return s.do(ctx, method, u, setHeader, false)
}

func (s *Service) do(ctx context.Context, method string, u *url.URL, setHeader func(*http.Request), withEditors bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("wado: build request: %w", err)
	}
	if setHeader != nil {
		setHeader(req)
	}
	var resp *http.Response
	if withEditors {
		resp, err = s.core.Do(ctx, req)
	} else {
		resp, err = s.core.DoWithoutCredentials(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	// 206 Partial Content means the server honored a Range request. This
	// library never sends one, so a 206 signals truncation by a proxy or
	// gateway rather than a complete resource; accepting it as a full
	// response would silently deliver partial data.
	if resp.StatusCode == http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("wado: %s %s: unexpected 206 Partial Content (no Range was requested), the response is not the complete resource",
			method, u.Redacted())
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, NewStatusError(req, resp)
	}
	return resp, nil
}

// SameHost reports whether target addresses the same host (and port) as the
// service base URL. It backs the Bulk Data host policy: a server-supplied URI
// to another host must not receive the gateway's credentials by default.
func (s *Service) SameHost(target *url.URL) bool {
	if target == nil || s.base == nil {
		return false
	}
	return strings.EqualFold(target.Host, s.base.Host) &&
		strings.EqualFold(target.Scheme, s.base.Scheme)
}

// CheckUIDs validates (field, uid) pairs against the whitelist.
func (s *Service) CheckUIDs(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if err := s.core.CheckUID(pairs[i], pairs[i+1]); err != nil {
			return err
		}
	}
	return nil
}
