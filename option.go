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
	bulkDataHost   BulkDataHostPolicy
	// httpClientOwned marks httpClient as built by this library (a Forked
	// core) rather than handed in by the caller. Only an owned client may have
	// its idle-connection pool adjusted; a caller-supplied transport is never
	// mutated, which is what WithMaxIdleConnsPerHost documents.
	httpClientOwned bool
}

// BulkDataHostPolicy controls which hosts a Bulk Data URI returned by the
// server may address, and whether the client's credentials travel there.
//
// A BulkDataURI is server-supplied data: a compromised or buggy gateway can
// point it at an arbitrary (including internal) address. Because the
// configured auth editors are applied to every request the client issues,
// following such a URI verbatim would hand the caller's credentials to a host
// of the server's choosing.
type BulkDataHostPolicy int

const (
	// BulkDataHostAnyNoCredentials (the default) fetches Bulk Data from any
	// host the URI names — cross-host object storage is standard-conformant —
	// but does not attach the client's credentials (auth/request editors) when
	// the target host differs from the service base URL.
	BulkDataHostAnyNoCredentials BulkDataHostPolicy = iota
	// BulkDataHostAny fetches from any host and forwards credentials there.
	// Use only when the object store is trusted and genuinely needs the same
	// credentials as the gateway.
	BulkDataHostAny
	// BulkDataHostSameOnly rejects Bulk Data URIs whose host differs from the
	// service base URL.
	BulkDataHostSameOnly
)

// String implements fmt.Stringer for diagnostics.
func (p BulkDataHostPolicy) String() string {
	switch p {
	case BulkDataHostAny:
		return "any-host"
	case BulkDataHostSameOnly:
		return "same-host-only"
	default:
		return "any-host-without-credentials"
	}
}

// WithHTTPClient takes over the HTTP client entirely (proxying etc.). The
// takeover includes the redirect policy: the cross-host header stripping the
// library's own clients apply (see WithRequestEditor) is not layered onto a
// caller-supplied client.
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
//
// On library-built clients an editor's headers never travel to a host a
// redirect names: a hop to another host keeps only the headers the library
// itself sets (User-Agent, Accept). This is the same trust boundary
// WithBulkDataHostPolicy draws for Bulk Data URIs; see WithHTTPClient for the
// takeover that keeps a caller's own redirect policy.
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
//
// The setting survives Fork: clients derived from a configured core inherit
// it (multi.Registry builds every per-target client that way).
func WithMaxIdleConnsPerHost(n int) Option { return func(s *settings) { s.maxIdlePerHost = n } }

// WithBulkDataHostPolicy controls how Bulk Data URIs that point at another
// host are handled; the default is BulkDataHostAnyNoCredentials.
//
// BulkDataURIs come from server metadata, so following them blindly lets a
// gateway redirect the client (and, with auth editors configured, its
// credentials) at an arbitrary host. The default keeps cross-host object
// storage working while withholding credentials from it; use
// BulkDataHostSameOnly in a locked-down deployment, or BulkDataHostAny when
// the object store is trusted and needs the same credentials.
func WithBulkDataHostPolicy(p BulkDataHostPolicy) Option {
	return func(s *settings) { s.bulkDataHost = p }
}

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
