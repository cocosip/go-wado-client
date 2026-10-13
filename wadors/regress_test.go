package wadors

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocosip/go-wado-client"
)

// truncServer serves a multipart body over a close-delimited HTTP/1.0
// response, so the body simply ends when the server closes the connection —
// the shape that makes a truncated download indistinguishable from a complete
// one at the transport level.
func truncServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
			_, _ = io.WriteString(w, body)
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

const (
	truncatedBody = "--BNDRY\r\nContent-Type: application/dicom\r\n\r\nPART1\r\n" +
		"--BNDRY\r\nContent-Type: application/dicom\r\n\r\nPART2\r\n" +
		"--BNDRY\r\n" // a boundary line, but never the closing "--BNDRY--"
	completeBody = "--BNDRY\r\nContent-Type: application/dicom\r\n\r\nPART1\r\n" +
		"--BNDRY\r\nContent-Type: application/dicom\r\n\r\nPART2\r\n" +
		"--BNDRY--\r\n"
)

// TestMultipartTruncatedIsNotCleanEOF is the regression for the silent
// partial download: a response that stops at a part boundary without the
// closing delimiter used to be reported as a clean io.EOF, so a caller (and
// WriteToDir) treated a half-downloaded Study as complete.
func TestMultipartTruncatedIsNotCleanEOF(t *testing.T) {
	srv := truncServer(t, truncatedBody)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	parts, rerr := mp.ReadAll()
	if rerr == nil {
		t.Fatalf("ReadAll returned %d parts with no error; a truncated response must not look complete", len(parts))
	}
	if !errors.Is(rerr, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", rerr)
	}
	// The parts read before the truncation are still delivered.
	if len(parts) != 2 {
		t.Errorf("parts = %d, want the 2 that arrived", len(parts))
	}
}

// TestMultipartCompleteEndsWithCleanEOF pins that the fix does not turn a
// well-formed response into an error, and that a repeated Next stays a plain
// io.EOF.
func TestMultipartCompleteEndsWithCleanEOF(t *testing.T) {
	srv := truncServer(t, completeBody)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	parts, err := mp.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll on a complete body: %v", err)
	}
	if len(parts) != 2 || string(parts[0]) != "PART1" || string(parts[1]) != "PART2" {
		t.Errorf("parts = %q", parts)
	}
	if _, err := mp.Next(); err != io.EOF {
		t.Errorf("Next after exhaustion = %v, want io.EOF", err)
	}
	if _, err := mp.Next(); err != io.EOF {
		t.Errorf("second Next after exhaustion = %v, want io.EOF", err)
	}
}

// TestMultipartTruncatedMidPart pins that a part cut off mid-body is surfaced
// too (it always was, via the part reader).
func TestMultipartTruncatedMidPart(t *testing.T) {
	srv := truncServer(t, "--BNDRY\r\nContent-Type: application/dicom\r\n\r\nPART1")
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	var sawErr bool
	for p, err := range mp.Parts() {
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
			}
			sawErr = true
			break
		}
		if _, err := io.ReadAll(p); err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("part read err = %v, want io.ErrUnexpectedEOF", err)
			}
			sawErr = true
			break
		}
	}
	if !sawErr {
		t.Error("truncation mid-part was not surfaced")
	}
}

// TestWriteToDirTruncatedReturnsError pins that WriteToDir propagates the
// truncation instead of silently persisting a partial Study.
func TestWriteToDirTruncatedReturnsError(t *testing.T) {
	srv := truncServer(t, truncatedBody)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	files, err := mp.WriteToDir(t.TempDir())
	if err == nil {
		t.Errorf("WriteToDir returned %d files and no error for a truncated response", len(files))
	}
}

// TestRenderedMultipartContentTypeIsCaseInsensitive guards the case-sensitive
// prefix test: "Multipart/Related" used to be delivered as raw image bytes,
// boundary and part headers included.
func TestRenderedMultipartContentTypeIsCaseInsensitive(t *testing.T) {
	const body = "--B\r\nContent-Type: image/jpeg\r\n\r\nJPEGBYTES\r\n--B--\r\n"
	for _, ct := range []string{
		`multipart/related; boundary=B; type="image/jpeg"`,
		`Multipart/Related; boundary=B; type="image/jpeg"`,
		`MULTIPART/RELATED; boundary=B; type="image/jpeg"`,
	} {
		t.Run(ct, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "Content-Type: image/jpeg") || strings.Contains(string(b), "--B") {
				t.Errorf("multipart framing leaked into the image body: %q", b)
			}
			if string(b) != "JPEGBYTES" {
				t.Errorf("body = %q, want JPEGBYTES", b)
			}
			if r.ContentType != "image/jpeg" {
				t.Errorf("ContentType = %q, want image/jpeg", r.ContentType)
			}
		})
	}
}

