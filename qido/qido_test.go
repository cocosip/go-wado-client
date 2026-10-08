package qido

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocosip/go-dicom/pkg/dicom/tag"

	"github.com/cocosip/go-wado-client"
)

const (
	testPatientID = "11235813"
	attrPatientID  = "PatientID"
	testStudyUID = "1.2.3"
	fieldAttributeID = "AttributeID"
)

func u64(v uint64) *uint64 { return &v }
func bp(b bool) *bool      { return &b }

// captured records the aspects of a request the fake server needs to expose
// to assertions.
type captured struct {
	path     string
	accept   string
	rawQuery string
	query    url.Values
	method   string
}

func newCaptureServer(t *testing.T, ct string, body []byte, extraHeaders map[string]string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.accept = r.Header.Get("Accept")
		got.rawQuery = r.URL.RawQuery
		got.query = r.URL.Query()
		got.method = r.Method
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestEncodeQuery(t *testing.T) {
	tests := []struct {
		name string
		q    Query
		want string
	}{
		{
			name: "empty query",
			q:    Query{},
			want: "",
		},
		{
			name: "match by keyword, includefield list, limit",
			q: Query{
				Match:         []Match{{Attribute: attrPatientID, Value: testPatientID}},
				IncludeFields: []string{"00081048", "00081060"},
				Limit:         u64(25),
			},
			want: "PatientID=" + testPatientID + "&includefield=00081048,00081060&limit=25",
		},
		{
			name: "match by tag with date range",
			q: Query{
				Match: []Match{{Attribute: "00080020", Value: "20130509-20130510"}},
			},
			want: "00080020=20130509-20130510",
		},
		{
			name: "open-ended range and wildcard",
			q: Query{
				Match: []Match{{Attribute: "StudyDate", Value: "20130509-"}, {Attribute: "PatientName", Value: "SMITH^JOHN*"}},
			},
			want: "StudyDate=20130509-&PatientName=SMITH%5EJOHN%2A",
		},
		{
			name: "uid list matching keeps the comma bare",
			q: Query{
				Match: []Match{{Attribute: "0020000E", Values: []string{testStudyUID, "1.2.4"}}},
			},
			want: "0020000E=1.2.3,1.2.4",
		},
		{
			name: "PS3.18 mandated percent encoding and non-ASCII",
			q: Query{
				Match: []Match{{Attribute: "StudyDescription", Value: "a#b=c&d[e]f Hôpital"}},
			},
			want: "StudyDescription=a%23b%3Dc%26d%5Be%5Df+H%C3%B4pital",
		},
		{
			name: "dotted sequence path",
			q: Query{
				Match: []Match{{Attribute: "00101002.00100020", Value: "11235813"}},
			},
			want: "00101002.00100020=11235813",
		},
		{
			name: "common parameters, tri-state booleans, offset",
			q: Query{
				FuzzyMatching:         bp(true),
				Offset:                25,
				EmptyValueMatching:    bp(false),
				MultipleValueMatching: bp(true),
			},
			want: "fuzzymatching=true&offset=25&emptyvaluematching=false&multiplevaluematching=true",
		},
		{
			name: "orderby descending and aetitle list",
			q: Query{
				OrderBy: []string{"-StudyDate", "StudyTime"},
				AETitle: []string{"AE1", "AE2"},
			},
			want: "orderby=-StudyDate,StudyTime&aetitle=AE1,AE2",
		},
		{
			name: "includefield all",
			q: Query{
				IncludeFields: []string{"all"},
			},
			want: "includefield=all",
		},
		{
			name: "extra overrides a standard parameter and appends private ones",
			q: Query{
				Match: []Match{{Attribute: attrPatientID, Value: "x"}},
				Limit: u64(10),
				Extra: url.Values{"limit": []string{"5"}, "privateKey": []string{"v 1"}},
			},
			want: "PatientID=x&limit=5&privateKey=v+1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.q.validate(func(string, string) error { return nil }); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if got := tt.q.encodeQuery(); got != tt.want {
				t.Errorf("encodeQuery = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryValidation(t *testing.T) {
	// A server whose handler fails the test: local validation must reject
	// before any request is sent.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the server despite local validation failure")
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tests := []struct {
		name   string
		search func() error
		field  string
	}{
		{
			name: "attribute id with whitespace",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Match: []Match{{Attribute: "Patient ID", Value: "x"}}})
				return err
			},
			field: fieldAttributeID,
		},
		{
			name: "attribute id path traversal",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Match: []Match{{Attribute: "../studies", Value: "x"}}})
				return err
			},
			field: fieldAttributeID,
		},
		{
			name: "match with both value forms",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Match: []Match{{
					Attribute: "StudyInstanceUID", Value: testStudyUID, Values: []string{"1.2.4"}}}})
				return err
			},
			field: "StudyInstanceUID",
		},
		{
			name: "match without value",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Match: []Match{{Attribute: attrPatientID}}})
				return err
			},
			field: "PatientID",
		},
		{
			name: "uid list with non-UID entry",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Match: []Match{{
					Attribute: "SeriesInstanceUID", Values: []string{"1.2.3", "not a uid"}}}})
				return err
			},
			field: "SeriesInstanceUID",
		},
		{
			name: "limit zero",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{Limit: u64(0)})
				return err
			},
			field: "Limit",
		},
		{
			name: "orderby invalid attribute",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{OrderBy: []string{"-Study Date"}})
				return err
			},
			field: fieldAttributeID,
		},
		{
			name: "aetitle non-ASCII",
			search: func() error {
				_, err := c.SearchStudies(ctx, Query{AETitle: []string{"HÖSP"}})
				return err
			},
			field: "AETitle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.search()
			var re *wado.RequestError
			var ue *wado.UIDError
			switch {
			case errors.As(err, &re):
				if re.Field != tt.field {
					t.Errorf("field = %q, want %q (err: %v)", re.Field, tt.field, err)
				}
			case errors.As(err, &ue):
				if ue.Field != tt.field {
					t.Errorf("field = %q, want %q (err: %v)", ue.Field, tt.field, err)
				}
			default:
				t.Fatalf("err = %v, want *wado.RequestError or *wado.UIDError", err)
			}
		})
	}
}

