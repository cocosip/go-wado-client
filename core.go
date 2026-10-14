// Package wado is the shared foundation of go-wado-client: the transport
// core (connection pool, auth editors, retry, logging), URL parsing and
// reference resolution, UID whitelist validation, and the common error
// model used by the wadors / wadouri / multi packages.
package wado

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultUserAgent is the default User-Agent.
const DefaultUserAgent = "go-wado-client/0.1"

// Header names this library sets itself on every request (everything else on
// the wire comes from the caller's auth/request editors) — the allowlist the
// redirect guard keeps when a redirect crosses to another host.
const (
	headerUserAgent = "User-Agent"
	headerAccept    = "Accept"
)

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
	legacyParams bool
	bulkDataHost BulkDataHostPolicy
	// ownTransport records that hc was built by this library (rather than
	// supplied via WithHTTPClient), so WithMaxIdleConnsPerHost may still be
	// applied to it. A caller-supplied client is never touched, matching that
	// option's documented scope.
	ownTransport bool
}

// NewCore builds a shared core from the given options. It fails when the
// options are contradictory — see WithTLSClientConfig for the one case that
// can trigger this.
func NewCore(opts ...Option) (*Core, error) {
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

func coreFromSettings(s *settings) (*Core, error) {
	hc, err := s.httpClientOrDefault()
	if err != nil {
		return nil, err
	}
	c := &Core{
		hc:           hc,
		editors:      s.editors,
		logger:       DiscardLogger,
		ua:           s.userAgent,
		lenientUID:   s.lenientUID,
		legacyParams: s.legacyParams,
		bulkDataHost: s.bulkDataHost,
		ownTransport: s.httpClient == nil || s.httpClientOwned,
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
	return c, nil
}

// Fork clones the core and applies extra options on top; anything that does
// not touch the transport keeps sharing the underlying connection pool.
//
// The idle-connection pool is carried forward explicitly: it lives in the
// transport rather than in a Core field, so without this a Fork would silently
// fall back to the default pool — and multi.Registry builds every per-target
// client through Fork.
func (c *Core) Fork(opts ...Option) (*Core, error) {
	s := &settings{
		httpClient:      c.hc,
		httpClientOwned: c.ownTransport,
		editors:         append([]func(*http.Request) error(nil), c.editors...),
		logger:          c.logger,
		userAgent:       c.ua,
		lenientUID:      c.lenientUID,
		legacyParams:    c.legacyParams,
		bulkDataHost:    c.bulkDataHost,
		maxIdlePerHost:  c.maxIdlePerHost(),
	}
	if c.retry.MaxAttempts > 1 {
		cp := c.retry
		s.retry = &cp
	}
	applySettings(s, opts...)
	return coreFromSettings(s)
}

// maxIdlePerHost reports the per-host idle pool of this core's transport (0
// when it is not an *http.Transport, in which case the setting never applied
// either).
func (c *Core) maxIdlePerHost() int {
	if t, ok := c.hc.Transport.(*http.Transport); ok {
		return t.MaxIdleConnsPerHost
	}
	return 0
}

// httpClientOrDefault resolves the client for a settings set.
//
// A caller-supplied client is handed back untouched unless a TLS config must
// be layered on (which requires cloning, never mutating, its transport). An
// owned client is cloned only when the idle-connection pool actually changes,
// so a plain Fork keeps sharing the parent's *http.Client — and therefore its
// connection pool, as Fork documents.
func (s *settings) httpClientOrDefault() (*http.Client, error) {
	if s.httpClient == nil {
		t := defaultTransport()
		applyIdleConnsPerHost(t, s.maxIdlePerHost)
		if s.tlsCfg != nil {
			t.TLSClientConfig = s.tlsCfg
		}
		return &http.Client{Transport: t, CheckRedirect: redirectGuard}, nil
	}

	base, isTransport := s.httpClient.Transport.(*http.Transport)
	if s.httpClient.Transport != nil && !isTransport && s.tlsCfg != nil {
		// A wrapper RoundTripper hides the *http.Transport that carries TLS
		// settings — applying the config is impossible, and silently ignoring
		// it would surface as opaque TLS failures at request time (hospital
		// self-signed CAs are a core scenario).
		return nil, errors.New("wado: WithTLSClientConfig cannot be combined with an HTTP client whose Transport is not *http.Transport")
	}

	// The idle-pool setting only ever applies to a client this library built;
	// a caller-supplied transport is never mutated (see
	// WithMaxIdleConnsPerHost).
	adjustIdle := isTransport && s.httpClientOwned && s.maxIdlePerHost > 0 &&
		base.MaxIdleConnsPerHost != s.maxIdlePerHost

	switch {
	case s.tlsCfg == nil && !adjustIdle:
		// Nothing transport-level to change: share the client (and its
		// connection pool) as-is.
		return s.httpClient, nil
	case s.httpClient.Transport == nil:
		t := defaultTransport()
		applyIdleConnsPerHost(t, s.maxIdlePerHost)
		t.TLSClientConfig = s.tlsCfg
		c2 := *s.httpClient
		c2.Transport = t
		return &c2, nil
	default:
		// Clone rather than mutate: the parent core keeps its own transport.
		tt := base.Clone()
		if s.tlsCfg != nil {
			tt.TLSClientConfig = s.tlsCfg
		}
		if adjustIdle {
			tt.MaxIdleConnsPerHost = s.maxIdlePerHost
		}
		c2 := *s.httpClient
		c2.Transport = tt
		return &c2, nil
	}
}

// applyIdleConnsPerHost applies the WithMaxIdleConnsPerHost override to a
// library-built transport; non-positive values keep the default.
func applyIdleConnsPerHost(t *http.Transport, n int) {
	if n > 0 {
		t.MaxIdleConnsPerHost = n
	}
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

// redirectGuard is the CheckRedirect of library-built HTTP clients: redirects
// are followed, but a hop to another host carries none of the caller's
// headers.
//
// net/http already drops Authorization, WWW-Authenticate and Cookie when a
// redirect changes host; this extends the same guarantee to the auth/request
// editors, whose headers (private signature headers etc.) would otherwise
// follow a server-chosen redirect anywhere — the same trust boundary
// WithBulkDataHostPolicy draws for Bulk Data URIs. A compromised or buggy
// gateway must not be able to harvest them with a single 302.
//
// Every redirect hop copies its headers from the original request, so each
// hop is checked (and stripped) independently; a hop back to the original
// host receives them again, which is safe — that host got them on the first
// request already. Clients supplied via WithHTTPClient keep their own policy.
func redirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("wado: stopped after 10 redirects")
	}
	if len(via) == 0 || strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return nil
	}
	for k := range req.Header {
		if k != headerUserAgent && k != headerAccept {
			req.Header.Del(k)
		}
	}
	return nil
}

