package wadouri

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocosip/go-wado-client"
)

// TestNegativeRowsColumnsRejected is the regression for the pairing check
// using != 0 while the encoder used > 0: a negative value passed validation and
// was then dropped, emitting exactly the one-sided viewport the pair rule
// forbids (or no viewport at all).
func TestNegativeRowsColumnsRejected(t *testing.T) {
	srv := newRetrieveServer(t, "image/jpeg")
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		rows    int
		columns int
	}{
		{"negative rows with columns", -1, 512},
		{"rows with negative columns", 512, -1},
		{"both negative", -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Retrieve(context.Background(), Request{
				StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
				ContentType: testContentTypeJPEG, Rows: tc.rows, Columns: tc.columns,
			})
			if !errors.Is(err, wado.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}

	// The one-sided rule still holds.
	if _, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		ContentType: testContentTypeJPEG, Rows: 512,
	}); !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("rows without columns: err = %v, want ErrInvalidRequest", err)
	}
}

// TestNonFiniteWindowRejected guards the NaN/Inf window values that used to
// reach the wire verbatim.
func TestNonFiniteWindowRejected(t *testing.T) {
	srv := newRetrieveServer(t, "image/jpeg")
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	nan, posInf, negInf := math.NaN(), math.Inf(1), math.Inf(-1)
	cases := map[string][2]float64{
		"NaN center":  {nan, 400},
		"NaN width":   {40, nan},
		"+Inf center": {posInf, 400},
		"-Inf width":  {40, negInf},
		"both NaN":    {nan, nan},
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			center, width := w[0], w[1]
			_, err := c.Retrieve(context.Background(), Request{
				StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
				ContentType: testContentTypeJPEG, WindowCenter: &center, WindowWidth: &width,
			})
			if !errors.Is(err, wado.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}

	// Finite values still pass.
	center, width := 40.0, 400.0
	resp, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		ContentType: testContentTypeJPEG, WindowCenter: &center, WindowWidth: &width,
	})
	if err != nil {
		t.Fatalf("finite window rejected: %v", err)
	}
	_ = resp.Close()
}

// TestExtraOverridesStandardParameters is the regression for Extra appending a
// duplicate instead of overriding, which left the server to pick between two
// values for the same key.
func TestExtraOverridesStandardParameters(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/dicom")
		_, _ = w.Write([]byte("d"))
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		ContentType: testContentTypeJPEG,
		Extra: url.Values{
			"contentType": {"image/png"},
			"studyUID":    {"9.9.9"},
			"privateKey":  {"v1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Close()

	parsed, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed["contentType"]; len(got) != 1 || got[0] != "image/png" {
		t.Errorf("contentType = %v, want exactly [image/png]", got)
	}
	if got := parsed["studyUID"]; len(got) != 1 || got[0] != "9.9.9" {
		t.Errorf("studyUID = %v, want exactly [9.9.9]", got)
	}
	if got := parsed["privateKey"]; len(got) != 1 || got[0] != "v1" {
		t.Errorf("privateKey = %v, want [v1]", got)
	}
	// The standard parameters that were not overridden survive.
	if got := parsed["seriesUID"]; len(got) != 1 || got[0] != "1.2.4" {
		t.Errorf("seriesUID = %v, want [1.2.4]", got)
	}
	if got := parsed["requestType"]; len(got) != 1 || got[0] != testRequestTypeWADO {
		t.Errorf("requestType = %v, want [WADO]", got)
	}
}

// TestTransferSyntaxRequiresDICOM guards the asymmetric rule: transferSyntax
// was emitted with a rendered contentType although §9.4.1.2.2 defines it for
// the DICOM instance transaction only.
func TestTransferSyntaxRequiresDICOM(t *testing.T) {
	srv := newRetrieveServer(t, "image/jpeg")
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Retrieve(context.Background(), Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		ContentType: testContentTypeJPEG, TransferSyntax: testTransferSyntaxEVRLE,
	}); !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}

	// With application/dicom it is valid, including the "*" wildcard.
	for _, ts := range []string{"1.2.840.10008.1.2.1", "*"} {
		resp, err := c.Retrieve(context.Background(), Request{
			StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			TransferSyntax: ts,
		})
		if err != nil {
			t.Errorf("transferSyntax %q rejected for application/dicom: %v", ts, err)
			continue
		}
		_ = resp.Close()
	}
}

