package wado

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	// ErrInvalidUID is the sentinel root cause of UIDError.
	ErrInvalidUID = errors.New("wado: invalid UID")
	// ErrInvalidRequest is the sentinel root cause of RequestError.
	ErrInvalidRequest = errors.New("wado: invalid request")
)

// StatusError represents a non-2xx response from the server.
type StatusError struct {
	StatusCode int
	Status     string
	Method     string
	URL        string
	Header     http.Header
	Body       []byte // truncated to 4KB for gateway diagnostics
}

func (e *StatusError) Error() string {
	if len(e.Body) == 0 {
		return fmt.Sprintf("wado: %s %s: %s", e.Method, e.URL, e.Status)
	}
	return fmt.Sprintf("wado: %s %s: %s: %s", e.Method, e.URL, e.Status, truncateUTF8(string(e.Body), 200))
}

// IsNotFound reports HTTP 404 Not Found.
func (e *StatusError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsNotAcceptable reports HTTP 406 Not Acceptable.
func (e *StatusError) IsNotAcceptable() bool { return e.StatusCode == http.StatusNotAcceptable }

// IsGone reports HTTP 410 Gone.
func (e *StatusError) IsGone() bool { return e.StatusCode == http.StatusGone }

// IsTooLarge reports HTTP 413 Request Entity Too Large.
func (e *StatusError) IsTooLarge() bool { return e.StatusCode == http.StatusRequestEntityTooLarge }

// IsUnauthorized reports HTTP 401 Unauthorized.
func (e *StatusError) IsUnauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// IsForbidden reports HTTP 403 Forbidden.
func (e *StatusError) IsForbidden() bool { return e.StatusCode == http.StatusForbidden }

// IsRetryable reports whether the status code qualifies for transport-level
// retry (network errors excluded; they are not StatusError values).
func (e *StatusError) IsRetryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// NewStatusError drains the (truncated) error response body, closes resp and
// returns a StatusError. Set-Cookie is dropped from the retained headers:
// error values end up in logs and bug reports, where session cookies are
// noise at best and a credential leak at worst.
func NewStatusError(req *http.Request, resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	if len(body) > 4096 {
		body = body[:4096]
	}
	header := resp.Header.Clone()
	header.Del("Set-Cookie")
	return &StatusError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Method:     req.Method,
		URL:        req.URL.String(),
		Header:     header,
		Body:       body,
	}
}

// UIDError reports a UID that failed the local whitelist validation.
type UIDError struct {
	Field  string
	UID    string
	Reason string
}

func (e *UIDError) Error() string {
	uid := e.UID
	if len(uid) > 32 {
		uid = truncateUTF8(uid, 32) + "..."
	}
	return fmt.Sprintf("wado: invalid UID for %s (%s): %q", e.Field, e.Reason, uid)
}

func (e *UIDError) Unwrap() error { return ErrInvalidUID }

// truncateUTF8 shortens s to at most limit bytes without splitting a
// multi-byte rune; invalid sequences (binary error bodies) become U+FFFD
// instead of corrupting the surrounding text.
func truncateUTF8(s string, limit int) string {
	if len(s) > limit {
		s = s[:limit]
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// RequestError reports a request that failed local validation (cases where
// the standard mandates a 400 from the server are rejected up front).
type RequestError struct {
	Field  string
	Reason string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("wado: invalid request: %s: %s", e.Field, e.Reason)
}

func (e *RequestError) Unwrap() error { return ErrInvalidRequest }
