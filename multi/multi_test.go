package multi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/qido"
	"github.com/cocosip/go-wado-client/wadouri"
)

// mustRegistry builds a registry, failing the test on contradictory
// defaults.
func mustRegistry[K comparable](t *testing.T, resolver Resolver[K], defaults ...wado.Option) *Registry[K] {
	t.Helper()
	reg, err := NewRegistry(resolver, defaults...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

const testTenant = "hosp-a"

// routeKey is the business-defined composite key (tenant + business line).
type routeKey struct{ Tenant, Biz string }

// newFakeGateway serves both the RS resources (paths containing /studies)
// and the WADO-URI endpoint (paths ending in /wado-uri) behind
// deployment-specific routes.
func newFakeGateway(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/wado-uri"):
			if r.URL.Query().Get("requestType") != "WADO" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/dicom")
			_, _ = w.Write([]byte("DICOMFILE"))
		case strings.HasSuffix(p, "/metadata"):
			w.Header().Set("Content-Type", "application/dicom+json")
			_, _ = w.Write([]byte(`[{"00080018":{"vr":"UI","Value":["1.2.840.777"]}}]`))
		default:
			w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
			_, _ = fmt.Fprint(w, "--BNDRY\r\nContent-Type: application/dicom\r\n\r\nDICOMFILE\r\n--BNDRY--\r\n")
		}
	}))
}

// endpoint builds a concrete Endpoint: one base plus two independent
// routes hanging directly off it.
func endpoint(base, prefix string) Endpoint {
	return Endpoint{
		Base:     base,
		RSRoute:  prefix,
		URIRoute: prefix + "/wado-uri",
	}
}

