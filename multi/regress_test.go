package multi

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/wadors"
)

// TestTemplateRejectsPathAlteringValues is the regression for the dot-segment
// collapse: url.PathEscape leaves "." and ".." untouched and the subsequent
// path join deletes segments, so a tenant code of ".." silently retargeted the
// client at a different route — with the caller's credentials.
func TestTemplateRejectsPathAlteringValues(t *testing.T) {
	res := Template(testGWBase, Routes{
		RS:  "/api/wado/{hospitalCode}/wado-rs",
		URI: "/api/wado/{hospitalCode}/wado-uri",
	}, func(k string) map[string]string {
		return map[string]string{testPlaceholder: k}
	})

	for _, bad := range []string{".", "..", "", "a/../..", "a/b", `a\b`} {
		t.Run("reject "+bad, func(t *testing.T) {
			if _, err := res.Resolve(context.Background(), bad); err == nil {
				t.Errorf("Resolve(%q) = nil error, want a rejection", bad)
			}
		})
	}

	// A normal value still resolves, and the route keeps its shape.
	ep, err := res.Resolve(context.Background(), "H1")
	if err != nil {
		t.Fatal(err)
	}
	if ep.RSRoute != "/api/wado/H1/wado-rs" {
		t.Errorf("RSRoute = %q", ep.RSRoute)
	}
	// An escaped value still works.
	ep, err = res.Resolve(context.Background(), "H 1")
	if err != nil {
		t.Fatal(err)
	}
	if ep.RSRoute != "/api/wado/H%201/wado-rs" {
		t.Errorf("RSRoute = %q, want the escaped segment", ep.RSRoute)
	}
}

// TestTemplateRejectsEmptyPlaceholderValue pins that an empty value cannot
// silently delete a path segment (which could address another tenant).
func TestTemplateRejectsEmptyPlaceholderValue(t *testing.T) {
	res := Template(testGWBase, Routes{
		RS: "/api/wado/{hospitalCode}/{bizCode}/wado-rs",
	}, func(k string) map[string]string {
		return map[string]string{testPlaceholder: k, "bizCode": ""}
	})
	if _, err := res.Resolve(context.Background(), "H1"); err == nil {
		t.Error("Resolve with an empty placeholder value = nil error, want a rejection")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to mention the empty value", err)
	}
}

// TestTemplateReportsUnreplacedPlaceholder guards the mismatch between the
// placeholder regex and the substitution: a template name outside the pattern
// was sent verbatim as a literal path segment instead of being reported.
func TestTemplateReportsUnreplacedPlaceholder(t *testing.T) {
	cases := []Routes{
		{RS: "/api/wado/{hospital-code}/wado-rs"},
		{RS: "/api/wado/{hospital.code}/wado-rs"},
		{RS: "/api/wado/{unknown}/wado-rs"},
	}
	for _, routes := range cases {
		res := Template(testGWBase, routes, func(k string) map[string]string {
			return map[string]string{testPlaceholder: k}
		})
		_, err := res.Resolve(context.Background(), "H1")
		if err == nil {
			t.Errorf("routes %+v: err = nil, want a missing-placeholder error", routes)
			continue
		}
		if !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("routes %+v: err = %v, want a placeholder error", routes, err)
		}
	}
}

// TestJoinRouteRejectsMalformedRoute is the regression for URL.JoinPath
// silently swallowing its error: a malformed route degraded to the bare base
// URL with a nil error, so a config typo looked like a server fault.
func TestJoinRouteRejectsMalformedRoute(t *testing.T) {
	for _, route := range []string{"/api/wado/100%/wado-rs", "/api/wado/%zz/wado-rs", "/api/wado/%2e%2e/x"} {
		got, err := joinRoute("https://gw.example.com/api/wado", route)
		if err == nil {
			t.Errorf("joinRoute(%q) = %q, nil; want a rejection", route, got)
		}
	}

	// Well-formed routes keep working.
	cases := []struct{ base, route, want string }{
		{testSimpleBase, testSimpleRoute, testSimpleRouteOK},
		{"https://h.cn/static", "/a", "https://h.cn/static/a"},
	}
	for _, c := range cases {
		got, err := joinRoute(c.base, c.route)
		if err != nil {
			t.Fatalf("joinRoute(%q, %q): %v", c.base, c.route, err)
		}
		if got != c.want {
			t.Errorf("joinRoute(%q, %q) = %q, want %q", c.base, c.route, got, c.want)
		}
	}
}

// TestNewRegistryRejectsNilResolver pins the nil-collaborator guard: the
// registry used to accept a nil resolver and panic on the first Client call.
func TestNewRegistryRejectsNilResolver(t *testing.T) {
	reg, err := NewRegistry[string](nil)
	if err == nil {
		t.Fatal("NewRegistry(nil) = nil error, want a rejection")
	}
	if reg != nil {
		t.Errorf("NewRegistry(nil) = %+v, want nil", reg)
	}
}

