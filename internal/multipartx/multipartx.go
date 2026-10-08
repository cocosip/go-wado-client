// Package multipartx provides the minimal multipart/related response cursor
// shared by the service packages. It tolerates single-part replies exactly
// like wadors.Multipart does (some servers answer non-multipart where
// multipart/related is expected) and exists so the lighter clients (qido)
// do not need the full wadors cursor with its DICOM-specific conveniences.
package multipartx

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// Parts is a streaming cursor over an HTTP response body: a
// multipart/related body yields its parts one by one, any other body is
// delivered as a single pseudo-part. Close must be called when done.
type Parts struct {
	resp     *http.Response
	mr       *multipart.Reader
	single   bool
	singleCT string
	next     int
}

// New classifies the response by its Content-Type and binds the cursor to
// its body.
func New(resp *http.Response) (*Parts, error) {
	ct := resp.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt, params = ct, map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("multipart response without boundary: %q", ct)
		}
		return &Parts{resp: resp, mr: multipart.NewReader(resp.Body, boundary)}, nil
	}
	return &Parts{resp: resp, single: true, singleCT: mt}, nil
}

// Next returns the next part; it returns io.EOF when the response is
// exhausted. The content type is the part's own Content-Type, falling back
// to the response media type for the single-part form.
func (p *Parts) Next() (r io.Reader, contentType string, err error) {
	if p.single {
		if p.next > 0 {
			return nil, "", io.EOF
		}
		p.next = 1
		return p.resp.Body, p.singleCT, nil
	}
	p.next++
	part, err := p.mr.NextRawPart()
	if err != nil {
		// mime/multipart may wrap io.EOF on repeated exhaustion; normalize
		// it so callers always see plain io.EOF at the end.
		if errors.Is(err, io.EOF) {
			return nil, "", io.EOF
		}
		return nil, "", err
	}
	ct := part.Header.Get("Content-Type")
	return part, ct, nil
}

// Close closes the underlying response.
func (p *Parts) Close() error {
	if p.resp == nil {
		return nil
	}
	return p.resp.Body.Close()
}
