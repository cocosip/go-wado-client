// Package wado is the shared foundation of go-wado-client: the transport
// core (connection pool, auth editors, retry, logging), URL parsing and
// reference resolution, UID whitelist validation, and the common error
// model used by the wadors / wadouri / multi packages.
package wado

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// DefaultUserAgent is the default User-Agent.
const DefaultUserAgent = "go-wado-client/0.1"

// Core carries the transport assembly shared by the wadors / wadouri
// clients: HTTP client (connection pool), auth/request editors, retry and
// logging. It is immutable and safe for concurrent use; cores derived via
// Fork share the same connection pool by default.
type Core struct {
	hc           *http.Client
	editors      []func(*http.Request) error
	retry        RetryPolicy
	logger       *slog.Logger
	ua           string
	lenientUID   bool
	modernParams bool
}

// NewCore builds a shared core from the given options.
func NewCore(opts ...Option) *Core {
	s := &settings{}
	applySettings(s, opts...)
	return coreFromSettings(s)
}

func applySettings(s *settings, opts ...Option) {
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
}

func coreFromSettings(s *settings) *Core {
	c := &Core{
		hc:           s.httpClientOrDefault(),
		editors:      s.editors,
		logger:       DiscardLogger,
		ua:           s.userAgent,
		lenientUID:   s.lenientUID,
		modernParams: s.modernParams,
	}
	if c.ua == "" {
		c.ua = DefaultUserAgent
	}
	if s.retry != nil {
		c.retry = *s.retry
	}
	if s.logger != nil {
		c.logger = s.logger
	}
	return c
}

// Fork clones the core and applies extra options on top; anything that does
// not touch the transport keeps sharing the underlying connection pool.
func (c *Core) Fork(opts ...Option) *Core {
	s := &settings{
		httpClient:   c.hc,
		editors:      append([]func(*http.Request) error(nil), c.editors...),
		logger:       c.logger,
		userAgent:    c.ua,
		lenientUID:   c.lenientUID,
		modernParams: c.modernParams,
	}
	if c.retry.MaxAttempts > 1 {
		cp := c.retry
		s.retry = &cp
	}
	applySettings(s, opts...)
	return coreFromSettings(s)
}

func (s *settings) httpClientOrDefault() *http.Client {
	if s.httpClient != nil {
		switch {
		case s.tlsCfg == nil:
			return s.httpClient
		case s.httpClient.Transport == nil:
			t := defaultTransport()
			t.TLSClientConfig = s.tlsCfg
			c2 := *s.httpClient
			c2.Transport = t
			return &c2
		default:
			if t, ok := s.httpClient.Transport.(*http.Transport); ok {
				tt := t.Clone()
				tt.TLSClientConfig = s.tlsCfg
				c2 := *s.httpClient
				c2.Transport = tt
				return &c2
			}
			return s.httpClient
		}
	}
	t := defaultTransport()
	if s.tlsCfg != nil {
		t.TLSClientConfig = s.tlsCfg
	}
	return &http.Client{Transport: t}
}

// defaultTransport follows design decision D4: fine-grained timeouts and no
// overall client Timeout — the total duration of a large streaming Study
// download is controlled by the caller via context.
func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	t.IdleConnTimeout = 90 * time.Second
	t.MaxIdleConnsPerHost = 8
	return t
}

// Do sends the request: editors (so tokens refresh) and the User-Agent are
// applied before every attempt, and network errors plus 429/502/503/504 are
// retried with backoff according to the configured policy.
//
// Requests carrying a body must make it replayable: every attempt shares the
// original req.Body reader as-is. All requests issued by this library are
// bodyless GETs.
func (c *Core) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	p := c.retry.normalized()
	for attempt := 1; ; attempt++ {
		r := req.Clone(ctx)
		for _, e := range c.editors {
			if err := e(r); err != nil {
				return nil, fmt.Errorf("wado: request editor: %w", err)
			}
		}
		if r.Header.Get("User-Agent") == "" {
			r.Header.Set("User-Agent", c.ua)
		}
		start := time.Now()
		resp, err := c.hc.Do(r)
		if err != nil {
			if !retryableErr(err) || attempt >= p.MaxAttempts {
				c.logger.Error("wado: request failed",
					"method", req.Method, "url", req.URL.Redacted(),
					"attempt", attempt, "err", err)
				return nil, err
			}
			c.logger.Debug("wado: retrying after transport error",
				"method", req.Method, "url", req.URL.Redacted(),
				"attempt", attempt, "err", err)
		} else {
			c.logger.Debug("wado: request done",
				"method", req.Method, "url", req.URL.Redacted(),
				"status", resp.Status, "duration", time.Since(start).Round(time.Millisecond))
			if !retryableStatus(resp.StatusCode) || attempt >= p.MaxAttempts {
				return resp, nil
			}
			retryAfter := parseRetryAfter(resp.Header)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
			_ = resp.Body.Close()
			if werr := sleepBackoff(ctx, p, attempt, retryAfter); werr != nil {
				return nil, werr
			}
			continue
		}
		if werr := sleepBackoff(ctx, p, attempt, 0); werr != nil {
			return nil, werr
		}
	}
}

func retryableErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepBackoff(ctx context.Context, p RetryPolicy, attempt int, retryAfter time.Duration) error {
	bo := p.InitialBackoff << (attempt - 1)
	if bo <= 0 || bo > p.MaxBackoff {
		bo = p.MaxBackoff
	}
	// Retry-After is honored but capped at MaxBackoff: a hostile or buggy
	// gateway must not be able to stall the client past the configured
	// ceiling.
	if retryAfter > p.MaxBackoff {
		retryAfter = p.MaxBackoff
	}
	if retryAfter > bo {
		bo = retryAfter
	}
	if p.Jitter > 0 {
		bo += time.Duration(rand.Int64N(int64(p.Jitter)))
	}
	if bo <= 0 {
		return nil
	}
	t := time.NewTimer(bo)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CheckUID validates a UID against the whitelist; WithLenientUID skips it.
func (c *Core) CheckUID(field, uid string) error {
	if c.lenientUID {
		return nil
	}
	if err := ValidateUID(uid); err != nil {
		return &UIDError{Field: field, UID: uid, Reason: err.Error()}
	}
	return nil
}

// ModernParams reports whether 2023+ parameter names are in use.
func (c *Core) ModernParams() bool { return c.modernParams }

// LenientUID reports whether UID validation is relaxed.
func (c *Core) LenientUID() bool { return c.lenientUID }