func TestRegistryStaticCompositeKey(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		{Tenant: testTenant, Biz: "ct"}: endpoint(srv.URL, "/api/wado/hosp-a/ct"),
	}))

	g, err := reg.Client(context.Background(), routeKey{Tenant: testTenant, Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RS.BaseURL() != srv.URL+"/api/wado/hosp-a/ct" {
		t.Errorf("RS base = %q", g.RS.BaseURL())
	}
	if want := srv.URL + "/api/wado/hosp-a/ct/wado-uri"; g.URI.Endpoint() != want {
		t.Errorf("URI endpoint = %q, want %q", g.URI.Endpoint(), want)
	}

	// The RS side works end to end.
	mp, err := g.RS.RetrieveStudy(context.Background(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	parts, err := mp.ReadAll()
	_ = mp.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || string(parts[0]) != "DICOMFILE" {
		t.Errorf("parts = %v", parts)
	}

	// The URI side works end to end.
	resp, err := g.URI.Retrieve(context.Background(), wadouri.Request{
		StudyUID: "1.2.3", SeriesUID: "1.2.4", ObjectUID: "1.2.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Close() }()
	if !resp.IsDICOM() {
		t.Errorf("IsDICOM = false, ContentType = %q", resp.ContentType)
	}
}

// TestBaseJoining verifies how base and route compose into the full URL.
func TestBaseJoining(t *testing.T) {
	cases := []struct {
		base, route, want string
	}{
		{"https://h.cn", "/a/b", "https://h.cn/a/b"},
		{"https://h.cn/", "/a/b", "https://h.cn/a/b"},
		{"https://h.cn:8443", "/api/wado/H1/wado-rs", "https://h.cn:8443/api/wado/H1/wado-rs"},
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

	// A route without a base is an explicit error.
	if _, err := joinRoute("", "/x"); err == nil {
		t.Error("empty base expected error")
	}
}

func TestTemplateFillsBothRoutes(t *testing.T) {
	res := Template("https://gw", Routes{
		RS:  "/api/wado/{hospitalCode}/{businessCode}/wado-rs",
		URI: "/api/wado/{hospitalCode}/{businessCode}/wado-uri",
	}, func(k routeKey) map[string]string {
		return map[string]string{"hospitalCode": k.Tenant, "businessCode": k.Biz}
	})

	ep, err := res.Resolve(context.Background(), routeKey{Tenant: "H1", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Base != "https://gw" {
		t.Errorf("Base = %q", ep.Base)
	}
	if ep.RSRoute != "/api/wado/H1/ct/wado-rs" {
		t.Errorf("RSRoute = %q", ep.RSRoute)
	}
	if ep.URIRoute != "/api/wado/H1/ct/wado-uri" {
		t.Errorf("URIRoute = %q", ep.URIRoute)
	}

	// Values are path-escaped in both routes.
	ep, err = res.Resolve(context.Background(), routeKey{Tenant: "H 1", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ep.RSRoute, "H%201") || !strings.Contains(ep.URIRoute, "H%201") {
		t.Errorf("routes = %q / %q, want escaped values", ep.RSRoute, ep.URIRoute)
	}

	// Missing placeholders name the offending route.
	badRS := Template("https://gw", Routes{RS: "/{a}/{unknown}"}, func(_ routeKey) map[string]string {
		return map[string]string{"a": "x"}
	})
	if _, err := badRS.Resolve(context.Background(), routeKey{}); err == nil ||
		!strings.Contains(err.Error(), "RS route") {
		t.Errorf("err = %v, want RS route placeholder error", err)
	}
	badURI := Template("https://gw", Routes{URI: "/{a}/{unknown}"}, func(_ routeKey) map[string]string {
		return map[string]string{"a": "x"}
	})
	if _, err := badURI.Resolve(context.Background(), routeKey{}); err == nil ||
		!strings.Contains(err.Error(), "URI route") {
		t.Errorf("err = %v, want URI route placeholder error", err)
	}

	// End to end through the registry.
	reg := mustRegistry(t, res)
	g, err := reg.Client(context.Background(), routeKey{Tenant: "H1", Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RS.BaseURL() != "https://gw/api/wado/H1/ct/wado-rs" {
		t.Errorf("RS base = %q", g.RS.BaseURL())
	}
	if g.URI.Endpoint() != "https://gw/api/wado/H1/ct/wado-uri" {
		t.Errorf("URI endpoint = %q", g.URI.Endpoint())
	}
}

func TestRegistryCacheAndInvalidate(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	key := routeKey{Tenant: "a", Biz: "b"}
	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		key: endpoint(srv.URL, "/x"),
	}))

	g1, err := reg.Client(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := reg.Client(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if g1 != g2 {
		t.Error("cached Gateway must be identical")
	}

	reg.Invalidate(key)
	g3, err := reg.Client(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if g3 == g1 {
		t.Error("Gateway must be rebuilt after Invalidate")
	}
}

func TestStaticUnknownKey(t *testing.T) {
	static := Static[string](map[string]Endpoint{})
	if _, err := static.Resolve(context.Background(), "nope"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

func TestURIRouteOnly(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	reg := mustRegistry(t, Static[string](map[string]Endpoint{
		"legacy": {Base: srv.URL, URIRoute: "/api/wado/legacy/wado-uri"},
	}))
	g, err := reg.Client(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if g.RS != nil {
		t.Error("RS must be nil when RSRoute is empty")
	}
	resp, err := g.URI.Retrieve(context.Background(), wadouri.Request{
		StudyUID: "1.2.3", SeriesUID: "1.2.4", ObjectUID: "1.2.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Close()
}

func TestRSRouteOnly(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	reg := mustRegistry(t, Static[string](map[string]Endpoint{
		"modern": {Base: srv.URL, RSRoute: "/api/wado/modern/dicomweb"},
	}))
	g, err := reg.Client(context.Background(), "modern")
	if err != nil {
		t.Fatal(err)
	}
	if g.URI != nil {
		t.Error("URI must be nil when URIRoute is empty")
	}
	mp, err := g.RS.RetrieveStudy(context.Background(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	_ = mp.Close()
}

func TestEndpointValidation(t *testing.T) {
	reg := mustRegistry(t, Static[string](map[string]Endpoint{
		"empty":  {},              // neither route
		"nobase": {RSRoute: "/x"}, // route without a base
	}))
	ctx := context.Background()
	if _, err := reg.Client(ctx, "empty"); err == nil {
		t.Error("endpoint without any route expected error")
	}
	if _, err := reg.Client(ctx, "nobase"); err == nil {
		t.Error("route without base expected error")
	}
}

// TestRegistryResolveOutsideLock verifies that a slow Resolver (database /
// config-service lookup in real deployments) does not block cache hits for
// other keys.
func TestRegistryResolveOutsideLock(t *testing.T) {
	release := make(chan struct{})
	reg := mustRegistry(t, ResolverFunc[string](func(_ context.Context, key string) (Endpoint, error) {
		if key == "slow" {
			<-release
		}
		return Endpoint{Base: "https://gw.example.com", RSRoute: "/rs"}, nil
	}))

	// Warm the cache for "fast" before "slow" starts resolving.
	if _, err := reg.Client(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := reg.Client(context.Background(), "slow")
		done <- err
	}()

	// While "slow" sits inside its resolver, the cached "fast" lookup must
	// still succeed (it would deadlock against a lock held during resolve).
	time.Sleep(50 * time.Millisecond)
	cacheDone := make(chan error, 1)
	go func() {
		_, err := reg.Client(context.Background(), "fast")
		cacheDone <- err
	}()
	select {
	case err := <-cacheDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cache hit blocked behind a slow resolve")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRegistryConcurrentSameKey verifies concurrent misses on one key all
// end up with the cached Gateway instance.
func TestRegistryConcurrentSameKey(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	key := routeKey{Tenant: "a", Biz: "b"}
	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		key: endpoint(srv.URL, "/x"),
	}))

	results := make([]*Gateway, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g, err := reg.Client(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = g
		}(i)
	}
	wg.Wait()
	for i, g := range results {
		if g == nil || g != results[0] {
			t.Fatalf("result[%d] differs from result[0]: concurrent same-key clients must share the cached Gateway", i)
		}
	}
}

// badRoundTripper is a Transport that is not *http.Transport.
type badRoundTripper struct{}

func (badRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unreachable")
}

// TestNewRegistryRejectsBadDefaults verifies that contradictory registry
// defaults fail at construction instead of at first use.
func TestNewRegistryRejectsBadDefaults(t *testing.T) {
	custom := &http.Client{Transport: badRoundTripper{}}
	_, err := NewRegistry(Static[string](nil),
		wado.WithHTTPClient(custom), wado.WithTLSClientConfig(&tls.Config{}))
	if err == nil {
		t.Error("contradictory defaults expected an error")
	}
}

// TestQidoRoute verifies the third (QIDO-RS) route: an endpoint carrying
// only a QidoRoute builds a Gateway with just the Qido client, wired end to
// end through Template placeholders.
func TestQidoRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/qido/hosp-a/studies" && r.URL.Query().Get("PatientID") == "11235813" {
			w.Header().Set("Content-Type", "application/dicom+json")
			_, _ = w.Write([]byte(`[{"0020000D":{"vr":"UI","Value":["1.2.3"]}}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res := Template("https://unused", Routes{
		QIDO: "/api/qido/{hospitalCode}",
	}, func(k routeKey) map[string]string {
		return map[string]string{"hospitalCode": k.Tenant}
	})
	ep, err := res.Resolve(context.Background(), routeKey{Tenant: testTenant, Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.QidoRoute != "/api/qido/hosp-a" {
		t.Errorf("QidoRoute = %q", ep.QidoRoute)
	}

	reg := mustRegistry(t, Static[routeKey](map[routeKey]Endpoint{
		{Tenant: testTenant, Biz: "ct"}: {Base: srv.URL, QidoRoute: "/api/qido/hosp-a"},
	}))
	g, err := reg.Client(context.Background(), routeKey{Tenant: testTenant, Biz: "ct"})
	if err != nil {
		t.Fatal(err)
	}
	if g.RS != nil || g.URI != nil || g.Qido == nil {
		t.Fatalf("gateway = RS:%v URI:%v Qido:%v; want Qido only", g.RS, g.URI, g.Qido)
	}
	res2, err := g.Qido.SearchStudies(context.Background(), qido.Query{
		Match: []qido.Match{{Attribute: "PatientID", Value: "11235813"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Datasets) != 1 {
		t.Fatalf("datasets = %d, want 1", len(res2.Datasets))
	}
}
