package qido

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocosip/go-wado-client"
)

// TestEmptyValueMatchingIsReachable is the regression for §8.3.4.5 being
// unreachable: an empty match value was rejected locally, so the exported
// EmptyValueMatching toggle could never be exercised.
func TestEmptyValueMatchingIsReachable(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json", nil, nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchStudies(context.Background(), Query{
		Match:              []Match{{Attribute: attrPatientName, Value: ""}},
		EmptyValueMatching: Bool(true),
	})
	if err != nil {
		t.Fatalf("empty-value match rejected: %v", err)
	}
	if res == nil {
		t.Fatal("res = nil")
	}
	if got.rawQuery != "PatientName=&emptyvaluematching=true" {
		t.Errorf("rawQuery = %q, want PatientName=&emptyvaluematching=true", got.rawQuery)
	}

	// Without the toggle the request would match nothing on a conformant
	// server, so it stays rejected.
	if _, err := c.SearchStudies(context.Background(), Query{
		Match: []Match{{Attribute: attrPatientName, Value: ""}},
	}); !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("empty value without the toggle: err = %v, want ErrInvalidRequest", err)
	}
}

// TestMultipleValueMatchingForNonUIDAttributes is the regression for §8.3.4.6:
// a value list on a non-UID attribute was forced through the UID whitelist, so
// Multiple Value Matching was unusable for anything but UIDs.
func TestMultipleValueMatchingForNonUIDAttributes(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json", nil, nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		attr   string
		values []string
		want   string
	}{
		{"Modality", []string{"CT", "MR"}, "Modality=CT,MR"},
		{attrPatientName, []string{"SMITH^JOHN", "DOE^JANE"}, "PatientName=SMITH%5EJOHN,DOE%5EJANE"},
		{"BodyPartExamined", []string{"HEAD"}, "BodyPartExamined=HEAD"},
		// UID attributes keep the local whitelist.
		{"0020000E", []string{testStudyUID, testSeriesUID}, "0020000E=" + testStudyUID + "," + testSeriesUID},
	}
	for _, tc := range cases {
		t.Run(tc.attr, func(t *testing.T) {
			if _, err := c.SearchStudies(context.Background(), Query{
				Match:                 []Match{{Attribute: tc.attr, Values: tc.values}},
				MultipleValueMatching: Bool(true),
			}); err != nil {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(got.rawQuery, tc.want) {
				t.Errorf("rawQuery = %q, want it to contain %q", got.rawQuery, tc.want)
			}
		})
	}

	// A non-UID list must not silently accept an empty element.
	if _, err := c.SearchStudies(context.Background(), Query{
		Match: []Match{{Attribute: "Modality", Values: []string{"CT", ""}}},
	}); !errors.Is(err, wado.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest for an empty list element", err)
	}

	// A UID attribute must still reject a non-UID value.
	if _, err := c.SearchStudies(context.Background(), Query{
		Match: []Match{{Attribute: attrStudyUID, Values: []string{"not-a-uid"}}},
	}); !errors.Is(err, wado.ErrInvalidUID) {
		t.Errorf("err = %v, want ErrInvalidUID", err)
	}
}

// TestWarning299InCommaJoinedList guards the RFC 9110 list form: a 299 that is
// not the first warning-value of a coalesced header used to be missed.
func TestWarning299InCommaJoinedList(t *testing.T) {
	cases := []struct {
		name   string
		header string
		wantN  int64
		wantOK bool
	}{
		{
			name:   "299 first",
			header: `299 gw "There are 42 additional results that can be requested", 110 gw "Response is stale"`,
			wantN:  42, wantOK: true,
		},
		{
			name:   "299 second",
			header: `110 gw "Response is stale", 299 gw "There are 42 additional results that can be requested"`,
			wantN:  42, wantOK: true,
		},
		{
			name:   "299 third of four",
			header: `110 gw "a", 112 gw "b", 299 gw "There are 7 additional results that can be requested", 214 gw "c"`,
			wantN:  7, wantOK: true,
		},
		{
			name:   "no 299",
			header: `110 gw "Response is stale"`,
			wantN:  0, wantOK: false,
		},
		{
			name:   "comma inside the quoted text is not a separator",
			header: `299 gw "There are 1,234 additional results that can be requested"`,
			wantN:  234, wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("Warning", tc.header)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.SearchStudies(context.Background(), Query{})
			if err != nil {
				t.Fatal(err)
			}
			n, ok := res.AdditionalResults()
			if ok != tc.wantOK || n != tc.wantN {
				t.Errorf("AdditionalResults() = (%d, %v), want (%d, %v)", n, ok, tc.wantN, tc.wantOK)
			}
		})
	}
}

