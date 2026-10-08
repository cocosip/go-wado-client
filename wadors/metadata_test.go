package wadors

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cocosip/go-dicom/pkg/dicom/serialization"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
)

// testSOPUID is the SOP Instance UID carried by every fake metadata payload.
const testSOPUID = "1.2.840.777"

// resolvedBulk7FE00010 is the absolute form of "./bulkdata/7FE00010"
// resolved against the metadata request URL used in the tests.
const resolvedBulk7FE00010 = "https://h/dicomweb/studies/1.2/bulkdata/7FE00010"

const studyMetaJSON = `[` +
	`{"00080018":{"vr":"UI","Value":["1.2.840.777"]},` +
	`"00100010":{"vr":"PN","Value":[{"Alphabetic":"DOE^JOHN"}]},` +
	`"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/00282000"}}]`

func TestStudyMetadata(t *testing.T) {
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.accept = r.URL.Path, r.Header.Get("Accept")
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(studyMetaJSON))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/api/x")
	dss, err := c.StudyMetadata(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/x/studies/1.2.840.1/metadata" {
		t.Errorf("path = %q", got.path)
	}
	if got.accept != mediaTypeDICOMJSON {
		t.Errorf("Accept = %q", got.accept)
	}
	if len(dss) != 1 {
		t.Fatalf("datasets = %d, want 1", len(dss))
	}

	sop, ok := dss[0].GetString(tag.SOPInstanceUID)
	if !ok {
		t.Fatal("SOPInstanceUID missing")
	}
	if sop != testSOPUID {
		t.Errorf("SOPInstanceUID = %q", sop)
	}
	pn, ok := dss[0].GetString(tag.PatientName)
	if !ok {
		t.Fatal("PatientName missing")
	}
	if pn != "DOE^JOHN" {
		t.Errorf("PatientName = %q", pn)
	}
}

// TestMetadataBulkDataURIResolution verifies the WADO-specific rewrite:
// "./bulkdata/..." resolves against the metadata request URL per PS3.18.
func TestMetadataBulkDataURIResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(studyMetaJSON))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/api/x")
	dss, err := c.StudyMetadata(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}

	out, err := serialization.ToJSON(dss[0])
	if err != nil {
		t.Fatal(err)
	}
	want := srv.URL + "/api/x/studies/1.2.840.1/bulkdata/00282000"
	if !strings.Contains(string(out), `"BulkDataURI":"`+want+`"`) {
		t.Errorf("resolved BulkDataURI missing in %s", out)
	}
}

// TestMetadataMultipartWrappedTolerance covers older servers that wrap
// dicom+json in multipart/related.
func TestMetadataMultipartWrappedTolerance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom+json"`)
		_, _ = w.Write(multipartBody([]fakePart{
			{ct: mediaTypeDICOMJSON, body: studyMetaJSON},
		}))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	dss, err := c.StudyMetadata(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(dss) != 1 {
		t.Fatalf("datasets = %d, want 1", len(dss))
	}
}

func TestInstanceMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(`{"00080018":{"vr":"UI","Value":["1.2.840.777"]}}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	ds, err := c.InstanceMetadata(context.Background(), "1.2.3", "1.2.4", testSOPUID)
	if err != nil {
		t.Fatal(err)
	}
	sop, ok := ds.GetString(tag.SOPInstanceUID)
	if !ok {
		t.Fatal("SOPInstanceUID missing")
	}
	if sop != testSOPUID {
		t.Errorf("SOPInstanceUID = %q", sop)
	}
}

// TestInstanceMetadataArrayForm covers the conformant deployed majority
// (Orthanc, dcm4che, the dicomweb-client reference): instance metadata
// returned as a single-element JSON array.
func TestInstanceMetadataArrayForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(`[{"00080018":{"vr":"UI","Value":["1.2.840.777"]},` +
			`"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/7FE00010"}}]`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	ds, err := c.InstanceMetadata(context.Background(), "1.2.3", "1.2.4", testSOPUID)
	if err != nil {
		t.Fatalf("array-form instance metadata rejected: %v", err)
	}
	sop, ok := ds.GetString(tag.SOPInstanceUID)
	if !ok || sop != testSOPUID {
		t.Errorf("SOPInstanceUID = %q, %v", sop, ok)
	}
}

// TestInstanceMetadataMultiItemArray guards the explicit error for a
// multi-item array (instance metadata is a single item by definition).
func TestInstanceMetadataMultiItemArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(`[{"00080018":{"vr":"UI","Value":["1.2.840.777"]}},` +
			`{"00080018":{"vr":"UI","Value":["1.2.840.888"]}}]`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	_, err := c.InstanceMetadata(context.Background(), "1.2.3", "1.2.4", testSOPUID)
	if err == nil || !strings.Contains(err.Error(), "single item") {
		t.Errorf("err = %v, want single-item error", err)
	}
}

func TestMetadataInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	if _, err := c.StudyMetadata(context.Background(), "1.2.3"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// TestMetadataNoTransferSyntaxOnAccept pins the scoping of
// WithTransferSyntax: dicom+json has no transfer syntax, so the parameter
// must not reach the metadata Accept header.
func TestMetadataNoTransferSyntaxOnAccept(t *testing.T) {
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(studyMetaJSON))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	if _, err := c.StudyMetadata(context.Background(), "1.2.840.1", WithTransferSyntax("1.2.840.10008.1.2.1")); err != nil {
		t.Fatal(err)
	}
	if gotAccept != mediaTypeDICOMJSON {
		t.Errorf("Accept = %q, want plain application/dicom+json", gotAccept)
	}
}

// TestMetadataMultiPartBareObjects covers older servers that emit one
// dicom+json dataset per multipart part as a bare object: each part must be
// parsed independently instead of being concatenated into invalid JSON.
func TestMetadataMultiPartBareObjects(t *testing.T) {
	const ds1 = `{"00080018":{"vr":"UI","Value":["1.2.840.777"]}}`
	const ds2 = `{"00080018":{"vr":"UI","Value":["1.2.840.888"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom+json"`)
		_, _ = w.Write(multipartBody([]fakePart{
			{ct: mediaTypeDICOMJSON, body: ds1},
			{ct: mediaTypeDICOMJSON, body: ds2},
		}))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	dss, err := c.StudyMetadata(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatalf("per-part metadata parsing failed: %v", err)
	}
	if len(dss) != 2 {
		t.Fatalf("datasets = %d, want 2", len(dss))
	}
	for i, want := range []string{testSOPUID, "1.2.840.888"} {
		sop, ok := dss[i].GetString(tag.SOPInstanceUID)
		if !ok || sop != want {
			t.Errorf("datasets[%d] SOPInstanceUID = %q, %v; want %q", i, sop, ok, want)
		}
	}

	// Instance metadata with more than one item (here: one per part) is a
	// contract violation and must fail explicitly.
	if _, err := c.InstanceMetadata(context.Background(), "1.2.3", "1.2.4", testSOPUID); err == nil ||
		!strings.Contains(err.Error(), "single item") {
		t.Errorf("err = %v, want single-item error", err)
	}
}

// TestResolveBulkDataURIsJSONNumberPrecision guards the decode / re-encode
// round trip: numeric literals must survive verbatim (no float64 round-off)
// while the relative BulkDataURI is rewritten.
func TestResolveBulkDataURIsJSONNumberPrecision(t *testing.T) {
	base, err := url.Parse("https://h/dicomweb/studies/1.2/metadata")
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"00181060":{"vr":"IS","Value":[9007199254740993]},` +
		`"00209222":{"vr":"DS","Value":[0.1234567890123456789]},` +
		`"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/7FE00010"}}`)

	out, err := resolveBulkDataURIsJSON(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{"9007199254740993", "0.1234567890123456789"} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Errorf("numeric literal %s lost in %s", literal, out)
		}
	}
	want := resolvedBulk7FE00010
	if !bytes.Contains(out, []byte(`"BulkDataURI":"`+want+`"`)) {
		t.Errorf("BulkDataURI not resolved in %s", out)
	}
}

// TestResolveBulkDataURIsJSONProbe pins the superset probes of
// resolveBulkDataURIsJSON: items whose every BulkDataURI is scheme-absolute
// (or absent) come back byte-identical without the decode/rewrite/re-encode
// pass, while every relative form — including bare relative paths and
// query/fragment references — is rewritten.
func TestResolveBulkDataURIsJSONProbe(t *testing.T) {
	base, err := url.Parse("https://h/dicomweb/studies/1.2/metadata")
	if err != nil {
		t.Fatal(err)
	}

	rewritten := map[string]string{
		`{"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/7FE00010"}}`:                           resolvedBulk7FE00010,
		`{"7FE00010":{"vr":"OW","BulkDataURI":"/dicomweb/bulk/1"}}`:                              "https://h/dicomweb/bulk/1",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"../pixeldata/9"}}`:                                "https://h/dicomweb/studies/pixeldata/9",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"bulkdata/7FE00010"}}`:                             resolvedBulk7FE00010,
		`{"7FE00010":{"vr":"OW","BulkDataURI":"bulk?x:y"}}`:                                      "https://h/dicomweb/studies/1.2/bulk?x:y",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"part#f"}}`:                                        "https://h/dicomweb/studies/1.2/part#f",
		`{"7FE00010" : {"vr":"OW", "BulkDataURI" : "./spaced/1"}}`:                               "https://h/dicomweb/studies/1.2/spaced/1",
		`{"00089123":{"vr":"SQ","Value":[{"7FE00010":{"vr":"OW","BulkDataURI":"./nested/1"}}]}}`: "https://h/dicomweb/studies/1.2/nested/1",
		// A relative value after an absolute one exercises the probe's
		// continue-after-scheme path.
		`{"A":{"vr":"OW","BulkDataURI":"https://elsewhere/x"},"B":{"vr":"OW","BulkDataURI":"./after/1"}}`: "https://h/dicomweb/studies/1.2/after/1",
	}
	for raw, want := range rewritten {
		out, err := resolveBulkDataURIsJSON(json.RawMessage(raw), base)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !bytes.Contains(out, []byte(`"BulkDataURI":"`+want+`"`)) {
			t.Errorf("%s: BulkDataURI not rewritten to %q in %s", raw, want, out)
		}
	}

	untouched := []string{
		// Fast path: no key at all.
		`{"00080018":{"vr":"UI","Value":["1.2.3"]},"7FE00010":{"vr":"OW","InlineBinary":"AAAA"}}`,
		// Single absolute value.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"https://h/dicomweb/bulk/1"}}`,
		// Two absolute values: the probe must keep scanning past the first.
		`{"A":{"vr":"OW","BulkDataURI":"https://h/a"},"B":{"vr":"OW","BulkDataURI":"http://x/b"}}`,
		// A non-http scheme is absolute too (rejected later at fetch time).
		`{"7FE00010":{"vr":"OW","BulkDataURI":"ftp://h/bulk"}}`,
		// A colon inside the query of a schemed URI does not make it
		// relative; see the rewritten list for the relative query form.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"https://h/a?x:y"}}`,
		// Non-string value: the walk never rewrites it.
		`{"7FE00010":{"vr":"OW","BulkDataURI":42}}`,
		// Empty value: skipped by the walk.
		`{"7FE00010":{"vr":"OW","BulkDataURI":""}}`,
		// The word appearing inside another string value is not a key.
		`{"00311030":{"vr":"LO","Value":["mentions BulkDataURI in prose"]}}`,
		// url.Parse rejects this shape; the probe defers to the walk, which
		// leaves it untouched.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"1a:b"}}`,
	}
	for _, raw := range untouched {
		out, err := resolveBulkDataURIsJSON(json.RawMessage(raw), base)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !bytes.Equal(out, []byte(raw)) {
			t.Errorf("%s: unexpectedly rewritten to %s", raw, out)
		}
	}
}

