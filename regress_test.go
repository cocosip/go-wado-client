package wado

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// transportOf returns the *http.Transport behind a core (in-package access).
func transportOf(t *testing.T, c *Core) *http.Transport {
	t.Helper()
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.hc.Transport)
	}
	return tr
}

// TestForkInheritsMaxIdleConnsPerHost guards the regression where Fork dropped
// WithMaxIdleConnsPerHost: the option is not stored on Core (it is baked into
// the transport), so a derived core silently fell back to the default pool.
// multi.Registry builds every per-target client through Fork, so the setting
// was lost for the whole multi-hospital scenario.
func TestForkInheritsMaxIdleConnsPerHost(t *testing.T) {
	base := mustCore(t, WithMaxIdleConnsPerHost(64))
	if got := transportOf(t, base).MaxIdleConnsPerHost; got != 64 {
		t.Fatalf("base MaxIdleConnsPerHost = %d, want 64", got)
	}

	// A plain Fork must carry the parent's pool forward.
	forked := mustFork(t, base)
	if got := transportOf(t, forked).MaxIdleConnsPerHost; got != 64 {
		t.Errorf("Fork() MaxIdleConnsPerHost = %d, want 64 (inherited)", got)
	}

	// And an explicit override on the fork must win.
	overridden := mustFork(t, base, WithMaxIdleConnsPerHost(128))
	if got := transportOf(t, overridden).MaxIdleConnsPerHost; got != 128 {
		t.Errorf("Fork(WithMaxIdleConnsPerHost(128)) = %d, want 128", got)
	}

	// The base must be unaffected by either fork.
	if got := transportOf(t, base).MaxIdleConnsPerHost; got != 64 {
		t.Errorf("base MaxIdleConnsPerHost = %d after forking, want 64", got)
	}
}

// TestForkDoesNotMutateParentTransport pins that deriving a core never
// modifies the parent's transport (the fork clones it).
func TestForkDoesNotMutateParentTransport(t *testing.T) {
	base := mustCore(t)
	before := transportOf(t, base).TLSClientConfig

	forked := mustFork(t, base, WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}))

	after := transportOf(t, base)
	if after.TLSClientConfig != before {
		t.Errorf("parent TLSClientConfig changed from %p to %p", before, after.TLSClientConfig)
	}
	if after.TLSClientConfig != nil && after.TLSClientConfig.InsecureSkipVerify {
		t.Error("parent transport was mutated by the fork's TLS config")
	}
	forkTr := transportOf(t, forked)
	if forkTr.TLSClientConfig == nil || !forkTr.TLSClientConfig.InsecureSkipVerify {
		t.Error("fork did not receive the TLS config")
	}
	if forkTr == after {
		t.Error("fork shares the parent transport; TLS settings would leak back")
	}
}

// TestTLSConfigWithoutHTTPClient covers the documented hospital self-signed-CA
// path (WithTLSClientConfig alone), which no test exercised before.
func TestTLSConfigWithoutHTTPClient(t *testing.T) {
	cfg := &tls.Config{InsecureSkipVerify: true}
	core := mustCore(t, WithTLSClientConfig(cfg))

	tr := transportOf(t, core)
	if tr.TLSClientConfig != cfg {
		t.Fatalf("TLSClientConfig = %p, want the supplied config %p", tr.TLSClientConfig, cfg)
	}
	if tr.MaxIdleConnsPerHost != 8 {
		t.Errorf("MaxIdleConnsPerHost = %d, want the default 8", tr.MaxIdleConnsPerHost)
	}
	// The fine-grained timeouts must still be in place.
	if tr.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 30s", tr.ResponseHeaderTimeout)
	}
}

// TestParseRetryAfterOverflow guards the integer overflow: a delay-seconds
// value above math.MaxInt64/1e9 wrapped negative, which disabled the header
// entirely instead of honoring it.
func TestParseRetryAfterOverflow(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"absent", "", 0},
		{"seconds", "5", 5 * time.Second},
		{"zero", "0", 0},
		{"negative", "-1", 0},
		{"garbage", "soon", 0},
		{"overflowing seconds", "9223372036854775807", time.Duration(1<<63 - 1)},
		{"large but valid", "1000000", 1000000 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.val != "" {
				h.Set("Retry-After", tc.val)
			}
			got := parseRetryAfter(h)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.val, got, tc.want)
			}
			if got < 0 {
				t.Errorf("parseRetryAfter(%q) returned a negative duration %v", tc.val, got)
			}
		})
	}

	// The HTTP-date form is honored when it lies in the future.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d <= 0 || d > 31*time.Second {
		t.Errorf("HTTP-date Retry-After = %v, want ~30s", d)
	}
	// A past date yields 0 rather than a negative delay.
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d != 0 {
		t.Errorf("past HTTP-date Retry-After = %v, want 0", d)
	}
}

// TestBackoffForBoundedShift guards the shift overflow: a large attempt count
// made InitialBackoff<<(attempt-1) wrap, shortening the sleep instead of
// lengthening it.
func TestBackoffForBoundedShift(t *testing.T) {
	p := RetryPolicy{InitialBackoff: time.Millisecond, MaxBackoff: time.Second}.normalized()
	for _, attempt := range []int{1, 2, 3, 10, 63, 64, 1000, 1 << 20} {
		got := backoffFor(p, attempt)
		if got <= 0 || got > p.MaxBackoff {
			t.Errorf("backoffFor(attempt=%d) = %v, want (0, %v]", attempt, got, p.MaxBackoff)
		}
	}
	// It must be monotonic up to the cap.
	prev := time.Duration(0)
	for attempt := 1; attempt <= 40; attempt++ {
		got := backoffFor(p, attempt)
		if got < prev {
			t.Fatalf("backoffFor is not monotonic: attempt %d gave %v after %v", attempt, got, prev)
		}
		prev = got
	}
}

