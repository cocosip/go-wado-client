package wadouri

import (
	"fmt"
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
	Anonymize      bool     // emitted as anonymity=yes (anonymize=yes with modern names)
	Annotation     []string // "patient" / "technique"

	// Rendered parameters below.
	FrameNumber  int
	ImageQuality int // 1..100
	Rows         int // paired with Columns
	Columns      int
	Region       *[4]float64 // xmin,ymin,xmax,ymax normalized to 0..1

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
	// The mandatory identification triple: the server must reject requests
	// missing any of them.
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
		{"transferSyntax", r.TransferSyntax},
	} {
		if f.uid == "" {
			continue
		}
		if err := checkUID(f.field, f.uid); err != nil {
			return err
		}
	}
	hasWindow := r.WindowCenter != nil || r.WindowWidth != nil
	if (r.WindowCenter != nil) != (r.WindowWidth != nil) {
		return &wado.RequestError{Field: "WindowCenter/WindowWidth", Reason: reasonSetTogether}
	}
	hasPres := r.PresentationUID != "" || r.PresentationSeriesUID != ""
	if (r.PresentationUID != "") != (r.PresentationSeriesUID != "") {
		return &wado.RequestError{Field: "PresentationUID/PresentationSeriesUID", Reason: reasonSetTogether}
	}
	if hasWindow && hasPres {
		return &wado.RequestError{Field: "WindowCenter", Reason: "windowing and presentation state are mutually exclusive"}
	}
	isDICOM := r.ContentType == "" ||
		strings.EqualFold(strings.TrimSpace(r.ContentType), "application/dicom")
	if hasWindow && isDICOM {
		return &wado.RequestError{Field: "WindowCenter", Reason: "windowing requires a rendered contentType"}
	}
	if (r.Rows > 0) != (r.Columns > 0) {
		return &wado.RequestError{Field: "Rows/Columns", Reason: reasonSetTogether}
	}
	if r.Region != nil {
		x1, y1, x2, y2 := r.Region[0], r.Region[1], r.Region[2], r.Region[3]
		if !(x1 >= 0 && x1 < x2 && x2 <= 1) || !(y1 >= 0 && y1 < y2 && y2 <= 1) {
			return &wado.RequestError{Field: "Region", Reason: "require 0<=xmin<xmax<=1 and 0<=ymin<ymax<=1"}
		}
	}
	if r.FrameNumber < 0 {
		return &wado.RequestError{Field: "FrameNumber", Reason: "must be >= 1"}
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

// query encodes the query parameters; classic naming is the default (what
// the deployed majority speaks) and modern switches to the 2023+ names.
func (r Request) query(modern bool) url.Values {
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
		if modern {
			q.Set("anonymize", anonymizeEnabled)
		} else {
			q.Set("anonymity", anonymizeEnabled)
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
		q.Set("columns", strconv.Itoa(r.Columns))
	}
	if r.Region != nil {
		q.Set("region", fmt.Sprintf("%g,%g,%g,%g", r.Region[0], r.Region[1], r.Region[2], r.Region[3]))
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
