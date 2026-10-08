package wadors

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cocosip/go-wado-client"
)

func newRenderedServer(t *testing.T, got *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.query = r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	}))
}

func TestRetrieveRenderedInstanceStandardNames(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL + "/api/wado/H1")
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithRenderedFormat("image/png"),
		WithAnnotation("patient", "technique"),
		WithQuality(90),
		WithViewport(512, 512),
		WithWindow(40, 400),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()

	if got.path != "/api/wado/H1/studies/1.2.3/series/1.2.4/instances/1.2.5/rendered" {
		t.Errorf("path = %q", got.path)
	}
	// PS3.18 §8.3.5: the standard parameter names are the default.
	if v := got.query.Get("annotation"); v != "patient,technique" {
		t.Errorf("annotation = %q", v)
	}
	// window=center,width,function with all three values mandatory (§8.3.5).
	if v := got.query.Get("window"); v != "40,400,linear" {
		t.Errorf("window = %q", v)
	}
	if v := got.query.Get("quality"); v != "90" {
		t.Errorf("quality = %q", v)
	}
	if v := got.query.Get("viewport"); v != "512,512" {
		t.Errorf("viewport = %q", v)
	}
	// The retired WADO-WS-era names must not appear in the default mode.
	for _, k := range []string{"annotations", "windowcenter", "windowwidth", "icccolorspace"} {
		if _, ok := got.query[k]; ok {
			t.Errorf("legacy parameter %q present in default mode", k)
		}
	}
	if img.ContentType != "image/png" {
		t.Errorf("ContentType = %q", img.ContentType)
	}
	b, _ := io.ReadAll(img.Body)
	if string(b) != "PNGDATA" {
		t.Errorf("body = %q", b)
	}
}

func TestRetrieveRenderedInstanceLegacyNames(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL+"/api/wado/H1", wado.WithLegacyParamNames())
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithAnnotation("patient"),
		WithWindow(40, 400),
		WithICCProfile("sRGB"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()

	if v := got.query.Get("annotations"); v != "patient" {
		t.Errorf("annotations = %q", v)
	}
	if v := got.query.Get("windowcenter"); v != "40" {
		t.Errorf("windowcenter = %q", v)
	}
	if v := got.query.Get("windowwidth"); v != "400" {
		t.Errorf("windowwidth = %q", v)
	}
	if v := got.query.Get("icccolorspace"); v != "sRGB" {
		t.Errorf("icccolorspace = %q", v)
	}
	// The standard names must not appear in legacy mode.
	for _, k := range []string{"annotation", "window", "iccprofile"} {
		if _, ok := got.query[k]; ok {
			t.Errorf("standard parameter %q present in legacy mode", k)
		}
	}
}

func TestRenderedRawQueryOverride(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL + "/api/wado/H1")
	raw := url.Values{"annotations": {"custom"}}
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithAnnotation("patient"),
		WithRawQuery(raw),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()

	if v := got.query.Get("annotations"); v != "custom" {
		t.Errorf("annotations = %q, want raw override", v)
	}
}

func TestRenderedValidation(t *testing.T) {
	c, err := New("https://h.example.com/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = c.RetrieveRenderedInstance(ctx, "1.2.3", "1.2.4", "1.2.5", WithRenderedFormat("application/dicom"))
	if !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("application/dicom format err = %v, want ErrInvalidRequest", err)
	}
	_, err = c.RetrieveRenderedInstance(ctx, "1.2.3", "1.2.4", "1.2.5", WithAnnotation("bogus"))
	if !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("unknown annotation err = %v, want ErrInvalidRequest", err)
	}
	for _, q := range []int{-1, 101} {
		_, err = c.RetrieveRenderedInstance(ctx, "1.2.3", "1.2.4", "1.2.5", WithQuality(q))
		if !errors.Is(err, wado.ErrInvalidRequest) {
			t.Errorf("quality %d err = %v, want ErrInvalidRequest", q, err)
		}
	}
}

// TestRenderedWindowFormatNoExponent verifies window values never reach the
// wire in exponent notation ("1e+07"), which some servers cannot parse.
func TestRenderedWindowFormatNoExponent(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	// Standard names.
	c, _ := New(srv.URL + "/api/wado/H1")
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithWindow(1e7, 4e7))
	if err != nil {
		t.Fatal(err)
	}
	_ = img.Close()
	if v := got.query.Get("window"); v != "10000000,40000000,linear" {
		t.Errorf("window = %q, want 10000000,40000000,linear", v)
	}

	// Legacy names.
	c, _ = New(srv.URL+"/api/wado/H1", wado.WithLegacyParamNames())
	img, err = c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithWindow(1e7, 4e7))
	if err != nil {
		t.Fatal(err)
	}
	_ = img.Close()
	if v := got.query.Get("windowcenter"); v != "10000000" {
		t.Errorf("windowcenter = %q, want 10000000", v)
	}
	if v := got.query.Get("windowwidth"); v != "40000000" {
		t.Errorf("windowwidth = %q, want 40000000", v)
	}
}

// TestRenderedWindowFunction covers the window=function component:
// PS3.18 §8.3.5 requires all three values; the legacy windowcenter/windowwidth
// pair has no function component and must ignore the option.
func TestRenderedWindowFunction(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL + "/api/wado/H1")
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithWindow(40, 400), WithWindowFunction(WindowFunctionSigmoid))
	if err != nil {
		t.Fatal(err)
	}
	_ = img.Close()
	if v := got.query.Get("window"); v != "40,400,sigmoid" {
		t.Errorf("window = %q, want 40,400,sigmoid", v)
	}

	// Unknown function names are rejected locally.
	_, err = c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithWindow(40, 400), WithWindowFunction("bogus"))
	if !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("unknown function err = %v, want ErrInvalidRequest", err)
	}

	// Legacy mode has no function component; the option is a no-op there.
	legacy, _ := New(srv.URL+"/api/wado/H1", wado.WithLegacyParamNames())
	img, err = legacy.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithWindow(40, 400), WithWindowFunction(WindowFunctionSigmoid))
	if err != nil {
		t.Fatal(err)
	}
	_ = img.Close()
	if v := got.query.Get("windowcenter"); v != "40" || got.query.Get("windowwidth") != "400" {
		t.Errorf("legacy window = %q/%q, want 40/400 without a function component",
			got.query.Get("windowcenter"), got.query.Get("windowwidth"))
	}
}

func TestRetrieveRenderedFrames(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL + "/api/wado/H1")
	img, err := c.RetrieveRenderedFrames(context.Background(), "1.2.3", "1.2.4", "1.2.5", []int{2, 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()

	want := "/api/wado/H1/studies/1.2.3/series/1.2.4/instances/1.2.5/frames/1,2/rendered"
	if got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
}