// TestDoNilContext guards the nil-context panic: http.Client tolerates a nil
// context, so a caller passing one reached req.Clone(nil) and panicked.
func TestDoNilContext(t *testing.T) {
	core := mustCore(t)
	req, err := http.NewRequest(http.MethodGet, "https://gw.example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx context.Context // deliberately nil: the API must not panic
	if _, err := core.Do(nilCtx, req); err == nil {
		t.Fatal("Do(nil ctx) = nil error, want an error instead of a panic")
	}
}

// TestDoRetriesTransportErrorThenSucceeds covers the transport-error retry
// path end to end (previously only status-code retry was tested).
func TestDoRetriesTransportErrorThenSucceeds(t *testing.T) {
	var attempts int
	core := mustCore(t, WithRetry(RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Jitter:         time.Millisecond,
	}))
	core.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection reset by peer")
		}
		_ = r
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("ok")),
			Request:    r,
		}, nil
	})}

	req, err := http.NewRequest(http.MethodGet, "https://gw.example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := core.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two retries after transport errors)", attempts)
	}
}

// TestDoRetryRespectsContextCancellation pins that the backoff sleep aborts on
// cancellation rather than waiting out the full delay.
func TestDoRetryRespectsContextCancellation(t *testing.T) {
	var attempts int
	core := mustCore(t, WithRetry(RetryPolicy{
		MaxAttempts:    5,
		InitialBackoff: 30 * time.Second,
		MaxBackoff:     30 * time.Second,
	}))
	core.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		_ = r
		return nil, errors.New("temporary failure")
	})}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://gw.example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = core.Do(ctx, req)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Do waited %v; cancellation was not honored during backoff", elapsed)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (cancelled before the retry)", attempts)
	}
}

// TestBearerTokenSourceFailureAborts guards the token-refresh failure path.
func TestBearerTokenSourceFailureAborts(t *testing.T) {
	sentinel := errors.New("token endpoint down")
	core := mustCore(t, WithBearerTokenSource(TokenSourceFunc(func() (string, error) {
		return "", sentinel
	})))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Do(context.Background(), req); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the token error", err)
	}
}

// TestWithLogHandler verifies the slog.Handler convenience option.
func TestWithLogHandler(t *testing.T) {
	var buf strings.Builder
	core := mustCore(t, WithLogHandler(newTextHandler(&buf)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := core.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(buf.String(), "request done") {
		t.Errorf("log output = %q, want a request-done record", buf.String())
	}
}

// TestCoreAccessors covers the small exported accessors.
func TestCoreAccessors(t *testing.T) {
	core := mustCore(t, WithLenientUID(), WithLegacyParamNames(), WithBulkDataHostPolicy(BulkDataHostSameOnly))
	if !core.LenientUID() {
		t.Error("LenientUID() = false, want true")
	}
	if !core.LegacyParams() {
		t.Error("LegacyParams() = false, want true")
	}
	if core.BulkDataHost() != BulkDataHostSameOnly {
		t.Errorf("BulkDataHost() = %v, want same-host-only", core.BulkDataHost())
	}
	if core.Logger() == nil {
		t.Error("Logger() = nil")
	}
}

// TestBulkDataHostPolicyString covers the diagnostic rendering.
func TestBulkDataHostPolicyString(t *testing.T) {
	cases := map[BulkDataHostPolicy]string{
		BulkDataHostAnyNoCredentials: "any-host-without-credentials",
		BulkDataHostAny:              "any-host",
		BulkDataHostSameOnly:         "same-host-only",
	}
	for policy, want := range cases {
		if got := policy.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", policy, got, want)
		}
	}
}

// TestServiceSameHost covers the host comparison backing the Bulk Data policy.
func TestServiceSameHost(t *testing.T) {
	svc := newTestService(t, "https://gw.example.com:8443/api/wado")
	cases := map[string]bool{
		"https://gw.example.com:8443/other": true,
		"https://GW.EXAMPLE.COM:8443/x":     true,  // host comparison is case-insensitive
		"https://gw.example.com/x":          false, // different port
		"http://gw.example.com:8443/x":      false, // different scheme
		"https://evil.example.com/x":        false,
	}
	for raw, want := range cases {
		u := mustParseURL(t, raw)
		if got := svc.SameHost(u); got != want {
			t.Errorf("SameHost(%q) = %v, want %v", raw, got, want)
		}
	}
	if svc.SameHost(nil) {
		t.Error("SameHost(nil) = true, want false")
	}
}

// TestDoWithoutCredentialsSkipsEditors pins the mechanism behind the default
// Bulk Data host policy.
func TestDoWithoutCredentialsSkipsEditors(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	core := mustCore(t, WithBasicAuth("user", "pass"))

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := core.DoWithoutCredentials(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if auth != "" {
		t.Errorf("DoWithoutCredentials sent Authorization %q, want none", auth)
	}

	req2, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := core.Do(context.Background(), req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if !strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Do sent Authorization %q, want Basic credentials", auth)
	}
}

// TestForkKeepsBulkDataPolicy pins that the new policy survives Fork.
func TestForkKeepsBulkDataPolicy(t *testing.T) {
	base := mustCore(t, WithBulkDataHostPolicy(BulkDataHostSameOnly))
	forked := mustFork(t, base)
	if forked.BulkDataHost() != BulkDataHostSameOnly {
		t.Errorf("forked BulkDataHost() = %v, want same-host-only", forked.BulkDataHost())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTextHandler builds a debug-level slog handler writing into w.
func newTextHandler(w io.Writer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// mustParseURL parses raw, failing the test on error.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