// Do sends the request: editors (so tokens refresh) and the User-Agent are
// applied before every attempt, and network errors plus 429/502/503/504 are
// retried with backoff according to the configured policy.
//
// Requests carrying a body are replayed via GetBody (populated automatically
// by http.NewRequest for in-memory readers); a body that cannot be replayed
// while retry is enabled is rejected up front instead of being silently
// truncated on the second attempt. All requests issued by this library are
// bodyless (GET/OPTIONS).
func (c *Core) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.do(ctx, req, true)
}

// DoWithoutCredentials is Do with the auth/request editors skipped: the
// request carries the User-Agent but none of the caller's credentials. It
// backs the cross-host Bulk Data policy (see WithBulkDataHostPolicy), where
// following a server-supplied URI to another host must not leak the gateway's
// credentials to it.
func (c *Core) DoWithoutCredentials(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.do(ctx, req, false)
}

func (c *Core) do(ctx context.Context, req *http.Request, withEditors bool) (*http.Response, error) {
	if ctx == nil {
		return nil, errors.New("wado: nil context")
	}
	p := c.retry.normalized()
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil && p.MaxAttempts > 1 {
		return nil, errors.New("wado: request carries a body that cannot be replayed (no GetBody) while retry is enabled")
	}
	for attempt := 1; ; attempt++ {
		r := req.Clone(ctx)
		if r.Body != nil && r.Body != http.NoBody && r.GetBody != nil {
			b, err := r.GetBody()
			if err != nil {
				return nil, fmt.Errorf("wado: replay request body: %w", err)
			}
			r.Body = b
		}
		if withEditors {
			for _, e := range c.editors {
				if err := e(r); err != nil {
					return nil, fmt.Errorf("wado: request editor: %w", err)
				}
			}
		}
		if r.Header.Get(headerUserAgent) == "" {
			r.Header.Set(headerUserAgent, c.ua)
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

// retryableErr classifies transport errors. Context cancellation never
// retries, and neither do permanent failures that no number of attempts can
// fix: TLS certificate verification (hospital self-signed CA not trusted)
// and an HTTP server answering a TLS request (scheme mismatch).
func retryableErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	return !errors.Is(err, http.ErrSchemeMismatch)
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// parseRetryAfter returns the delay requested by a Retry-After header, or 0
// when the header is absent or unparsable.
//
// The delay-seconds form is validated against the range a time.Duration can
// hold: time.Duration(secs) * time.Second overflows for secs above
// math.MaxInt64/1e9 and wraps negative, which would silently disable the
// header instead of honoring it (a hostile or buggy gateway could also use
// the overflow to skip the cap applied by sleepBackoff). Out-of-range values
// are clamped to the maximum representable duration.
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
		if int64(secs) > maxSeconds {
			return time.Duration(math.MaxInt64)
		}
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
	bo := backoffFor(p, attempt)
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

// backoffFor computes InitialBackoff<<(attempt-1) capped at MaxBackoff.
//
// The shift is bounded before it happens: a large attempt count would
// overflow int64 and wrap negative (or to a small positive value), which
// would shorten the sleep instead of lengthening it. Capping the shift count
// first keeps the result monotonic up to MaxBackoff.
func backoffFor(p RetryPolicy, attempt int) time.Duration {
	if p.MaxBackoff <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	bo := p.InitialBackoff
	if bo <= 0 {
		return p.MaxBackoff
	}
	for i := 1; i < attempt; i++ {
		if bo >= p.MaxBackoff {
			return p.MaxBackoff
		}
		bo <<= 1
	}
	if bo <= 0 || bo > p.MaxBackoff {
		return p.MaxBackoff
	}
	return bo
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

// LegacyParams reports whether the retired WADO-WS-era parameter names are in
// use (WithLegacyParamNames); the default is the current PS3.18 naming.
func (c *Core) LegacyParams() bool { return c.legacyParams }

// Logger returns the core's logger — DiscardLogger unless one was injected
// via WithLogger/WithLogHandler.
func (c *Core) Logger() *slog.Logger { return c.logger }

// LenientUID reports whether UID validation is relaxed.
func (c *Core) LenientUID() bool { return c.lenientUID }

// BulkDataHost returns the configured Bulk Data host policy.
func (c *Core) BulkDataHost() BulkDataHostPolicy { return c.bulkDataHost }
