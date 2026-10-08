package wadouri

import (
	"errors"
	"net/url"
	"testing"

	"github.com/cocosip/go-wado-client"
)

// testContentTypeJPEG selects the rendered transaction in the tests below.
const testContentTypeJPEG = "image/jpeg"

func strictCheck(field, uid string) error {
	if err := wado.ValidateUID(uid); err != nil {
		return &wado.UIDError{Field: field, UID: uid}
	}
	return nil
}

func TestRequestValidate(t *testing.T) {
	wc, ww := 40.0, 400.0
	base := Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID}

	cases := []struct {
		name string
		req  Request
	}{
		{"missing studyUID", Request{SeriesUID: testSeriesUID, ObjectUID: testObjectUID}},
		{"missing seriesUID", Request{StudyUID: testStudyUID, ObjectUID: testObjectUID}},
		{"missing objectUID", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID}},
		{"window center only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, WindowCenter: &wc}},
		{"window width only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, WindowWidth: &ww}},
		{"presentation uid only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, PresentationUID: testPresentationUID}},
		{"presentation pair with window pair", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			WindowCenter: &wc, WindowWidth: &ww, PresentationUID: testPresentationUID, PresentationSeriesUID: testPresentationSeriesUID}},
		{"window with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, WindowCenter: &wc, WindowWidth: &ww}},
		{"window with case-varied dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "Application/DICOM", WindowCenter: &wc, WindowWidth: &ww}},
		{"frameNumber with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, FrameNumber: 3}},
		{"imageQuality with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, ImageQuality: 90}},
		{"rows/columns with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Rows: 512, Columns: 512}},
		{"rows only with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Rows: 512}},
		{"region with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Region: &[4]float64{0.1, 0.1, 0.9, 0.9}}},
		// PS3.18 §9.5.1.2.4: "If either parameter is present, both shall be
		// present."
		{"rows only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, Rows: 512}},
		{"columns only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, Columns: 512}},
		// PS3.18 §9.5.1.2.7: with a Presentation State the only other optional
		// parameters allowed are annotation, imageQuality, region and
		// rows/columns — not frameNumber.
		{"frameNumber with presentation state", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, FrameNumber: 3,
			PresentationUID: testPresentationUID, PresentationSeriesUID: testPresentationSeriesUID}},
		// PS3.18 §9.5.1.2.2: annotation is burned into image pixels —
		// rendered-only.
		{"annotation with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Annotation: []string{AnnotationPatient}}},
		// PS3.18 §9.4.1.2.1: anonymize applies to DICOM responses only.
		{"anonymize with rendered content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, Anonymize: true}},
		{"invalid content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "not a media type"}},
		// Parameters do not change the classification: still application/dicom.
		{"window with parameterized dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "application/dicom; charset=utf-8", WindowCenter: &wc, WindowWidth: &ww}},
		{"region out of range", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			Region: &[4]float64{0.5, 0.1, 0.4, 0.9}}},
		{"quality too large", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, ImageQuality: 101}},
		{"unknown annotation", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			Annotation: []string{"bogus"}}},
	}
	for _, tc := range cases {
		if err := tc.req.validate(strictCheck); !errors.Is(err, wado.ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}

	// Valid requests must pass, including paired constraints.
	valid := []Request{
		base,
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, ContentType: testContentTypeJPEG,
			WindowCenter: &wc, WindowWidth: &ww, Rows: 512, Columns: 512,
			Region: &[4]float64{0.1, 0.1, 0.9, 0.9}, ImageQuality: 90, FrameNumber: 3,
			Annotation: []string{AnnotationPatient}},
		// PS3.18 §9.5.1.2.7: region combines with a Presentation State.
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, Region: &[4]float64{0.1, 0.1, 0.9, 0.9},
			PresentationUID: testPresentationUID, PresentationSeriesUID: testPresentationSeriesUID},
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			PresentationUID: testPresentationUID, PresentationSeriesUID: testPresentationSeriesUID},
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			TransferSyntax: "1.2.840.10008.1.2.1"},
		// The PS3.18 wildcard ("any transfer syntax") is not a UID and passes.
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, TransferSyntax: "*"},
		// Anonymize is the anonymization of the returned DICOM object, so it
		// combines with application/dicom (explicitly or by omission).
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, Anonymize: true},
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "application/dicom; charset=utf-8", Anonymize: true},
	}
	for _, req := range valid {
		if err := req.validate(strictCheck); err != nil {
			t.Errorf("valid request rejected: %v", err)
		}
	}

	// UID whitelist violations surface as ErrInvalidUID.
	badUID := Request{StudyUID: "../evil", SeriesUID: testSeriesUID, ObjectUID: testObjectUID}
	if err := badUID.validate(strictCheck); !errors.Is(err, wado.ErrInvalidUID) {
		t.Errorf("err = %v, want ErrInvalidUID", err)
	}
}

func TestRequestQueryEncoding(t *testing.T) {
	wc, ww := 40.0, 400.0
	req := Request{
		StudyUID:       testStudyUID,
		SeriesUID:      testSeriesUID,
		ObjectUID:      testObjectUID,
		ContentType:    testContentTypeJPEG,
		TransferSyntax: "1.2.840.10008.1.2.4.50",
		Charset:        "ISO_IR 100",
		Anonymize:      true,
		Annotation:     []string{AnnotationPatient, AnnotationTechnique},
		FrameNumber:    7,
		ImageQuality:   90,
		Rows:           512,
		Columns:        1024,
		Region:         &[4]float64{0.1, 0.2, 0.8, 0.9},
		WindowCenter:   &wc,
		WindowWidth:    &ww,
		Extra:          url.Values{"privateParam": {"v1"}},
	}
	q := req.query(false)

	want := map[string]string{
		"requestType":    "WADO",
		"studyUID":       testStudyUID,
		"seriesUID":      testSeriesUID,
		"objectUID":      testObjectUID,
		"contentType":    testContentTypeJPEG,
		"transferSyntax": "1.2.840.10008.1.2.4.50",
		"charset":        "ISO_IR 100",
		"anonymize":      anonymizeEnabled,
		"annotation":     "patient,technique",
		"frameNumber":    "7",
		"imageQuality":   "90",
		"rows":           "512",
		"columns":        "1024",
		"region":         "0.1,0.2,0.8,0.9",
		"windowCenter":   "40",
		"windowWidth":    "400",
		"privateParam":   "v1",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// The default naming is the current standard: anonymize (PS3.18 §9.4.1.2.1).
	// Legacy naming switches the anonymization key to the pre-2019 name.
	lq := req.query(true)
	if got := lq.Get("anonymity"); got != anonymizeEnabled {
		t.Errorf("anonymity = %q", got)
	}
	if _, ok := lq["anonymize"]; ok {
		t.Error("anonymize must not appear in legacy mode")
	}

	// Empty content type omits the parameter (application/dicom default).
	plain := Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID}.query(false)
	if _, ok := plain["contentType"]; ok {
		t.Error("contentType must be omitted when empty")
	}

	// Tiny region coordinates must stay in plain decimal notation — servers
	// choke on exponent forms like "1e-07" (same rationale as window values).
	tiny := Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
		Region: &[4]float64{1e-07, 0, 5e-06, 1}}.query(false)
	if got := tiny.Get("region"); got != "0.0000001,0,0.000005,1" {
		t.Errorf("region = %q, want plain decimal notation", got)
	}
}