// studyJSON is one study search result item.
const studyJSON = `{"0020000D":{"vr":"UI","Value":["1.2.3"]},"00100020":{"vr":"LO","Value":["PID1"]}}`
const studyJSON2 = `{"0020000D":{"vr":"UI","Value":["1.2.9"]},"00100020":{"vr":"LO","Value":["PID2"]}}`

func TestSearchStudies(t *testing.T) {
	srv, got := newCaptureServer(t, "application/dicom+json",
		[]byte("["+studyJSON+","+studyJSON2+"]"), nil)
	c, err := New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchStudies(context.Background(), Query{
		Match:         []Match{{Attribute: attrPatientID, Value: testPatientID}},
		IncludeFields: []string{"00081048", "00081060"},
		FuzzyMatching: bp(false),
		Limit:         u64(25),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet {
		t.Errorf("method = %s, want GET", got.method)
	}
	if got.path != "/dicomweb/studies" {
		t.Errorf("path = %s, want /dicomweb/studies", got.path)
	}
	if got.accept != MediaTypeDICOMJSON {
		t.Errorf("accept = %s, want %s", got.accept, MediaTypeDICOMJSON)
	}
	wantRaw := "PatientID=11235813&includefield=00081048,00081060&fuzzymatching=false&limit=25"
	if got.rawQuery != wantRaw {
		t.Errorf("rawQuery = %q, want %q", got.rawQuery, wantRaw)
	}
	if len(res.Datasets) != 2 {
		t.Fatalf("datasets = %d, want 2", len(res.Datasets))
	}
	for i, want := range []string{"1.2.3", "1.2.9"} {
		uid, ok := res.Datasets[i].GetString(tag.StudyInstanceUID)
		if !ok || uid != want {
			t.Errorf("datasets[%d] StudyInstanceUID = %q, %v; want %q", i, uid, ok, want)
		}
	}
	if _, ok := res.AdditionalResults(); ok {
		t.Error("AdditionalResults reported without a Warning 299 header")
	}
}

func TestSearchResources(t *testing.T) {
	tests := []struct {
		name   string
		search func(c *Client) error
		path   string
		uids   []string
	}{
		{
			name: "study series",
			search: func(c *Client) error {
				_, err := c.SearchSeries(context.Background(), testStudyUID, Query{})
				return err
			},
			path: "/dicomweb/studies/" + testStudyUID + "/series",
		},
		{
			name: "study instances",
			search: func(c *Client) error {
				_, err := c.SearchStudyInstances(context.Background(), testStudyUID, Query{})
				return err
			},
			path: "/dicomweb/studies/" + testStudyUID + "/instances",
		},
		{
			name: "series instances",
			search: func(c *Client) error {
				_, err := c.SearchSeriesInstances(context.Background(), testStudyUID, "1.2.4", Query{})
				return err
			},
			path: "/dicomweb/studies/" + testStudyUID + "/series/1.2.4/instances",
		},
		{
			name: "all series",
			search: func(c *Client) error {
				_, err := c.SearchAllSeries(context.Background(), Query{})
				return err
			},
			path: "/dicomweb/series",
		},
		{
			name: "all instances",
			search: func(c *Client) error {
				_, err := c.SearchAllInstances(context.Background(), Query{})
				return err
			},
			path: "/dicomweb/instances",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newCaptureServer(t, "application/dicom+json", []byte("[]"), nil)
			c, err := New(srv.URL + "/dicomweb")
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.search(c); err != nil {
				t.Fatal(err)
			}
			if got.path != tt.path {
				t.Errorf("path = %s, want %s", got.path, tt.path)
			}
		})
	}
}

func TestSearchInvalidScopedUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request reached the server despite an invalid UID")
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchSeries(context.Background(), testStudyUID + "/../x", Query{}); !errors.Is(err, wado.ErrInvalidUID) {
		t.Fatalf("err = %v, want ErrInvalidUID", err)
	}
}

