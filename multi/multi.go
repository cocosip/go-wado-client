// Package multi provides a multi-target (multi-hospital/tenant/business
// line) client registry: a generic business key → endpoint → cached client
// group (RS + URI + Qido).
//
// An Endpoint keeps responsibilities strictly separated: one standard base
// address (scheme://host with an optional static prefix, carrying no
// service routing) plus one independent route per service. Neither route
// derives from the other — the library only joins base and route into the
// full URL. When the two services live on different hosts, multi does not
// apply: construct wadors/wadouri clients directly and manage them
// yourself.
//
// The library makes zero assumptions about two things: (1) the internal
// structure of the routes (they are opaque strings) and (2) the semantics
// of the key that distinguishes targets (K is merely comparable; its
// meaning is defined by the business layer). The registry creates a single
// shared core, and every target client is derived from it — one connection
// pool for the whole process.
package multi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/qido"
	"github.com/cocosip/go-wado-client/wadors"
	"github.com/cocosip/go-wado-client/wadouri"
)

// Endpoint describes the access points of one target: a standard base
// address plus one independent route per service.
type Endpoint struct {
	// Base is the standard base address of the gateway: scheme://host with
	// an optional static prefix, carrying no service routing. Required.
	Base string
	// RSRoute is the WADO-RS route as an absolute path joined onto Base,
	// e.g. "/api/wado/H0001/RIS/wado-rs". Empty means the target offers no
	// WADO-RS service and Gateway.RS will be nil.
	RSRoute string
	// URIRoute is the WADO-URI route as an absolute path joined onto Base,
	// e.g. "/api/wado/H0001/RIS/wado-uri". Empty means the target offers
	// no WADO-URI service and Gateway.URI will be nil.
	URIRoute string
	// QidoRoute is the QIDO-RS route as an absolute path joined onto Base.
	// QIDO-RS is usually served under the same route prefix as WADO-RS —
	// give both routes the same value then. Empty means the target offers
	// no QIDO-RS service and Gateway.Qido will be nil.
	QidoRoute string
	// Options are target-specific settings (auth/timeouts etc.) layered on
	// top of the registry defaults.
	Options []wado.Option
}

// Gateway is the client group of one target; a field is nil when the
// corresponding Endpoint route is empty.
type Gateway struct {
	RS   *wadors.Client
	URI  *wadouri.Client
	Qido *qido.Client
}

