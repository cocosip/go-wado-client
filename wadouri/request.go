package wadouri

import (
	"fmt"
	"mime"
	"net/url"
	"strconv"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// Annotation kinds (values of the annotation parameter).
const (
	AnnotationPatient   = "patient"
	AnnotationTechnique = "technique"
)

const (
	// anonymizeEnabled is the value of the anonymity/anonymize switch.
	anonymizeEnabled = "yes"
	// reasonSetTogether is shared by the paired-parameter validations.
	reasonSetTogether = "must be set together"
	// reasonNeedsRendered is shared by the rendered-only-parameter checks.
	reasonNeedsRendered = "requires a rendered contentType"
	// fieldRegion is the error field name of the region parameter.
	fieldRegion = "Region"
	// fieldFrameNumber is the error field name of the frameNumber parameter.
	fieldFrameNumber = "FrameNumber"
)

// Request holds the WADO-URI retrieval parameters.
type Request struct {
	// Mandatory identification triple.
	StudyUID  string
	SeriesUID string
	ObjectUID string

	// "" selects application/dicom; a rendered media type such as
	// image/jpeg selects the rendered transaction.
	ContentType string

	TransferSyntax string
	Charset        string
	// Anonymize is emitted as anonymize=yes (anonymity=yes with legacy
	// naming). It is a DICOM-only parameter: PS3.18 §9.4.1.2.1 forbids it
	// together with a rendered contentType.
	Anonymize  bool
	Annotation []string // "patient" / "technique"; rendered-only (PS3.18 §9.5.1.2.2)

	// Rendered parameters below. Per PS3.18 §9.5.1.2 they apply to image
	// retrieval only, so they are rejected together with the
	// application/dicom content type (the server would answer 400).
	FrameNumber  int // 1-based; 0 = absent
	ImageQuality int // 1..100; 0 = absent
	// Rows/Columns cap the rendered image size; PS3.18 §9.5.1.2.4: "If
	// either parameter is present, both shall be present"; 0 = absent.
	Rows    int
	Columns int
	Region  *[4]float64 // xmin,ymin,xmax,ymax normalized to 0..1; nil = absent

	// Window center/width: set together, mutually exclusive with
	// Presentation*, and not allowed for application/dicom.
	WindowCenter *float64
	WindowWidth  *float64

	// Presentation State: set together, mutually exclusive with the window
	// pair.
	PresentationUID       string
	PresentationSeriesUID string

	// Extra passes through private gateway parameters (appended after the
	// standard ones).
	Extra url.Values
}

// validate performs local validation, rejecting up front the cases where
// the standard mandates a server-side 400.
func (r Request) validate(checkUID func(field, uid string) error) error {
	if err := r.validateUIDs(checkUID); err != nil {
		return err
	}
	isDICOM, err := r.dicomContentType()
	if err != nil {
		return err
	}
	if err := r.validateWindowAndPresentation(isDICOM); err != nil {
		return err
	}
	return r.validateRenderedValues(isDICOM)
}

// dicomContentType classifies the contentType: the media type (parameters
// stripped) decides between the DICOM instance transaction (the default) and
// the rendered transaction. An unparseable value is rejected up front — the
// server would answer 400.
func (r Request) dicomContentType() (bool, error) {
	if r.ContentType == "" {
		return true, nil
	}
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(r.ContentType))
	if err != nil {
		return false, &wado.RequestError{Field: "ContentType", Reason: fmt.Sprintf("not a valid media type: %v", err)}
	}
	return mt == "application/dicom", nil
}

// validateUIDs checks the mandatory identification triple and the optional
// UIDs against the whitelist.
func (r Request) validateUIDs(checkUID func(field, uid string) error) error {
	for _, f := range []struct{ field, uid string }{
		{"StudyUID", r.StudyUID},
		{"SeriesUID", r.SeriesUID},
		{"ObjectUID", r.ObjectUID},
	} {
		if f.uid == "" {
			return &wado.RequestError{Field: f.field, Reason: "is required"}
		}
	}
	for _, f := range []struct{ field, uid string }{
		{"studyUID", r.StudyUID},
		{"seriesUID", r.SeriesUID},
		{"objectUID", r.ObjectUID},
		{"presentationUID", r.PresentationUID},
		{"presentationSeriesUID", r.PresentationSeriesUID},
	} {
		if f.uid == "" {
			continue
		}
		if err := checkUID(f.field, f.uid); err != nil {
			return err
		}
	}
	// transferSyntax is validated separately: besides UIDs, PS3.18
	// §8.7.3.5.2 defines the wildcard "*" ("any transfer syntax the server
	// supports").
	if r.TransferSyntax != "" && r.TransferSyntax != "*" {
		if err := checkUID("transferSyntax", r.TransferSyntax); err != nil {
			return err
		}
	}
	return nil
}

// validateWindowAndPresentation enforces the window pair, the presentation
// state pair, and their mutual exclusion, and that windowing only combines
// with a rendered content type.
func (r Request) validateWindowAndPresentation(isDICOM bool) error {
	if (r.WindowCenter != nil) != (r.WindowWidth != nil) {
		return &wado.RequestError{Field: "WindowCenter/WindowWidth", Reason: reasonSetTogether}
	}
	if (r.PresentationUID != "") != (r.PresentationSeriesUID != "") {
		return &wado.RequestError{Field: "PresentationUID/PresentationSeriesUID", Reason: reasonSetTogether}
	}
	hasWindow := r.WindowCenter != nil || r.WindowWidth != nil
	hasPres := r.PresentationUID != "" || r.PresentationSeriesUID != ""
	if hasWindow && hasPres {
		return &wado.RequestError{Field: "WindowCenter", Reason: "windowing and presentation state are mutually exclusive"}
	}
	if hasWindow && isDICOM {
		return &wado.RequestError{Field: "WindowCenter", Reason: "windowing requires a rendered contentType"}
	}
	return nil
}

