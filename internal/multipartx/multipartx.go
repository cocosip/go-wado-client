// Package multipartx provides the minimal multipart/related response cursor
// shared by the service packages. It tolerates single-part replies exactly
// like wadors.Multipart does (some servers answer non-multipart where
// multipart/related is expected) and exists so the lighter clients (qido)
// do not need the full wadors cursor with its DICOM-specific conveniences.
package multipartx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// Tracker is an io.Reader wrapper that records whether the multipart
// terminating delimiter ("--boundary--") passed through the stream.
//
// mime/multipart cannot tell a completed body from one that merely stopped at
// a part boundary: a close-delimited body ending after "--boundary\r\n" and a
// proper "--boundary--\r\n" end both surface as the bare io.EOF sentinel.
// Observing the delimiter on the wire is what makes the difference
// detectable, so a truncated download is never mistaken for a complete one.
type Tracker struct {
	r     io.Reader
	delim []byte
	tail  []byte
	seen  bool
}

// NewTracker wraps r, watching for the terminating delimiter of boundary.
func NewTracker(r io.Reader, boundary string) *Tracker {
	return &Tracker{r: r, delim: []byte("--" + boundary + "--")}
}

// Complete reports whether the terminating delimiter was observed.
func (t *Tracker) Complete() bool { return t.seen }

// Read implements io.Reader, scanning the bytes for the delimiter.
func (t *Tracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && !t.seen {
		t.tail = append(t.tail, p[:n]...)
		if bytes.Contains(t.tail, t.delim) {
			t.seen = true
			t.tail = nil
		} else if keep := len(t.delim) - 1; len(t.tail) > keep {
			// Retain only as much tail as a split delimiter could occupy.
			t.tail = append(t.tail[:0], t.tail[len(t.tail)-keep:]...)
		}
	}
	return n, err
}

// Parts is a streaming cursor over an HTTP response body: a
// multipart/related body yields its parts one by one, any other body is
// delivered as a single pseudo-part. Close must be called when done.
type Parts struct {
	resp     *http.Response
	mr       *multipart.Reader
	tracker  *Tracker
	single   bool
	singleCT string
	next     int
	done     bool
	doneErr  error
}

// New classifies the response by its Content-Type and binds the cursor to
// its body.
//
// A Content-Type that mime.ParseMediaType rejects is still sniffed for the
// multipart prefix and the boundary is recovered textually: real gateways
// emit unquoted or otherwise non-conformant type parameters, and giving up on
// them would fail the whole request rather than just the media-type detail.
func New(resp *http.Response) (*Parts, error) {
	ct := resp.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt, params = ct, map[string]string{}
	}
	if strings.HasPrefix(strings.ToLower(mt), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			boundary = sniffBoundary(ct)
		}
		if boundary == "" {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("multipart response without boundary: %q", ct)
		}
		tracker := NewTracker(resp.Body, boundary)
		return &Parts{resp: resp, mr: multipart.NewReader(tracker, boundary), tracker: tracker}, nil
	}
	return &Parts{resp: resp, single: true, singleCT: mt}, nil
}

// sniffBoundary recovers a boundary parameter from a Content-Type that
// mime.ParseMediaType could not parse.
func sniffBoundary(ct string) string {
	for _, part := range strings.Split(ct, ";") {
		part = strings.TrimSpace(part)
		name, value, found := strings.Cut(part, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "boundary") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		return value
	}
	return ""
}

// Next returns the next part; it returns io.EOF when the response is
// exhausted. The content type is the part's own Content-Type, falling back
// to the response media type for the single-part form.
//
// A response that stops at a part boundary without the closing delimiter is
// reported as io.ErrUnexpectedEOF instead of io.EOF, so a truncated result
// set is never mistaken for a complete one.
func (p *Parts) Next() (r io.Reader, contentType string, err error) {
	if p.single {
		if p.next > 0 {
			return nil, "", io.EOF
		}
		p.next = 1
		return p.resp.Body, p.singleCT, nil
	}
	if p.done {
		return nil, "", p.doneErr
	}
	p.next++
	part, err := p.mr.NextRawPart()
	if err != nil {
		p.done = true
		if errors.Is(err, io.EOF) {
			// The only clean end is the terminating delimiter having been
			// observed on the wire; every other EOF-shaped stop is a
			// truncated response.
			if p.tracker.Complete() {
				p.doneErr = io.EOF
				return nil, "", io.EOF
			}
			p.doneErr = fmt.Errorf("multipart response truncated (no closing boundary): %w", io.ErrUnexpectedEOF)
			return nil, "", p.doneErr
		}
		p.doneErr = err
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
