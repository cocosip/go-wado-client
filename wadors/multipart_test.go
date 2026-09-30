package wadors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMultipartIteration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = w.Write(multipartBody(dicomParts(r.Host)))
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	if mp.PartMediaType != mediaTypeDICOM {
		t.Errorf("PartMediaType = %q, want %q", mp.PartMediaType, mediaTypeDICOM)
	}

	var contents []string
	for p, err := range mp.Parts() {
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		contents = append(contents, string(b))
	}
	if len(contents) != 2 || contents[0] != dicomData1 || contents[1] != dicomData2 {
		t.Errorf("contents = %v", contents)
	}

	// Next after exhaustion returns io.EOF.
	if _, err := mp.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestMultipartEarlyBreak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = w.Write(multipartBody(dicomParts(r.Host)))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	n := 0
	for p, err := range mp.Parts() {
		if err != nil {
			t.Fatal(err)
		}
		n++
		_ = p
		break
	}
	if n != 1 {
		t.Errorf("iterated %d parts after break, want 1", n)
	}
}

func TestMultipartWriteToDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = w.Write(multipartBody(dicomParts(r.Host)))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	dir := t.TempDir()
	files, err := mp.WriteToDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v, want 2", files)
	}
	// First file named by the Content-Location UID, second by index.
	if filepath.Base(files[0]) != "1.2.840.777.dcm" {
		t.Errorf("file[0] = %q, want 1.2.840.777.dcm", filepath.Base(files[0]))
	}
	if filepath.Base(files[1]) != "part-000002.dcm" {
		t.Errorf("file[1] = %q, want part-000002.dcm", filepath.Base(files[1]))
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != dicomData1 {
		t.Errorf("file content = %q, want %q", b, dicomData1)
	}
}

// TestMultipartSinglePartTolerance verifies the fallback for servers that
// answer instance retrieval with a bare application/dicom body.
func TestMultipartSinglePartTolerance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dicom")
		_, _ = w.Write([]byte(dicomData1))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	mp, err := c.RetrieveInstance(context.Background(), "1.2.3", "1.2.4", "1.2.5")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	parts, err := mp.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || string(parts[0]) != dicomData1 {
		t.Errorf("parts = %v", parts)
	}
	if _, err := mp.Next(); err != io.EOF {
		t.Errorf("second Next err = %v, want io.EOF", err)
	}
}

// TestMultipartMissingBoundary guards the explicit error for a malformed
// multipart response.
func TestMultipartMissingBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; type="application/dicom"`)
		_, _ = w.Write([]byte("garbage"))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	_, err := c.RetrieveStudy(context.Background(), "1.2.3")
	if err == nil {
		t.Fatal("expected error for multipart without boundary")
	}
}
