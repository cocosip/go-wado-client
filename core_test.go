package wado

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// mustCore builds a core, failing the test on contradictory options.
func mustCore(t *testing.T, opts ...Option) *Core {
	t.Helper()
	c, err := NewCore(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustFork forks a core, failing the test on contradictory options.
func mustFork(t *testing.T, c *Core, opts ...Option) *Core {
	t.Helper()
	f, err := c.Fork(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCoreHeadersAndAuth(t *testing.T) {
	var ua, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx := context.Background()

	core := mustCore(t, WithBasicAuth("u", "p"), WithUserAgent("ua-test"))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := core.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ua != "ua-test" {
		t.Errorf("User-Agent = %q, want ua-test", ua)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if auth != want {
		t.Errorf("Authorization = %q, want %q", auth, want)
	}

	core = mustCore(t, WithBearerTokenSource(StaticToken("tok")))
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err = core.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", auth)
	}

	// Default UA applies when none is configured.
	core = mustCore(t)
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err = core.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ua != DefaultUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, DefaultUserAgent)
	}
}

// TestCoreLoggerInjection verifies slog integration: the logger is injected
// explicitly and slog.Default() is never used implicitly.
func TestCoreLoggerInjection(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	core := mustCore(t, WithLogger(logger))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := core.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out := buf.String()
	if !strings.Contains(out, "request done") {
		t.Errorf("injected logger received %q, want a request done record", out)
	}
}

func TestCoreRetryOn503(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	core := mustCore(t, WithRetry(RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := core.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := n.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCoreNoRetryOn404(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	core := mustCore(t, WithRetry(RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := core.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := n.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (404 is not retryable)", got)
	}
}

func TestCoreEditorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be sent when the editor fails")
	}))
	defer srv.Close()

	sentinel := errors.New("boom")
	core := mustCore(t, WithRequestEditor(func(*http.Request) error { return sentinel }))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := core.Do(context.Background(), req); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrapped %v", err, sentinel)
	}
}

func TestCoreForkSharesTransport(t *testing.T) {
	base := mustCore(t, WithUserAgent("x"))
	forked := mustFork(t, base, WithBasicAuth("u", "p"))
	if base.hc != forked.hc {
		t.Error("Fork without transport options must share the *http.Client")
	}
	withTLS := mustFork(t, base, WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	if base.hc == withTLS.hc {
		t.Error("Fork with TLS config must not share the *http.Client")
	}
}

func TestNewStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such study"))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := mustCore(t).Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	se := NewStatusError(req, resp)
	if !se.IsNotFound() {
		t.Error("IsNotFound expected")
	}
	if se.IsRetryable() {
		t.Error("404 must not be retryable")
	}
	if !strings.Contains(se.Error(), "404") || !strings.Contains(se.Error(), "no such study") {
		t.Errorf("Error() = %q, want status and body included", se.Error())
	}

	retryable := &StatusError{StatusCode: http.StatusServiceUnavailable}
	if !retryable.IsRetryable() {
		t.Error("503 must be retryable")
	}
}

// TestStatusErrorRedactionAndTruncation guards the error-hygiene contract:
// Set-Cookie never leaks into the retained headers, and the echoed body is
// truncated to 4KB without splitting multi-byte runes.
func TestStatusErrorRedactionAndTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "SESSIONID=topsecret; HttpOnly")
		w.WriteHeader(http.StatusForbidden)
		// 66 CJK runes (198 bytes) plus one more rune cut in half by the
		// 200-byte message truncation, then filler.
		_, _ = w.Write([]byte(strings.Repeat("检", 66) + "查" + strings.Repeat("x", 8192)))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := mustCore(t).Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	se := NewStatusError(req, resp)
	if _, ok := se.Header["Set-Cookie"]; ok {
		t.Error("Set-Cookie leaked into StatusError.Header")
	}
	msg := se.Error()
	if !utf8.ValidString(msg) {
		t.Errorf("StatusError.Error() is not valid UTF-8: %q", msg)
	}
	if !strings.Contains(msg, strings.Repeat("检", 10)) {
		t.Errorf("StatusError.Error() lost the readable prefix: %q", msg)
	}
	if len(se.Body) > 4096 {
		t.Errorf("body = %d bytes, want <= 4096", len(se.Body))
	}
}