// validateRenderedValues checks the rendered-only parameters: their exclusion
// from the DICOM instance transaction (PS3.18 §9.5.1.2: they apply to image
// retrieval only — a request carrying them for a non-image object is answered
// 400), the anonymize direction (PS3.18 §9.4.1.2.1: DICOM responses only),
// the region constraints (PS3.18 §9.5.1.2.5: normalized 0..1), the
// presentation-state combination rules (PS3.18 §9.5.1.2.7), and the remaining
// value ranges.
func (r Request) validateRenderedValues(isDICOM bool) error {
	if r.Anonymize && !isDICOM {
		return &wado.RequestError{Field: "Anonymize", Reason: "requires the application/dicom contentType"}
	}
	if isDICOM {
		switch {
		case r.FrameNumber != 0:
			return &wado.RequestError{Field: fieldFrameNumber, Reason: reasonNeedsRendered}
		case r.ImageQuality != 0:
			return &wado.RequestError{Field: "ImageQuality", Reason: reasonNeedsRendered}
		case r.Rows != 0 || r.Columns != 0:
			return &wado.RequestError{Field: "Rows/Columns", Reason: reasonNeedsRendered}
		case r.Region != nil:
			return &wado.RequestError{Field: fieldRegion, Reason: reasonNeedsRendered}
		case len(r.Annotation) > 0:
			// PS3.18 §9.5.1.2.2: "It shall not be present if contentType is
			// application/dicom".
			return &wado.RequestError{Field: "Annotation", Reason: reasonNeedsRendered}
		}
	}
	// PS3.18 §9.5.1.2.4: "If either parameter is present, both shall be
	// present."
	if (r.Rows != 0) != (r.Columns != 0) {
		return &wado.RequestError{Field: "Rows/Columns", Reason: reasonSetTogether}
	}
	hasPres := r.PresentationUID != "" || r.PresentationSeriesUID != ""
	// PS3.18 §9.5.1.2.7: with a Presentation State the only other optional
	// parameters that may be present are annotation, imageQuality, region
	// and rows/columns — not frameNumber (windowing is already excluded by
	// validateWindowAndPresentation).
	if hasPres && r.FrameNumber != 0 {
		return &wado.RequestError{Field: fieldFrameNumber, Reason: "must not be combined with a presentation state"}
	}
	if r.Region != nil {
		x1, y1, x2, y2 := r.Region[0], r.Region[1], r.Region[2], r.Region[3]
		if !(x1 >= 0 && x1 < x2 && x2 <= 1) || !(y1 >= 0 && y1 < y2 && y2 <= 1) {
			return &wado.RequestError{Field: fieldRegion, Reason: "require 0<=xmin<xmax<=1 and 0<=ymin<ymax<=1"}
		}
	}
	if r.FrameNumber < 0 {
		return &wado.RequestError{Field: fieldFrameNumber, Reason: "must be >= 1"}
	}
	if r.ImageQuality != 0 && (r.ImageQuality < 1 || r.ImageQuality > 100) {
		return &wado.RequestError{Field: "ImageQuality", Reason: "must be within 1..100"}
	}
	for _, a := range r.Annotation {
		if a != AnnotationPatient && a != AnnotationTechnique {
			return &wado.RequestError{Field: "Annotation", Reason: fmt.Sprintf("unknown kind %q", a)}
		}
	}
	return nil
}

// query encodes the query parameters; the current PS3.18 naming is the
// default (anonymize, in force since 2019) and legacy switches the
// anonymization key to the pre-2019 name (anonymity).
func (r Request) query(legacy bool) url.Values {
	q := url.Values{}
	q.Set("requestType", "WADO")
	q.Set("studyUID", r.StudyUID)
	q.Set("seriesUID", r.SeriesUID)
	q.Set("objectUID", r.ObjectUID)
	if r.ContentType != "" {
		q.Set("contentType", r.ContentType)
	}
	if r.TransferSyntax != "" {
		q.Set("transferSyntax", r.TransferSyntax)
	}
	if r.Charset != "" {
		q.Set("charset", r.Charset)
	}
	if r.Anonymize {
		if legacy {
			q.Set("anonymity", anonymizeEnabled)
		} else {
			q.Set("anonymize", anonymizeEnabled)
		}
	}
	if len(r.Annotation) > 0 {
		q.Set("annotation", strings.Join(r.Annotation, ","))
	}
	if r.FrameNumber > 0 {
		q.Set("frameNumber", strconv.Itoa(r.FrameNumber))
	}
	if r.ImageQuality > 0 {
		q.Set("imageQuality", strconv.Itoa(r.ImageQuality))
	}
	if r.Rows > 0 {
		q.Set("rows", strconv.Itoa(r.Rows))
	}
	if r.Columns > 0 {
		q.Set("columns", strconv.Itoa(r.Columns))
	}
	if r.Region != nil {
		q.Set("region", strings.Join([]string{
			formatFloat(r.Region[0]), formatFloat(r.Region[1]),
			formatFloat(r.Region[2]), formatFloat(r.Region[3]),
		}, ","))
	}
	if r.WindowCenter != nil {
		q.Set("windowCenter", formatFloat(*r.WindowCenter))
	}
	if r.WindowWidth != nil {
		q.Set("windowWidth", formatFloat(*r.WindowWidth))
	}
	if r.PresentationUID != "" {
		q.Set("presentationUID", r.PresentationUID)
	}
	if r.PresentationSeriesUID != "" {
		q.Set("presentationSeriesUID", r.PresentationSeriesUID)
	}
	for k, vs := range r.Extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return q
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
