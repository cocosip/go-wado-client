package wadors

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocosip/go-wado-client"
)

// TestResourceURLsAndAccept verifies the core URL contract: an arbitrary
// opaque prefix configured at construction plus the fixed standard resource
// path appended by the library.
func TestResourceURLsAndAccept(t *testing.T) {
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.accept, got.query = r.URL.Path, r.Header.Get("Accept"), r.URL.Query()
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = w.Write(multipartBody(dicomParts(r.Host)))
	}))
	defer srv.Close()

	const prefix = "/api/wado/H0001/RIS" // deployment-specific, opaque to the library
	c, err := New(srv.URL + prefix)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cases := []struct {
		name       string
		call       func() error
		wantPath   string
		wantAccept string
	}{
		{
			name: "study",
			call: func() error {
				mp, err := c.RetrieveStudy(ctx, "1.2.840.113619")
				if err != nil {
					return err
				}
				return mp.Close()
			},
			wantPath:   prefix + "/studies/1.2.840.113619",
			wantAccept: `multipart/related; type="application/dicom"`,
		},
		{
			name: "series",
			call: func() error {
				mp, err := c.RetrieveSeries(ctx, "1.2.840.113619", "1.2.840.4")
				if err != nil {
					return err
				}
				return mp.Close()
			},
			wantPath:   prefix + "/studies/1.2.840.113619/series/1.2.840.4",
			wantAccept: `multipart/related; type="application/dicom"`,
		},
		{
			name: "instance with transfer syntax",
			call: func() error {
				mp, err := c.RetrieveInstance(ctx, "1.2.840.113619", "1.2.840.4", "1.2.840.5",
					WithTransferSyntax("1.2.840.10008.1.2.1"))
				if err != nil {
					return err
				}
				return mp.Close()
			},
			wantPath:   prefix + "/studies/1.2.840.113619/series/1.2.840.4/instances/1.2.840.5",
			wantAccept: `multipart/related; type="application/dicom"; transfer-syntax=1.2.840.10008.1.2.1`,
		},
		{
			name: "frames sorted and deduplicated",
			call: func() error {
				mp, err := c.RetrieveFrames(ctx, "1.2.840.113619", "1.2.840.4", "1.2.840.5",
					[]int{5, 1, 1, 3})
				if err != nil {
					return err
				}
				return mp.Close()
			},
			wantPath:   prefix + "/studies/1.2.840.113619/series/1.2.840.4/instances/1.2.840.5/frames/1,3,5",
			wantAccept: `multipart/related; type="application/octet-stream"`,
		},
	}
	for _, tc := range cases {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.path != tc.wantPath {
			t.Errorf("%s: path = %q, want %q", tc.name, got.path, tc.wantPath)
		}
		if got.accept != tc.wantAccept {
			t.Errorf("%s: Accept = %q, want %q", tc.name, got.accept, tc.wantAccept)
		}
	}
}

func TestUIDValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = w.Write(multipartBody(dicomParts(r.Host)))
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Injection attempt is rejected locally.
	if _, err := c.RetrieveStudy(ctx, "../evil"); !errors.Is(err, wado.ErrInvalidUID) {
		t.Errorf("err = %v, want ErrInvalidUID", err)
	}
	if _, err := c.RetrieveFrames(ctx, "1.2.3", "1.2.4", "1.2.5", []int{0}); err == nil {
		t.Error("frame number < 1 expected error")
	}
	if _, err := c.RetrieveFrames(ctx, "1.2.3", "1.2.4", "1.2.5", nil); err == nil {
		t.Error("empty frame list expected error")
	}

	// WithLenientUID disables the whitelist (private gateway escape hatch).
	lenient, err := New(srv.URL+"/dicomweb", wado.WithLenientUID())
	if err != nil {
		t.Fatal(err)
	}
	mp, err := lenient.RetrieveStudy(ctx, "../evil")
	if err != nil {
		t.Fatalf("lenient mode: %v", err)
	}
	_ = mp.Close()
}

func TestStatusErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unknown study"))
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RetrieveStudy(context.Background(), "1.2.840.1")
	var se *wado.StatusError
	if !errors.As(err, &se) || !se.IsNotFound() {
		t.Fatalf("err = %v, want *StatusError with IsNotFound", err)
	}
}

func TestFork(t *testing.T) {
	c, err := New("https://h.example.com/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Fork("https://h2.example.com/api/wado/H2")
	if err != nil {
		t.Fatal(err)
	}
	if f.BaseURL() != "https://h2.example.com/api/wado/H2" {
		t.Errorf("BaseURL = %q", f.BaseURL())
	}
	if _, err := c.Fork("not-a-url"); err == nil {
		t.Error("Fork with invalid URL expected error")
	}
}
