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
			WindowCenter: &wc, WindowWidth: &ww, PresentationUID: testPresentationUID, PresentationSeriesUID: "1.2.10"}},
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
		{"region with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Region: &[4]float64{0.1, 0.1, 0.9, 0.9}}},
		// PS3.18 §8.2: annotation is burned into image pixels — rendered-only.
		{"annotation with dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeDICOM, Annotation: []string{AnnotationPatient}}},
		// PS3.18 §8.1: anonymize applies to DICOM responses only.
		{"anonymize with rendered content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: testContentTypeJPEG, Anonymize: true}},
		{"invalid content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "not a media type"}},
		// Parameters do not change the classification: still application/dicom.
		{"window with parameterized dicom content type", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			ContentType: "application/dicom; charset=utf-8", WindowCenter: &wc, WindowWidth: &ww}},
		{"rows only", Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID, Rows: 512}},
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
		{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID,
			PresentationUID: testPresentationUID, PresentationSeriesUID: "1.2.10"},
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
		"anonymity":      anonymizeEnabled,
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

	// Modern naming switches the anonymize key.
	mq := req.query(true)
	if got := mq.Get("anonymize"); got != anonymizeEnabled {
		t.Errorf("anonymize = %q", got)
	}
	if _, ok := mq["anonymity"]; ok {
		t.Error("anonymity must not appear in modern mode")
	}

	// Empty content type omits the parameter (application/dicom default).
	plain := Request{StudyUID: testStudyUID, SeriesUID: testSeriesUID, ObjectUID: testObjectUID}.query(false)
	if _, ok := plain["contentType"]; ok {
		t.Error("contentType must be omitted when empty")
	}
}