func TestSearch204NoMatches(t *testing.T) {
	srv, _ := newCaptureServer(t, "application/dicom+json", nil, nil)
	// A 204 carries no body; rewrite the capture server's fixed status via a
	// dedicated server instead.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv2.Close()
	_ = srv

	c, err := New(srv2.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchStudies(context.Background(), Query{})
	if err != nil {
		t.Fatalf("204 must not be an error: %v", err)
	}
	if res.Datasets == nil || len(res.Datasets) != 0 {
		t.Errorf("datasets = %v, want empty non-nil", res.Datasets)
	}

	// The stream form is equally exhausted.
	rs, err := c.SearchStudiesStream(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	if _, err := rs.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("stream Next err = %v, want io.EOF", err)
	}
}

func TestSearchWarning299(t *testing.T) {
	srv, _ := newCaptureServer(t, "application/dicom+json", []byte("["+studyJSON+"]"),
		map[string]string{"Warning": `299 https://srv.example/dicomweb: "There are 42 additional results that can be requested"`})
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchStudies(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := res.AdditionalResults()
	if !ok || n != 42 {
		t.Errorf("AdditionalResults = (%d, %v), want (42, true)", n, ok)
	}
}

func TestSearchBareObjectResponse(t *testing.T) {
	// Older servers answer a single bare dataset object instead of an array.
	srv, _ := newCaptureServer(t, "application/dicom+json", []byte(studyJSON), nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchSeries(context.Background(), testStudyUID, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Datasets) != 1 {
		t.Fatalf("datasets = %d, want 1", len(res.Datasets))
	}
}

func TestSearchMultipartWrapped(t *testing.T) {
	// Tolerance: some servers wrap dicom+json in multipart/related (one
	// dataset per part); array parts contribute their elements.
	body := strings.Join([]string{
		"--BNDRY",
		"Content-Type: application/dicom+json",
		"",
		"[" + studyJSON + "," + studyJSON2 + "]",
		"--BNDRY",
		"Content-Type: application/dicom+json",
		"",
		studyJSON,
		"--BNDRY--",
		"",
	}, "\r\n")
	srv, _ := newCaptureServer(t, `multipart/related; type="application/dicom+json"; boundary=BNDRY`, []byte(body), nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.SearchAllSeries(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Datasets) != 3 {
		t.Fatalf("datasets = %d, want 3", len(res.Datasets))
	}
}

func TestSearchStreamMatchesAggregate(t *testing.T) {
	srv, _ := newCaptureServer(t, "application/dicom+json",
		[]byte("["+studyJSON+","+studyJSON2+"]"), nil)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	agg, err := c.SearchStudies(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := c.SearchStudiesStream(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var streamed []string
	for {
		ds, err := rs.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		uid, _ := ds.GetString(tag.StudyInstanceUID)
		streamed = append(streamed, uid)
	}
	var aggregated []string
	for _, ds := range agg.Datasets {
		uid, _ := ds.GetString(tag.StudyInstanceUID)
		aggregated = append(aggregated, uid)
	}
	if strings.Join(streamed, ",") != strings.Join(aggregated, ",") {
		t.Errorf("stream %v != aggregate %v", streamed, aggregated)
	}
}

func TestSearchXMLGuard(t *testing.T) {
	body := "--BNDRY\r\nContent-Type: application/dicom+xml\r\n\r\n<xml/>\r\n--BNDRY--\r\n"
	for _, tt := range []struct{ name, ct string }{
		{"single part", "application/dicom+xml"},
		{"multipart", `multipart/related; type="application/dicom+xml"; boundary=BNDRY`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newCaptureServer(t, tt.ct, []byte(body), nil)
			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.SearchStudies(context.Background(), Query{}, WithAccept(tt.ct))
			if err == nil || !strings.Contains(err.Error(), "dicom+xml") {
				t.Fatalf("err = %v, want explicit dicom+xml guard error", err)
			}
		})
	}
}

func TestNewWithCoreAndFork(t *testing.T) {
	core, err := wado.NewCore()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWithCore(core, "https://gw.example.com/api")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != "https://gw.example.com/api" {
		t.Errorf("BaseURL = %q", c.BaseURL())
	}
	f, err := c.Fork("https://gw.example.com/api/H0001/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	if f.BaseURL() != "https://gw.example.com/api/H0001/dicomweb" {
		t.Errorf("Fork BaseURL = %q", f.BaseURL())
	}
	if _, err := NewWithCore(nil, "https://gw.example.com"); err == nil {
		t.Error("NewWithCore(nil) must fail")
	}
}