// Resolver maps a business key to an endpoint.
type Resolver[K comparable] interface {
	Resolve(ctx context.Context, key K) (Endpoint, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc[K comparable] func(ctx context.Context, key K) (Endpoint, error)

// Resolve calls f(ctx, key).
func (f ResolverFunc[K]) Resolve(ctx context.Context, key K) (Endpoint, error) {
	return f(ctx, key)
}

// ErrUnknownKey is the sentinel root cause of Static lookup failures.
var ErrUnknownKey = errors.New("multi: unknown key")

// Static is a static mapping (typically loaded from a config file or DB).
type Static[K comparable] map[K]Endpoint

// Resolve looks up the endpoint by key, wrapping the failure in
// ErrUnknownKey.
func (s Static[K]) Resolve(_ context.Context, key K) (Endpoint, error) {
	ep, ok := s[key]
	if !ok {
		return Endpoint{}, fmt.Errorf("%w: %v", ErrUnknownKey, key)
	}
	return ep, nil
}

// Routes holds the service route templates of one gateway for Template.
// Placeholders ({name}) are filled per key; an empty route means the
// gateway does not offer that service.
type Routes struct {
	RS   string // WADO-RS route template, e.g. "/api/wado/{hospitalCode}/{businessCode}/wado-rs"
	URI  string // WADO-URI route template, e.g. "/api/wado/{hospitalCode}/{businessCode}/wado-uri"
	QIDO string // QIDO-RS route template; usually the same value as RS
}

// Template returns a Resolver that fills the placeholders of both route
// templates from vars(key) (path-escaped) — the two routes stay fully
// independent — and joins them onto the standard base address.
//
// A placeholder value must be a non-empty path segment: empty values and the
// dot segments "." / ".." are rejected, because url.PathEscape leaves dots
// untouched and the subsequent path join would then delete or escape route
// segments, silently addressing a different (possibly another tenant's)
// route with the caller's credentials.
func Template[K comparable](base string, routes Routes, vars func(K) map[string]string) ResolverFunc[K] {
	return func(_ context.Context, key K) (Endpoint, error) {
		vals := map[string]string{}
		if vars != nil {
			vals = vars(key)
		}
		pairs := make([]string, 0, 2*len(vals))
		for k, v := range vals {
			if err := checkPlaceholderValue(k, v); err != nil {
				return Endpoint{}, err
			}
			pairs = append(pairs, "{"+k+"}", url.PathEscape(v))
		}
		rep := strings.NewReplacer(pairs...)
		rs, err := fillRoute(rep, routes.RS)
		if err != nil {
			return Endpoint{}, fmt.Errorf("multi: RS route: %w", err)
		}
		uri, err := fillRoute(rep, routes.URI)
		if err != nil {
			return Endpoint{}, fmt.Errorf("multi: URI route: %w", err)
		}
		qido, err := fillRoute(rep, routes.QIDO)
		if err != nil {
			return Endpoint{}, fmt.Errorf("multi: QIDO route: %w", err)
		}
		return Endpoint{Base: base, RSRoute: rs, URIRoute: uri, QidoRoute: qido}, nil
	}
}

// checkPlaceholderValue rejects values that would change the route structure
// once substituted and path-joined.
func checkPlaceholderValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("multi: placeholder {%s} resolved to an empty value", name)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("multi: placeholder {%s} resolved to the dot segment %q, which would alter the route path", name, value)
	}
	// A value carrying a separator or an already-escaped dot is equally
	// capable of changing the path once joined.
	if strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("multi: placeholder {%s} resolved to %q, which contains a path separator", name, value)
	}
	return nil
}

// fillRoute replaces placeholders in one route template; empty patterns
// stay empty (service not offered).
//
// The leftover check deliberately scans for any "{...}" token rather than
// only the names the substitution recognises: a template using a character
// the pattern does not cover would otherwise be sent verbatim as a literal
// path segment instead of being reported as a missing value.
func fillRoute(rep *strings.Replacer, pattern string) (string, error) {
	if pattern == "" {
		return "", nil
	}
	s := rep.Replace(pattern)
	if m := leftoverPlaceholder(s); m != "" {
		return "", fmt.Errorf("no value for placeholder %s in %q", m, pattern)
	}
	return s, nil
}

// leftoverPlaceholder returns the first unreplaced "{...}" token in s, or "".
func leftoverPlaceholder(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return s[i:]
		}
		return s[i : i+j+1]
	}
	return ""
}

// Registry caches Gateways by key. The cache has no eviction: it grows by
// one entry per distinct key, which is fine for the intended key spaces
// (hospitals / tenants); use Invalidate to drop entries after a route change
// or credential rotation.
type Registry[K comparable] struct {
	resolver Resolver[K]
	base     *wado.Core
	mu       sync.Mutex
	cache    map[K]*Gateway
	// gen increments on every Invalidate. A build that started before an
	// invalidation carries the older generation and refuses to publish its
	// result, so a stale Gateway can never be resurrected into the cache.
	gen map[K]uint64
}

// NewRegistry creates a registry; defaults are the shared settings for all
// targets (TLS/self-signed CA, timeouts, retry, logging etc.). It fails when
// the defaults are contradictory (see wado.WithTLSClientConfig) or the
// resolver is nil.
func NewRegistry[K comparable](resolver Resolver[K], defaults ...wado.Option) (*Registry[K], error) {
	if resolver == nil {
		return nil, errors.New("multi: nil resolver")
	}
	base, err := wado.NewCore(defaults...)
	if err != nil {
		return nil, err
	}
	return &Registry[K]{
		resolver: resolver,
		base:     base,
		cache:    map[K]*Gateway{},
		gen:      map[K]uint64{},
	}, nil
}