// TestInvalidateBeatsInFlightBuild is the regression for the lost
// invalidation: a build that started before Invalidate published its stale
// Gateway afterwards, so the documented "rebuild after a credential rotation"
// silently did nothing.
func TestInvalidateBeatsInFlightBuild(t *testing.T) {
	var mu sync.Mutex
	route := "/api/wado/OLD/wado-rs"
	inResolver := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	reg, err := NewRegistry(ResolverFunc[string](func(_ context.Context, _ string) (Endpoint, error) {
		once.Do(func() { close(inResolver) })
		<-release // hold the build open so Invalidate can run mid-flight
		mu.Lock()
		defer mu.Unlock()
		return Endpoint{Base: testGWBase, RSRoute: route}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var racingErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, racingErr = reg.Client(context.Background(), "K")
	}()

	<-inResolver
	mu.Lock()
	route = "/api/wado/NEW/wado-rs"
	mu.Unlock()
	reg.Invalidate("K")
	close(release)
	wg.Wait()

	if racingErr == nil {
		t.Error("the stale Gateway was published despite Invalidate")
	}
	// The next call must see the new configuration, never the stale route.
	g, err := reg.Client(context.Background(), "K")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g.RS.BaseURL(), "OLD") {
		t.Errorf("stale route resurrected: %s", g.RS.BaseURL())
	}
	if !strings.Contains(g.RS.BaseURL(), "NEW") {
		t.Errorf("RS base = %s, want the new route", g.RS.BaseURL())
	}
}

// TestRegistryInheritsMaxIdleConnsPerHost guards the Fork regression from the
// multi side: the registry derives every client through Fork, so the option
// used to be lost for all targets.
func TestRegistryInheritsMaxIdleConnsPerHost(t *testing.T) {
	reg := mustRegistry(t,
		Static[routeKey](map[routeKey]Endpoint{
			{Tenant: "H1", Biz: "ct"}: {Base: testGWBase, RSRoute: "/api/wado/H1/ct/wado-rs"},
		}),
		wado.WithMaxIdleConnsPerHost(64),
	)
	g, err := reg.Client(context.Background(), routeKey{Tenant: "H1", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idleConnsOf(t, g.RS); got != 64 {
		t.Errorf("registry-built client MaxIdleConnsPerHost = %d, want 64 (inherited through Fork)", got)
	}

	// Per-target options are layered on top of the defaults.
	reg2 := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		{Tenant: "H2", Biz: "ct"}: {
			Base: testGWBase, RSRoute: "/api/wado/H2/ct/wado-rs",
			Options: []wado.Option{wado.WithMaxIdleConnsPerHost(128)},
		},
	}), wado.WithMaxIdleConnsPerHost(64))
	g2, err := reg2.Client(context.Background(), routeKey{Tenant: "H2", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if got := idleConnsOf(t, g2.RS); got != 128 {
		t.Errorf("per-target MaxIdleConnsPerHost = %d, want 128", got)
	}
}

// TestRegistryPropagatesBulkDataPolicy pins that the new policy survives the
// Fork chain too.
func TestRegistryPropagatesBulkDataPolicy(t *testing.T) {
	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		{Tenant: "H1", Biz: "ct"}: {Base: testGWBase, RSRoute: "/rs"},
	}), wado.WithBulkDataHostPolicy(wado.BulkDataHostSameOnly))
	g, err := reg.Client(context.Background(), routeKey{Tenant: "H1", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RS == nil {
		t.Fatal("RS client = nil")
	}
	// Exercise the policy through a cross-host bulk fetch: it must be refused.
	if _, err := g.RS.FetchBulkData(context.Background(), "https://evil.example.com/bulk"); err == nil {
		t.Error("cross-host bulk fetch accepted; the registry default policy was lost")
	}
}

// TestRegistryUnknownKeyError covers the sentinel chain.
func TestRegistryUnknownKeyError(t *testing.T) {
	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{}))
	_, err := reg.Client(context.Background(), routeKey{Tenant: "nope"})
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

// idleConnsOf reads the per-host idle pool of the transport behind a client.
// The setting is not exposed on the public API, so it is read reflectively;
// this is a test-only helper.
func idleConnsOf(t *testing.T, c *wadors.Client) int {
	t.Helper()
	v := reflect.ValueOf(c).Elem().FieldByName("svc")
	svc := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*wado.Service)
	core := svc.Core()
	cv := reflect.ValueOf(core).Elem().FieldByName("hc")
	hc := reflect.NewAt(cv.Type(), unsafe.Pointer(cv.UnsafeAddr())).Elem().Interface().(*http.Client)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", hc.Transport)
	}
	return tr.MaxIdleConnsPerHost
}
