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
	"regexp"
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

var placeholderRe = regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)

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
func Template[K comparable](base string, routes Routes, vars func(K) map[string]string) ResolverFunc[K] {
	return func(_ context.Context, key K) (Endpoint, error) {
		vals := map[string]string{}
		if vars != nil {
			vals = vars(key)
		}
		pairs := make([]string, 0, 2*len(vals))
		for k, v := range vals {
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

// fillRoute replaces placeholders in one route template; empty patterns
// stay empty (service not offered).
func fillRoute(rep *strings.Replacer, pattern string) (string, error) {
	if pattern == "" {
		return "", nil
	}
	s := rep.Replace(pattern)
	if m := placeholderRe.FindString(s); m != "" {
		return "", fmt.Errorf("no value for placeholder %s in %q", m, pattern)
	}
	return s, nil
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
}

// NewRegistry creates a registry; defaults are the shared settings for all
// targets (TLS/self-signed CA, timeouts, retry, logging etc.). It fails when
// the defaults are contradictory (see wado.WithTLSClientConfig).
func NewRegistry[K comparable](resolver Resolver[K], defaults ...wado.Option) (*Registry[K], error) {
	base, err := wado.NewCore(defaults...)
	if err != nil {
		return nil, err
	}
	return &Registry[K]{
		resolver: resolver,
		base:     base,
		cache:    map[K]*Gateway{},
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
	r.cache[key] = g
	r.mu.Unlock()
	return g, nil
}

// Invalidate drops the cached Gateway for the key (rebuild after a route
// change or credential rotation).
func (r *Registry[K]) Invalidate(key K) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, key)
}

// joinRoute joins an absolute route path onto the standard base address.
func joinRoute(base, route string) (string, error) {
	u, err := wado.ParseBaseURL(base)
	if err != nil {
		return "", err
	}
	return u.JoinPath(route).String(), nil
}
