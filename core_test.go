package wado

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoreHeadersAndAuth(t *testing.T) {
	var ua, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx := context.Background()

	core := NewCore(WithBasicAuth("u", "p"), WithUserAgent("ua-test"))
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

	core = NewCore(WithBearerTokenSource(StaticToken("tok")))
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
	core = NewCore()
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

	core := NewCore(WithLogger(logger))
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

	core := NewCore(WithRetry(RetryPolicy{
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

	core := NewCore(WithRetry(RetryPolicy{
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
	core := NewCore(WithRequestEditor(func(*http.Request) error { return sentinel }))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := core.Do(context.Background(), req); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want wrapped %v", err, sentinel)
	}
}

func TestCoreForkSharesTransport(t *testing.T) {
	base := NewCore(WithUserAgent("x"))
	forked := base.Fork(WithBasicAuth("u", "p"))
	if base.hc != forked.hc {
		t.Error("Fork without transport options must share the *http.Client")
	}
	withTLS := base.Fork(WithTLSClientConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
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
	resp, err := NewCore().Do(context.Background(), req)
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