// TestDecodeErrorIsStickyAndDoesNotLoop guards the stuck-cursor regression: a
// malformed part used to return the identical error forever, so a caller that
// skipped non-EOF errors spun and never reached later parts.
func TestDecodeErrorIsStickyAndDoesNotLoop(t *testing.T) {
	// Part 1 is a truncated JSON array; part 2 is a valid dataset.
	body := "--BNDRY\r\nContent-Type: application/dicom+json\r\n\r\n" +
		`[{"00080018":{"vr":"UI","Value":["1.2.3"]}` + "\r\n" +
		"--BNDRY\r\nContent-Type: application/dicom+json\r\n\r\n" +
		`[{"00080018":{"vr":"UI","Value":["1.2.4"]}}]` + "\r\n" +
		"--BNDRY--\r\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom+json"`)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.SearchStudiesStream(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()

	_, firstErr := rs.Next()
	if firstErr == nil {
		t.Fatal("first Next = nil error, want a decode failure")
	}
	// Every later call must return the same error, deterministically, rather
	// than looping on the same broken cursor.
	for i := 0; i < 3; i++ {
		_, err := rs.Next()
		if err == nil {
			t.Fatalf("Next #%d = nil error after a decode failure", i+2)
		}
		if err.Error() != firstErr.Error() {
			t.Errorf("Next #%d error changed: %v vs %v", i+2, err, firstErr)
		}
	}
}

// TestIncludefieldAllCaseInsensitive guards the literal "all" comparison.
func TestIncludefieldAllCaseInsensitive(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json", nil, nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"all", "ALL", "All"} {
		if _, err := c.SearchStudies(context.Background(), Query{IncludeFields: []string{f}}); err != nil {
			t.Errorf("IncludeFields %q: %v", f, err)
		}
	}
	if !strings.Contains(got.rawQuery, "includefield=all") && !strings.Contains(got.rawQuery, "includefield=ALL") {
		t.Errorf("rawQuery = %q", got.rawQuery)
	}
}

// TestExtraOverridesStandardParameterCaseInsensitively guards the override
// semantics documented on Query.Extra.
func TestExtraOverridesStandardParameterCaseInsensitively(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json", nil, nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	limit := uint64(10)
	if _, err := c.SearchStudies(context.Background(), Query{
		Limit: &limit,
		Extra: map[string][]string{"Limit": {"5"}},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.rawQuery, "limit=10") {
		t.Errorf("rawQuery = %q; the standard limit=10 was not overridden", got.rawQuery)
	}
	if !strings.Contains(got.rawQuery, "Limit=5") {
		t.Errorf("rawQuery = %q, want the Extra value", got.rawQuery)
	}
}

// TestScalarCommaIsEscaped guards that a comma inside an ordinary matching
// value is not emitted as the list separator.
func TestScalarCommaIsEscaped(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json", nil, nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchStudies(context.Background(), Query{
		Match: []Match{{Attribute: attrPatientName, Value: "SMITH,JOHN"}},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.rawQuery, "PatientName=SMITH%2CJOHN") {
		t.Errorf("rawQuery = %q, want the comma percent-encoded", got.rawQuery)
	}
	// A genuine list keeps the bare comma.
	if _, err := c.SearchStudies(context.Background(), Query{
		IncludeFields: []string{fieldTagA, fieldTagB},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.rawQuery, "includefield=00081048,00081060") {
		t.Errorf("rawQuery = %q, want the bare list comma", got.rawQuery)
	}
}

// TestAllStreamVariantsReachTheirPaths covers the five *Stream methods that had
// no test at all.
func TestAllStreamVariantsReachTheirPaths(t *testing.T) {
	cases := []struct {
		name string
		want string
		call func(*Client) error
	}{
		{
			name: "SearchStudiesStream",
			want: "/studies",
			call: func(c *Client) error {
				rs, err := c.SearchStudiesStream(context.Background(), Query{})
				return closeStream(rs, err)
			},
		},
		{
			name: "SearchSeriesStream",
			want: "/studies/1.2.3/series",
			call: func(c *Client) error {
				rs, err := c.SearchSeriesStream(context.Background(), "1.2.3", Query{})
				return closeStream(rs, err)
			},
		},
		{
			name: "SearchStudyInstancesStream",
			want: "/studies/1.2.3/instances",
			call: func(c *Client) error {
				rs, err := c.SearchStudyInstancesStream(context.Background(), "1.2.3", Query{})
				return closeStream(rs, err)
			},
		},
		{
			name: "SearchSeriesInstancesStream",
			want: "/studies/1.2.3/series/1.2.4/instances",
			call: func(c *Client) error {
				rs, err := c.SearchSeriesInstancesStream(context.Background(), "1.2.3", "1.2.4", Query{})
				return closeStream(rs, err)
			},
		},
		{
			name: "SearchAllSeriesStream",
			want: "/series",
			call: func(c *Client) error {
				rs, err := c.SearchAllSeriesStream(context.Background(), Query{})
				return closeStream(rs, err)
			},
		},
		{
			name: "SearchAllInstancesStream",
			want: "/instances",
			call: func(c *Client) error {
				rs, err := c.SearchAllInstancesStream(context.Background(), Query{})
				return closeStream(rs, err)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := newCaptureServer(t, "application/dicom+json", []byte(`[]`), nil)
			defer srv.Close()
			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.path != tc.want {
				t.Errorf("path = %q, want %q", got.path, tc.want)
			}
		})
	}
}

// TestCapabilitiesOnQidoClient covers the previously untested delegation.
func TestCapabilitiesOnQidoClient(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(srv.URL + "/dicomweb")
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
	if path != "/dicomweb" {
		t.Errorf("path = %q, want /dicomweb", path)
	}
	if !caps.Supports("GET") {
		t.Errorf("Allow = %v, want GET", caps.Allow)
	}
}

// TestBoolHelper covers the exported tri-state helper.
func TestBoolHelper(t *testing.T) {
	tr, fa := Bool(true), Bool(false)
	if tr == nil || fa == nil {
		t.Fatal("Bool returned nil")
	}
	if !*tr || *fa {
		t.Errorf("Bool values = %v/%v, want true/false", *tr, *fa)
	}
}

// TestResultsStreamAdditionalResults covers the streaming accessor.
func TestResultsStreamAdditionalResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Warning", `299 gw "There are 3 additional results that can be requested"`)
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.SearchStudiesStream(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	n, ok := rs.AdditionalResults()
	if !ok || n != 3 {
		t.Errorf("AdditionalResults() = (%d, %v), want (3, true)", n, ok)
	}
	if rs.Header().Get("Warning") == "" {
		t.Error("Header() lost the Warning header")
	}
}

func closeStream(rs *ResultsStream, err error) error {
	if err != nil {
		return err
	}
	return rs.Close()
}
