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

func TestRetrieveRenderedInstanceClassicNames(t *testing.T) {
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
	if v := got.query.Get("annotations"); v != "patient,technique" {
		t.Errorf("annotations = %q", v)
	}
	if v := got.query.Get("windowcenter"); v != "40" {
		t.Errorf("windowcenter = %q", v)
	}
	if v := got.query.Get("windowwidth"); v != "400" {
		t.Errorf("windowwidth = %q", v)
	}
	if v := got.query.Get("quality"); v != "90" {
		t.Errorf("quality = %q", v)
	}
	if v := got.query.Get("viewport"); v != "512,512" {
		t.Errorf("viewport = %q", v)
	}
	if img.ContentType != "image/png" {
		t.Errorf("ContentType = %q", img.ContentType)
	}
	b, _ := io.ReadAll(img.Body)
	if string(b) != "PNGDATA" {
		t.Errorf("body = %q", b)
	}
}

func TestRetrieveRenderedInstanceModernNames(t *testing.T) {
	var got captured
	srv := newRenderedServer(t, &got)
	defer srv.Close()

	c, _ := New(srv.URL+"/api/wado/H1", wado.WithModernParamNames())
	img, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithAnnotation("patient"),
		WithWindow(40, 400),
		WithICCProfile("sRGB"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()

	if v := got.query.Get("annotation"); v != "patient" {
		t.Errorf("annotation = %q", v)
	}
	if v := got.query.Get("window"); v != "40,400" {
		t.Errorf("window = %q", v)
	}
	if v := got.query.Get("iccprofile"); v != "sRGB" {
		t.Errorf("iccprofile = %q", v)
	}
	// Classic names must not appear in modern mode.
	for _, k := range []string{"annotations", "windowcenter", "windowwidth", "icccolorspace"} {
		if _, ok := got.query[k]; ok {
			t.Errorf("classic parameter %q present in modern mode", k)
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