// TestMetadataTrailingContent guards the streaming decoder's strictness: a
// part carrying content after its top-level value fails like a plain
// json.Unmarshal would.
func TestMetadataTrailingContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(`[{"00080018":{"vr":"UI","Value":["1.2.840.777"]}}] garbage`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	if _, err := c.StudyMetadata(context.Background(), "1.2.3"); err == nil {
		t.Error("expected error for trailing content")
	}
}

// TestStudyMetadataStream verifies the streaming cursor: parts are parsed as
// the caller advances, array parts contribute multiple items, io.EOF
// terminates the iteration, and Close releases the response.
func TestStudyMetadataStream(t *testing.T) {
	const item1 = `{"00080018":{"vr":"UI","Value":["1.2.840.777"]}}`
	const item2 = `{"00080018":{"vr":"UI","Value":["1.2.840.888"]}}`
	const item3 = `{"00080018":{"vr":"UI","Value":["1.2.840.999"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom+json"`)
		_, _ = w.Write(multipartBody([]fakePart{
			{ct: mediaTypeDICOMJSON, body: "[" + item1 + "," + item2 + "]"},
			{ct: mediaTypeDICOMJSON, body: item3},
		}))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	ms, err := c.StudyMetadataStream(context.Background(), "1.2.840.1")
	if err != nil {
		t.Fatal(err)
	}
	var sops []string
	for {
		ds, err := ms.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		sop, ok := ds.GetString(tag.SOPInstanceUID)
		if !ok {
			t.Fatal("SOPInstanceUID missing")
		}
		sops = append(sops, sop)
	}
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{testSOPUID, "1.2.840.888", "1.2.840.999"}
	if len(sops) != len(want) {
		t.Fatalf("streamed datasets = %v, want %v", sops, want)
	}
	for i := range want {
		if sops[i] != want[i] {
			t.Errorf("sops[%d] = %q, want %q", i, sops[i], want[i])
		}
	}

	// Next after EOF keeps returning io.EOF.
	if _, err := ms.Next(); err != io.EOF {
		t.Errorf("Next after EOF = %v, want io.EOF", err)
	}
}

// TestMetadataCharsetAsQueryParam pins the PS3.18 negotiation mechanism for
// metadata requests: the charset travels in the query string, not in an
// Accept-Charset header.
func TestMetadataCharsetAsQueryParam(t *testing.T) {
	var gotQuery url.Values
	var gotAcceptCharset string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAcceptCharset = r.URL.Query(), r.Header.Get("Accept-Charset")
		w.Header().Set("Content-Type", mediaTypeDICOMJSON)
		_, _ = w.Write([]byte(studyMetaJSON))
	}))
	defer srv.Close()

	c, _ := New(srv.URL + "/dicomweb")
	if _, err := c.StudyMetadata(context.Background(), "1.2.840.1", WithCharset("ISO_IR 100")); err != nil {
		t.Fatal(err)
	}
	if got := gotQuery.Get("charset"); got != "ISO_IR 100" {
		t.Errorf("charset query = %q, want ISO_IR 100", got)
	}
	if gotAcceptCharset != "" {
		t.Errorf("Accept-Charset header = %q, want absent", gotAcceptCharset)
	}
}
