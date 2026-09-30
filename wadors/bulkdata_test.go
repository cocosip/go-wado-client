package wadors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// bulkBody is the payload served by the fake bulkdata endpoints.
const bulkBody = "BULK"

func TestFetchBulkData(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(bulkBody))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/api/x")
	ctx := context.Background()

	// Absolute-path reference resolves against scheme+host of the base.
	rc, err := c.FetchBulkData(ctx, "/dicomweb/studies/1.2.3/bulkdata/7FE00010")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != bulkBody || path != "/dicomweb/studies/1.2.3/bulkdata/7FE00010" {
		t.Errorf("body/path = %q/%q", b, path)
	}

	// Relative path resolves per RFC 3986: the last path segment of the
	// base ("/api/x") is replaced, yielding "/api/bulkdata/...". In real
	// metadata flows the base ends with "/metadata", so "./bulkdata"
	// replaces that segment (covered by TestMetadataBulkDataURIResolution).
	rc, err = c.FetchBulkData(ctx, "./bulkdata/00282000")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if path != "/api/bulkdata/00282000" {
		t.Errorf("path = %q, want /api/bulkdata/00282000", path)
	}

	// Absolute URL passes through untouched.
	rc, err = c.FetchBulkData(ctx, srv.URL+"/other/bulk")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if path != "/other/bulk" {
		t.Errorf("path = %q", path)
	}

	// Unsupported scheme rejected per the standard.
	if _, err := c.FetchBulkData(ctx, "dicomweb://x/y"); err == nil {
		t.Error("dicomweb:// expected error")
	}
}

// TestFetchBulkDataMultipartTolerance covers servers that answer with
// multipart despite the single-part expectation.
func TestFetchBulkDataMultipartTolerance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/octet-stream"`)
		_, _ = w.Write(multipartBody([]fakePart{
			{ct: "application/octet-stream", body: bulkBody},
		}))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/api/x")
	rc, err := c.FetchBulkData(context.Background(), "/bulk")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != bulkBody {
		t.Errorf("body = %q", b)
	}
}
