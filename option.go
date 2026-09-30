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
	httpClient   *http.Client
	tlsCfg       *tls.Config
	editors      []func(*http.Request) error
	retry        *RetryPolicy
	logger       *slog.Logger
	userAgent    string
	lenientUID   bool
	modernParams bool
}

// WithHTTPClient takes over the HTTP client entirely (proxying etc.).
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.httpClient = c }
}

// WithTLSClientConfig sets the TLS configuration (common for hospital
// self-signed CAs).
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
// errors and 429/502/503/504 are retried, honoring Retry-After and context
// cancellation.
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
func WithLenientUID() Option { return func(s *settings) { s.lenientUID = true } }

// WithModernParamNames switches rendered/anonymize parameters to the 2023+
// standard names (annotation/window/iccprofile/anonymize). The default is the
// classic naming deployed by the vast majority of servers
// (annotations/windowcenter/windowwidth/icccolorspace/anonymity).
func WithModernParamNames() Option { return func(s *settings) { s.modernParams = true } }

// RetryPolicy describes the retry behavior for idempotent GETs.
type RetryPolicy struct {
	MaxAttempts    int           // total attempts including the first; <=1 disables retry
	InitialBackoff time.Duration // backoff before the first retry; 0 selects the 100ms default
	MaxBackoff     time.Duration // upper bound; values below InitialBackoff are clamped
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