// TestSleepBackoffCapsRetryAfter guards against a hostile or buggy gateway
// stalling the client past the configured ceiling with a huge Retry-After.
func TestSleepBackoffCapsRetryAfter(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}
	start := time.Now()
	if err := sleepBackoff(context.Background(), p, 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 5*time.Millisecond {
		t.Errorf("elapsed = %v, want at least the capped 5ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %v, Retry-After was not capped at MaxBackoff", elapsed)
	}
}

// roundTripperFunc is a Transport that is not *http.Transport.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCoreRejectsIncompatibleTLS pins the constructor contract: TLS settings
// cannot be applied to a client whose Transport is a wrapper RoundTripper,
// and the combination must fail at construction instead of silently ignoring
// the TLS config until request time.
func TestCoreRejectsIncompatibleTLS(t *testing.T) {
	custom := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request must not be sent")
		return nil, errors.New("unreachable")
	})}
	_, err := NewCore(WithHTTPClient(custom), WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	if err == nil {
		t.Fatal("expected an error for WithHTTPClient(wrapper transport) + WithTLSClientConfig")
	}
	if !strings.Contains(err.Error(), "WithTLSClientConfig") {
		t.Errorf("err = %v, want it to name the offending option", err)
	}

	// *http.Transport transports and nil transports keep working.
	if _, err := NewCore(WithHTTPClient(&http.Client{Transport: http.DefaultTransport}), WithTLSClientConfig(&tls.Config{})); err != nil {
		t.Errorf("err = %v, want nil for an *http.Transport", err)
	}
	if _, err := NewCore(WithHTTPClient(&http.Client{}), WithTLSClientConfig(&tls.Config{})); err != nil {
		t.Errorf("err = %v, want nil for a nil transport", err)
	}
}

// TestCoreNoRetryOnPermanentTransportError verifies the retry classifier:
// context cancellation, TLS certificate verification failures and scheme
// mismatches are permanent and must not be retried.
func TestCoreNoRetryOnPermanentTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"context canceled", context.Canceled},
		{"context deadline", context.DeadlineExceeded},
		{"certificate verification", &tls.CertificateVerificationError{}},
		{"wrapped certificate verification", fmt.Errorf("Get %q: %w", "https://h", &tls.CertificateVerificationError{})},
		{"scheme mismatch", http.ErrSchemeMismatch},
		{"wrapped deadline", fmt.Errorf("Get %q: %w", "https://h", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		if retryableErr(tc.err) {
			t.Errorf("%s: retryableErr(%v) = true, want false", tc.name, tc.err)
		}
	}
	retryable := []error{
		errors.New("connection refused"),
		io.EOF,
		&net.OpError{Op: "dial", Err: errors.New("refused")},
	}
	for _, err := range retryable {
		if !retryableErr(err) {
			t.Errorf("retryableErr(%v) = false, want true", err)
		}
	}

	// End to end: an untrusted certificate fails after a single attempt.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request must not reach the server")
	}))
	defer srv.Close()
	core := mustCore(t, WithRetry(RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond}))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := core.Do(context.Background(), req)
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("err = %v, want a *tls.CertificateVerificationError", err)
	}
}