// Client returns the Gateway for the key (created lazily and cached) — the
// business key appears here and nowhere else.
//
// Resolution runs outside the registry lock: a slow Resolver (database /
// config-service lookup) must not block cache hits for other keys.
// Concurrent misses on the same key may each build a Gateway; the first to
// finish wins the cache and the others get that instance.
func (r *Registry[K]) Client(ctx context.Context, key K) (*Gateway, error) {
	r.mu.Lock()
	g, ok := r.cache[key]
	gen := r.gen[key]
	r.mu.Unlock()
	if ok {
		return g, nil
	}

	ep, err := r.resolver.Resolve(ctx, key)
	if err != nil {
		return nil, err
	}
	if ep.RSRoute == "" && ep.URIRoute == "" && ep.QidoRoute == "" {
		return nil, fmt.Errorf("multi: endpoint for key %v sets neither RSRoute nor URIRoute nor QidoRoute", key)
	}
	core, err := r.base.Fork(ep.Options...)
	if err != nil {
		return nil, err
	}
	g = &Gateway{}
	if ep.RSRoute != "" {
		full, err := joinRoute(ep.Base, ep.RSRoute)
		if err != nil {
			return nil, err
		}
		if g.RS, err = wadors.NewWithCore(core, full); err != nil {
			return nil, err
		}
	}
	if ep.URIRoute != "" {
		full, err := joinRoute(ep.Base, ep.URIRoute)
		if err != nil {
			return nil, err
		}
		if g.URI, err = wadouri.NewWithCore(core, full); err != nil {
			return nil, err
		}
	}
	if ep.QidoRoute != "" {
		full, err := joinRoute(ep.Base, ep.QidoRoute)
		if err != nil {
			return nil, err
		}
		if g.Qido, err = qido.NewWithCore(core, full); err != nil {
			return nil, err
		}
	}

	r.mu.Lock()
	if cached, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	if r.gen[key] != gen {
		// Invalidate ran while this Gateway was being built: the result is
		// based on superseded configuration, so it must not be published.
		r.mu.Unlock()
		return nil, fmt.Errorf("multi: endpoint for key %v was invalidated while being resolved; retry", key)
	}
	r.cache[key] = g
	r.mu.Unlock()
	return g, nil
}

// Invalidate drops the cached Gateway for the key (rebuild after a route
// change or credential rotation).
//
// It also invalidates any build that is currently in flight for the key: such
// a build refuses to publish its (now stale) Gateway, so the next Client call
// rebuilds from the current configuration instead of resurrecting it.
func (r *Registry[K]) Invalidate(key K) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, key)
	r.gen[key]++
}

// joinRoute joins an absolute route path onto the standard base address.
//
// The route is validated before joining: url.URL.JoinPath silently discards
// its internal error and returns the *base* URL unchanged when the path is
// malformed (a stray "%"), so relying on it alone would turn a configuration
// typo into a client that quietly addresses the wrong prefix instead of
// failing.
func joinRoute(base, route string) (string, error) {
	u, err := wado.ParseBaseURL(base)
	if err != nil {
		return "", err
	}
	if err := validateRoute(route); err != nil {
		return "", err
	}
	joined := u.JoinPath(route)
	if joined.Path == u.Path && strings.Trim(route, "/") != "" {
		// JoinPath dropped the route: refuse to hand back a client pointed at
		// the bare base URL.
		return "", fmt.Errorf("multi: route %q could not be joined onto base %q", route, base)
	}
	return joined.String(), nil
}

// validateRoute rejects route paths that cannot be safely joined.
func validateRoute(route string) error {
	if strings.Contains(route, "%") {
		// Reject any percent sign: either it is a malformed escape (which
		// JoinPath silently drops) or a pre-encoded value that JoinPath would
		// double-encode. Callers should pass the route as a plain path.
		if _, err := url.PathUnescape(route); err != nil {
			return fmt.Errorf("multi: route %q contains an invalid percent escape", route)
		}
		return fmt.Errorf("multi: route %q must not contain percent-encoded characters; pass the route as a plain path", route)
	}
	return nil
}
