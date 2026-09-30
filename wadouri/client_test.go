package wadouri

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

// Conventional UID triple of the fake gateway, shared by the test files.
const (
	testStudyUID        = "1.2.3"
	testSeriesUID       = "1.2.4"
	testObjectUID       = "1.2.5"
	testPresentationUID = "1.2.9"
)

func TestRetrieveIntegration(t *testing.T) {
	var gotQuery url.Values
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/wado/H0001/RIS/wado-uri" {
			t.Errorf("path = %q, want the configured endpoint", r.URL.Path)
		}
		gotQuery, gotAccept = r.URL.Query(), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/dicom")
		_, _ = w.Write([]byte("DICOMFILE"))
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/api/wado/H0001/RIS/wado-uri")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Retrieve(context.Background(), Request{
		StudyUID:  testStudyUID,
		SeriesUID: testSeriesUID,
		ObjectUID: testObjectUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Close() }()

	if gotQuery.Get("requestType") != "WADO" {
		t.Errorf("requestType = %q", gotQuery.Get("requestType"))
	}
	if gotQuery.Get("studyUID") != testStudyUID || gotQuery.Get("seriesUID") != testSeriesUID || gotQuery.Get("objectUID") != testObjectUID {
		t.Errorf("UID query = %v", gotQuery)
	}
	if gotAccept != "application/dicom" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if !resp.IsDICOM() {
		t.Errorf("IsDICOM = false, ContentType = %q", resp.ContentType)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "DICOMFILE" {
		t.Errorf("body = %q", b)
	}
}

func TestRetrieveModernAnonymize(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/dicom")
	}))
	defer srv.Close()

	c, _ := New(srv.URL+"/wado", wado.WithModernParamNames())
	resp, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, Anonymize: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Close() }()
	if gotQuery.Get("anonymize") != anonymizeEnabled {
		t.Errorf("anonymize = %q", gotQuery.Get("anonymize"))
	}
	if _, ok := gotQuery["anonymity"]; ok {
		t.Error("anonymity must not appear in modern mode")
	}
}

func TestRetrieveLocalValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request must not reach the server when local validation fails")
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/wado")
	_, err := c.Retrieve(context.Background(), Request{
		StudyUID: "../evil", SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
	})
	if !errors.Is(err, wado.ErrInvalidUID) {
		t.Errorf("err = %v, want ErrInvalidUID", err)
	}

	wc := 40.0
	_, err = c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, WindowCenter: &wc,
	})
	if !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestRetrieveNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/wado")
	_, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
	})
	var se *wado.StatusError
	if !errors.As(err, &se) || !se.IsNotFound() {
		t.Fatalf("err = %v, want *StatusError with IsNotFound", err)
	}
}

func TestForkAndEndpoint(t *testing.T) {
	c, err := New("https://h.example.com/api/wado/H1/wado-uri")
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint() != "https://h.example.com/api/wado/H1/wado-uri" {
		t.Errorf("Endpoint = %q", c.Endpoint())
	}
	f, err := c.Fork("https://h2.example.com/wado", wado.WithBasicAuth("u", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Endpoint() != "https://h2.example.com/wado" {
		t.Errorf("forked Endpoint = %q", f.Endpoint())
	}
}
