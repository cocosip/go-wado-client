package wadors

import (
	"bytes"
	"fmt"
	"net/url"
)

// captured records the aspects of a request the fake server needs to
// expose to assertions.
type captured struct {
	path   string
	accept string
	query  url.Values
}

// fakePart describes one part of a hand-built multipart body.
type fakePart struct {
	ct   string
	loc  string
	body string
}

// Test fixtures shared by the fake servers.
const (
	mediaTypeDICOM       = "application/dicom"
	mediaTypeDICOMJSON   = "application/dicom+json"
	mediaTypeOctetStream = "application/octet-stream"
	mediaTypeJPEG        = "image/jpeg"
	dicomData1           = "DICOMDATA1"
	dicomData2           = "DICOMDATA2"
)

// multipartBody renders a complete multipart/related body with the fixed
// test boundary.
func multipartBody(parts []fakePart) []byte {
	const boundary = "BNDRY"
	var b bytes.Buffer
	for _, p := range parts {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		if p.ct != "" {
			fmt.Fprintf(&b, "Content-Type: %s\r\n", p.ct)
		}
		if p.loc != "" {
			fmt.Fprintf(&b, "Content-Location: %s\r\n", p.loc)
		}
		b.WriteString("\r\n")
		b.WriteString(p.body)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// dicomParts builds a typical two-part application/dicom response.
func dicomParts(host string) []fakePart {
	return []fakePart{
		{
			ct:   mediaTypeDICOM,
			loc:  "http://" + host + "/dicomweb/studies/1.2.840.1/series/2.3/instances/1.2.840.777",
			body: dicomData1,
		},
		{ct: mediaTypeDICOM, body: dicomData2},
	}
}