// TestRenderedRejectsDICOMFormatRegardlessOfCaseOrSource guards the guard
// itself: it used to test cfg.format while the wire value came from
// cfg.accept, so WithRenderedAccept("application/dicom") slipped through.
func TestRenderedRejectsDICOMFormatRegardlessOfCaseOrSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	opts := map[string][]RenderedOption{
		"format exact":     {WithRenderedFormat("application/dicom")},
		"format uppercase": {WithRenderedFormat("APPLICATION/DICOM")},
		"format mixed":     {WithRenderedFormat("Application/Dicom")},
		"format parameter": {WithRenderedFormat("application/dicom; transfer-syntax=1.2.840.10008.1.2.1")},
		"accept override":  {WithRenderedAccept("application/dicom")},
		"accept overrides a good format": {
			WithRenderedFormat("image/png"), WithRenderedAccept("application/dicom"),
		},
	}
	for name, o := range opts {
		t.Run(name, func(t *testing.T) {
			if _, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5", o...); !errors.Is(err, wado.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}

	// A good Accept must still work even when the format option is the
	// forbidden one (the effective value is what counts).
	if _, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithRenderedFormat("application/dicom"), WithRenderedAccept("image/png")); err != nil {
		t.Errorf("effective Accept image/png rejected: %v", err)
	}
}

// TestRenderedViewportAndWindowValidation guards the missing range checks:
// degenerate viewports and non-finite window values used to reach the wire.
func TestRenderedViewportAndWindowValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string][]RenderedOption{
		"viewport zero columns": {WithViewport(0, 512)},
		"viewport zero rows":    {WithViewport(512, 0)},
		"viewport negative":     {WithViewport(-10, 512)},
		"window NaN":            {WithWindow(math.NaN(), math.NaN())},
		"window +Inf":           {WithWindow(math.Inf(1), 400)},
		"window -Inf":           {WithWindow(math.Inf(-1), 400)},
		"window zero width":     {WithWindow(40, 0)},
		"window negative width": {WithWindow(40, -400)},
		"window function bogus": {WithWindow(40, 400), WithWindowFunction("bogus")},
	}
	for name, o := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5", o...); !errors.Is(err, wado.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}

	// Valid values must pass.
	if _, err := c.RetrieveRenderedInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5",
		WithViewport(512, 512), WithWindow(40, 400), WithWindowFunction(WindowFunctionSigmoid)); err != nil {
		t.Errorf("valid viewport/window rejected: %v", err)
	}
}

// TestMetadataStreamWithCustomRoundTripper guards the resp.Request nil
// dereference: a wrapper RoundTripper is explicitly supported, and http.Client
// itself never populates Response.Request.
func TestMetadataStreamWithCustomRoundTripper(t *testing.T) {
	core, err := wado.NewCore(wado.WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/dicom+json"}},
				Body: io.NopCloser(strings.NewReader(
					`[{"00080018":{"vr":"UI","Value":["1.2.3"]},"7FE00010":{"vr":"OB","BulkDataURI":"./bulkdata/7FE00010"}}]`)),
				Request: nil, // deliberately unset, as a caching RoundTripper would
			}, nil
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWithCore(core, "https://gw.example.com/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	// Must not panic.
	dss, err := c.StudyMetadata(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatalf("StudyMetadata: %v", err)
	}
	if len(dss) != 1 {
		t.Fatalf("datasets = %d, want 1", len(dss))
	}
}

// TestBulkDataCrossHostWithholdsCredentials is the regression for the
// credential leak: a server-supplied BulkDataURI pointing at another host must
// not receive the gateway's Authorization header under the default policy.
func TestBulkDataCrossHostWithholdsCredentials(t *testing.T) {
	var otherAuth, otherHost string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherAuth, otherHost = r.Header.Get("Authorization"), r.Host
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("BULK"))
	}))
	defer other.Close()

	var sameAuth string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sameAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("BULK"))
	}))
	defer gateway.Close()

	c, err := New(gateway.URL, wado.WithBasicAuth("svc", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}

	// Same host: credentials are attached.
	rc, err := c.FetchBulkData(context.Background(), "/bulk/1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if !strings.HasPrefix(sameAuth, "Basic ") {
		t.Errorf("same-host Authorization = %q, want the configured credentials", sameAuth)
	}

	// Cross host (default policy): no credentials.
	rc, err = c.FetchBulkData(context.Background(), other.URL+"/bulk/1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "BULK" {
		t.Errorf("cross-host body = %q, want BULK (cross-host fetch must still work)", body)
	}
	if otherAuth != "" {
		t.Errorf("cross-host host %s received Authorization %q; credentials must not follow a server-supplied URI", otherHost, otherAuth)
	}

	// BulkDataHostAny opts back in to forwarding credentials.
	anyHost, err := New(gateway.URL, wado.WithBasicAuth("svc", "s3cret"), wado.WithBulkDataHostPolicy(wado.BulkDataHostAny))
	if err != nil {
		t.Fatal(err)
	}
	rc, err = anyHost.FetchBulkData(context.Background(), other.URL+"/bulk/2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if !strings.HasPrefix(otherAuth, "Basic ") {
		t.Errorf("BulkDataHostAny Authorization = %q, want credentials forwarded", otherAuth)
	}

	// BulkDataHostSameOnly rejects the cross-host URI outright.
	same, err := New(gateway.URL, wado.WithBasicAuth("svc", "s3cret"), wado.WithBulkDataHostPolicy(wado.BulkDataHostSameOnly))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := same.FetchBulkData(context.Background(), other.URL+"/bulk/3"); err == nil {
		t.Error("BulkDataHostSameOnly accepted a cross-host BulkDataURI")
	}
}

// TestBulkDataRejectsNonHTTPScheme guards scheme validation of a
// server-supplied URI.
func TestBulkDataRejectsNonHTTPScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"dicomweb://x/y", "file:///etc/passwd", "ftp://h/x"} {
		if _, err := c.FetchBulkData(context.Background(), uri); err == nil {
			t.Errorf("FetchBulkData(%q) = nil error, want a rejection", uri)
		}
	}
}

// TestMultipartHeaderIsACopy guards the aliasing fix.
func TestMultipartHeaderIsACopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		w.Header().Set("X-Gateway", "original")
		_, _ = w.Write(multipartBody([]fakePart{{ct: mediaTypeDICOM, body: "P1"}}))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	h := mp.Header()
	h.Set("X-Gateway", "mutated")
	if got := mp.Header().Get("X-Gateway"); got != "original" {
		t.Errorf("mutating the returned header changed the response header: %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
