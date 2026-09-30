package dicomx

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/writer"
	"github.com/cocosip/go-wado-client/wadors"
)

// buildDICOMFile writes a minimal Part 10 file with go-dicom's writer.
func buildDICOMFile(t *testing.T, sopUID, patientName string) []byte {
	t.Helper()
	ds := dataset.New()
	if err := ds.AddOrUpdateValue(tag.SOPInstanceUID, sopUID); err != nil {
		t.Fatal(err)
	}
	if err := ds.AddOrUpdateValue(tag.SOPClassUID, "1.2.840.10008.5.1.4.1.1.7"); err != nil {
		t.Fatal(err)
	}
	if err := ds.AddOrUpdateValue(tag.PatientName, patientName); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writer.Write(&buf, ds); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDatasetsRoundTrip covers the full bridge: go-dicom writer → multipart
// HTTP response → WADO client → dicomx streaming parse via go-dicom parser.
func TestDatasetsRoundTrip(t *testing.T) {
	dicomFile := buildDICOMFile(t, "1.2.840.113619.2.1", "DOE^JOHN")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = fmt.Fprintf(w, "--BNDRY\r\nContent-Type: application/dicom\r\n\r\n")
		_, _ = w.Write(dicomFile)
		_, _ = fmt.Fprint(w, "\r\n--BNDRY--\r\n")
	}))
	defer srv.Close()

	c, err := wadors.New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.113619")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	n := 0
	for res, err := range Datasets(mp) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		sop, ok := res.Dataset.GetString(tag.SOPInstanceUID)
		if !ok {
			t.Fatal("SOPInstanceUID missing")
		}
		if sop != "1.2.840.113619.2.1" {
			t.Errorf("SOPInstanceUID = %q", sop)
		}
		pn, ok := res.Dataset.GetString(tag.PatientName)
		if !ok {
			t.Fatal("PatientName missing")
		}
		if pn != "DOE^JOHN" {
			t.Errorf("PatientName = %q", pn)
		}
	}
	if n != 1 {
		t.Errorf("parsed %d datasets, want 1", n)
	}
}

// TestDatasetsParseError verifies that a non-DICOM part surfaces as an
// iteration error.
func TestDatasetsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; boundary="BNDRY"; type="application/dicom"`)
		_, _ = fmt.Fprint(w, "--BNDRY\r\nContent-Type: application/dicom\r\n\r\nnot-a-dicom-file\r\n--BNDRY--\r\n")
	}))
	defer srv.Close()

	c, err := wadors.New(srv.URL + "/dicomweb")
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.RetrieveStudy(context.Background(), "1.2.840.113619")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mp.Close() }()

	for _, err := range Datasets(mp) {
		if err == nil {
			t.Fatal("expected parse error for non-DICOM part")
		}
		return
	}
	t.Fatal("iteration produced no values")
}