// TestIsDICOMExcludesJSONAndXML guards the prefix match that reported
// application/dicom+json as a DICOM file.
func TestIsDICOMExcludesJSONAndXML(t *testing.T) {
	cases := map[string]bool{
		"application/dicom": true,
		"application/dicom; transfer-syntax=1.2.840.10008.1.2.1": true,
		"APPLICATION/DICOM":      true,
		"application/dicom+json": false,
		"application/dicom+xml":  false,
		"image/jpeg":             false,
		"":                       false,
	}
	for ct, want := range cases {
		r := &Response{ContentType: ct}
		if got := r.IsDICOM(); got != want {
			t.Errorf("IsDICOM(%q) = %v, want %v", ct, got, want)
		}
	}
}

// TestRenderRangeChecksAreReached pins that the rendered value-range checks are
// actually exercised: the pre-existing "region out of range" / "quality too
// large" / "unknown annotation" cases omitted ContentType, so they were caught
// by the DICOM early-return and never reached the range checks at all.
func TestRenderRangeChecksAreReached(t *testing.T) {
	cases := map[string]Request{
		"region out of range": {
			ContentType: testContentTypeJPEG, Region: &[4]float64{0.5, 0.1, 0.4, 0.9},
		},
		"region above 1": {
			ContentType: testContentTypeJPEG, Region: &[4]float64{0.1, 0.1, 1.5, 0.9},
		},
		"quality too large": {
			ContentType: testContentTypeJPEG, ImageQuality: 101,
		},
		"quality negative": {
			ContentType: testContentTypeJPEG, ImageQuality: -1,
		},
		"unknown annotation": {
			ContentType: testContentTypeJPEG, Annotation: []string{"bogus"},
		},
		"frame number negative": {
			ContentType: testContentTypeJPEG, FrameNumber: -1,
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			req.StudyUID, req.SeriesUID, req.ObjectUID = testStudyUID, testSeriesUID, testObjectUID
			if err := req.validate(strictCheck); !errors.Is(err, wado.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestPresentationUIDEncoding covers the two presentation parameters that the
// existing encoding test never set.
func TestPresentationUIDEncoding(t *testing.T) {
	req := Request{
		StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		ContentType:           testContentTypeJPEG,
		PresentationUID:       "1.2.840.10008.5.1.4.1.1.11.1",
		PresentationSeriesUID: "1.2.840.10008.5.1.4.1.1.11",
	}
	q := req.query(false)
	if got := q.Get("presentationUID"); got != req.PresentationUID {
		t.Errorf("presentationUID = %q, want %q", got, req.PresentationUID)
	}
	if got := q.Get("presentationSeriesUID"); got != req.PresentationSeriesUID {
		t.Errorf("presentationSeriesUID = %q, want %q", got, req.PresentationSeriesUID)
	}
}

// TestCapabilitiesOnURIEndpoint covers the delegation to Service.Capabilities.
func TestCapabilitiesOnURIEndpoint(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/wado-uri")
	if err != nil {
		t.Fatal(err)
	}
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodOptions {
		t.Errorf("method = %s, want OPTIONS", method)
	}
	if path != "/wado-uri" {
		t.Errorf("path = %q, want /wado-uri", path)
	}
	if !caps.Supports("GET") {
		t.Errorf("Allow = %v, want GET", caps.Allow)
	}
}

// newRetrieveServer serves a minimal successful WADO-URI reply.
func newRetrieveServer(t *testing.T, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var _ = strings.Contains