// TestCoreBodyReplay verifies the body contract of Core.Do: in-memory bodies
// (GetBody set) are replayed across retry attempts, a body that cannot be
// replayed is rejected up front when retry is enabled, and a single attempt
// (retry disabled) accepts it.
func TestCoreBodyReplay(t *testing.T) {
	var attempts int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&attempts, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	// Retryable status + GetBody-backed body: replayed on the second attempt.
	core := mustCore(t, WithRetry(RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, bytes.NewBufferString("payload"))
	resp, err := core.Do(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if attempts != 2 || len(bodies) != 2 {
		t.Fatalf("attempts = %d, bodies = %d, want 2 each", attempts, len(bodies))
	}
	if bodies[0] != "payload" || bodies[1] != "payload" {
		t.Errorf("bodies = %q, want both attempts to carry the full payload", bodies)
	}

	// Body without GetBody + retry enabled: rejected before anything is sent.
	sent := false
	srv2 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer srv2.Close()
	core2 := mustCore(t, WithRetry(RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond}))
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv2.URL, nil)
	req2.Body = io.NopCloser(strings.NewReader("unreplayable"))
	req2.GetBody = nil
	if _, err := core2.Do(ctx, req2); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Errorf("err = %v, want a body replay error", err)
	}
	if sent {
		t.Error("request with unreplayable body must not be sent")
	}

	// Retry disabled: the same request passes through untouched.
	core3 := mustCore(t)
	req3, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv2.URL, nil)
	req3.Body = io.NopCloser(strings.NewReader("unreplayable"))
	req3.GetBody = nil
	resp3, err := core3.Do(ctx, req3)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp3.Body.Close()
	if !sent {
		t.Error("single-attempt request must be sent")
	}
}

// TestStatusErrorURLRedaction pins the error-hygiene contract for the URL:
// userinfo passwords are masked (as on the log path) while the query string
// stays intact for diagnostics.
func TestStatusErrorURLRedaction(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://user:secret@gw.example.com/wado?requestType=WADO&studyUID=1.2.3", nil)
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Status:     "403 Forbidden",
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("denied")),
	}
	se := NewStatusError(req, resp)
	if strings.Contains(se.URL, "secret") {
		t.Errorf("StatusError.URL = %q, want the password redacted", se.URL)
	}
	if !strings.Contains(se.URL, "requestType=WADO") || !strings.Contains(se.URL, "studyUID=1.2.3") {
		t.Errorf("StatusError.URL = %q, want the query preserved for diagnostics", se.URL)
	}
	if !strings.Contains(se.URL, "xxxxx") {
		t.Errorf("StatusError.URL = %q, want the stdlib redaction marker", se.URL)
	}
	_ = se.Error() // must not panic on the redacted URL
}

// TestWithMaxIdleConnsPerHost verifies the idle-pool override: it applies to
// transports the library builds, and is left alone when the caller supplies
// a client with its own transport.
func TestWithMaxIdleConnsPerHost(t *testing.T) {
	core := mustCore(t, WithMaxIdleConnsPerHost(64))
	tpt, ok := core.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", core.hc.Transport)
	}
	if tpt.MaxIdleConnsPerHost != 64 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 64", tpt.MaxIdleConnsPerHost)
	}

	// Default stays at the library default when the option is absent.
	coreDefault := mustCore(t)
	tptDefault, ok := coreDefault.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", coreDefault.hc.Transport)
	}
	if tptDefault.MaxIdleConnsPerHost != 8 {
		t.Errorf("MaxIdleConnsPerHost = %d, want the default 8", tptDefault.MaxIdleConnsPerHost)
	}

	// A caller-supplied client without TLS config is handed back untouched.
	callerTpt := http.DefaultTransport.(*http.Transport).Clone()
	custom := &http.Client{Transport: callerTpt}
	coreCustom := mustCore(t, WithHTTPClient(custom), WithMaxIdleConnsPerHost(64))
	if coreCustom.hc != custom || coreCustom.hc.Transport.(*http.Transport) != callerTpt {
		t.Error("WithHTTPClient without TLS must keep the caller's client and transport")
	}
	if callerTpt.MaxIdleConnsPerHost == 64 {
		t.Error("WithMaxIdleConnsPerHost must not mutate a caller-supplied transport")
	}
}
