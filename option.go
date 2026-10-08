package wado

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Option is a shared configuration item, common to wadors / wadouri / multi.
type Option func(*settings)

type settings struct {
	httpClient     *http.Client
	tlsCfg         *tls.Config
	editors        []func(*http.Request) error
	retry          *RetryPolicy
	logger         *slog.Logger
	userAgent      string
	lenientUID     bool
	legacyParams   bool
	maxIdlePerHost int
}

// WithHTTPClient takes over the HTTP client entirely (proxying etc.).
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithTLSClientConfig sets the TLS configuration (common for hospital
// self-signed CAs).
//
// It composes with WithHTTPClient only when the client's Transport is nil or
// an *http.Transport (both are cloned, never mutated); otherwise the
// combination is rejected at construction with an error, because a wrapper
// RoundTripper offers no way to apply the settings.
func WithTLSClientConfig(cfg *tls.Config) Option {
	return func(s *settings) { s.tlsCfg = cfg }
}

// WithBasicAuth attaches HTTP Basic authentication.
func WithBasicAuth(user, pass string) Option {
	return WithRequestEditor(func(r *http.Request) error {
		r.SetBasicAuth(user, pass)
		return nil
	})
}

// TokenSource supplies a bearer token; it is called before every attempt so
// implementations can cache or refresh on their own.
type TokenSource interface {
	Token() (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func() (string, error)

// Token calls f.
func (f TokenSourceFunc) Token() (string, error) { return f() }

// StaticToken returns a TokenSource that always returns tok.
func StaticToken(tok string) TokenSource {
	return TokenSourceFunc(func() (string, error) { return tok, nil })
}

// WithBearerTokenSource attaches Bearer authentication.
func WithBearerTokenSource(ts TokenSource) Option {
	return WithRequestEditor(func(r *http.Request) error {
		tok, err := ts.Token()
		if err != nil {
			return fmt.Errorf("get bearer token: %w", err)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	})
}

// WithRequestEditor mutates the request right before it is sent (private
// signature headers etc.); editors are chainable.
func WithRequestEditor(fn func(*http.Request) error) Option {
	return func(s *settings) { s.editors = append(s.editors, fn) }
}

// WithRetry enables transport-level retry (off by default). Only network
// errors and 429/502/503/504 are retried. Retry-After is honored but capped
// at MaxBackoff; retries respect context cancellation.
func WithRetry(p RetryPolicy) Option {
	return func(s *settings) { cp := p; s.retry = &cp }
}

// WithUserAgent overrides the default User-Agent (go-wado-client/0.1).
func WithUserAgent(ua string) Option { return func(s *settings) { s.userAgent = ua } }

// WithLogger injects an slog logger; when unset all logging is discarded
// (slog.Default() is never used implicitly).
func WithLogger(l *slog.Logger) Option { return func(s *settings) { s.logger = l } }

// WithLogHandler injects an slog Handler (equivalent to
// WithLogger(slog.New(h))).
func WithLogHandler(h slog.Handler) Option { return WithLogger(slog.New(h)) }

// WithLenientUID disables local UID whitelist validation (escape hatch for
// private gateways that accept non-conformant UIDs).
//
// Caution: values that would have been rejected now reach the request path
// verbatim; a hostile caller could craft a UID containing "../" or "?" that
// escapes the intended resource prefix. Only use with trusted callers.
func WithLenientUID() Option { return func(s *settings) { s.lenientUID = true } }

// WithLegacyParamNames switches rendered/anonymize parameters to the retired
// WADO-WS-era dialect (annotations/windowcenter/windowwidth/icccolorspace/
// anonymity) for gateways that only speak it. Those names never appeared in
// any published WADO-RS edition, so this is strictly a private-gateway
// compatibility mode; the default is the PS3.18 parameter set (annotation /
// window / iccprofile — the rendered names since the transaction was
// introduced in 2016 — and anonymize, which replaced anonymity in 2019).
func WithLegacyParamNames() Option { return func(s *settings) { s.legacyParams = true } }

// WithMaxIdleConnsPerHost overrides the per-host idle-connection pool of the
// transports the library creates (default 8). Raise it when fanning out many
// concurrent HTTP/1.1 retrievals against one gateway, or connections beyond
// the pool are discarded after use and pay a TLS handshake every time;
// irrelevant for HTTP/2 (one connection multiplexes all requests). No effect
// when WithHTTPClient supplies a client with its own transport.
func WithMaxIdleConnsPerHost(n int) Option { return func(s *settings) { s.maxIdlePerHost = n } }

// RetryPolicy describes the retry behavior for idempotent GETs.
type RetryPolicy struct {
	MaxAttempts    int           // total attempts including the first; <=1 disables retry
	InitialBackoff time.Duration // backoff before the first retry; 0 selects the 100ms default
	MaxBackoff     time.Duration // upper bound; values below InitialBackoff are clamped. Retry-After is also capped here
	// Jitter is an upper bound of extra random delay added to each retry
	// sleep, spreading simultaneous client retries after a shared failure
	// (thundering herd); 0 disables it.
	Jitter time.Duration
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.InitialBackoff <= 0 {
		p.InitialBackoff = 100 * time.Millisecond
	}
	if p.MaxBackoff < p.InitialBackoff {
		p.MaxBackoff = p.InitialBackoff
	}
	return p
}
