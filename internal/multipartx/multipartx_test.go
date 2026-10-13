package multipartx

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"testing"
)

// mimeParse is a thin alias so the test can assert on the stdlib behavior the
// fallback compensates for.
func mimeParse(ct string) (string, map[string]string, error) {
	return mime.ParseMediaType(ct)
}

// resp builds a minimal response with the given Content-Type and body.
func resp(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestNewRejectsMultipartWithoutBoundary(t *testing.T) {
	r := resp(`multipart/related; type="application/dicom+json"`, "garbage")
	p, err := New(r)
	if err == nil {
		t.Fatalf("New = %v, nil; want a boundary error", p)
	}
	if !strings.Contains(err.Error(), "boundary") {
		t.Errorf("err = %v, want it to mention the boundary", err)
	}
}

// TestNewRecoversBoundaryFromUnparsableContentType guards the fallback that
// could never succeed: when mime.ParseMediaType failed, params was empty, so a
// multipart type was reported as "without boundary" even though a boundary was
// present in the header text.
func TestNewRecoversBoundaryFromUnparsableContentType(t *testing.T) {
	// An unquoted "type" parameter makes mime.ParseMediaType fail.
	const ct = `multipart/related; type=application/dicom+json; boundary=B`
	if _, _, err := mimeParse(ct); err == nil {
		t.Skip("the stdlib now parses this Content-Type; the fallback is no longer exercised")
	}
	body := "--B\r\nContent-Type: application/dicom+json\r\n\r\n[]\r\n--B--\r\n"
	p, err := New(resp(ct, body))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()
	r, _, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	b, _ := io.ReadAll(r)
	if strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("part = %q, want []", b)
	}
}

func TestNewSinglePartFallback(t *testing.T) {
	p, err := New(resp("application/dicom+json", "[]"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	r, ct, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if ct != "application/dicom+json" {
		t.Errorf("contentType = %q", ct)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Next(); err != io.EOF {
		t.Errorf("second Next = %v, want io.EOF", err)
	}
}

// TestTruncatedMultipartIsNotCleanEOF guards the shared cursor's truncation
// detection: a body that stops at a boundary line without the closing
// delimiter must not look complete.
func TestTruncatedMultipartIsNotCleanEOF(t *testing.T) {
	truncated := "--B\r\nContent-Type: application/dicom+json\r\n\r\n[]\r\n--B\r\n"
	p, err := New(resp(`multipart/related; boundary=B; type="application/dicom+json"`, truncated))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if _, _, err := p.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	_, _, err = p.Next()
	if err == nil {
		t.Fatal("truncated body reported a clean end")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestCompleteMultipartEndsCleanly(t *testing.T) {
	body := "--B\r\nContent-Type: application/dicom+json\r\n\r\n[]\r\n--B--\r\n"
	p, err := New(resp(`multipart/related; boundary=B; type="application/dicom+json"`, body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if _, _, err := p.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, _, err := p.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if _, _, err := p.Next(); err != io.EOF {
		t.Errorf("repeated Next = %v, want io.EOF", err)
	}
}

func TestCloseOnNilResponseIsSafe(t *testing.T) {
	var p Parts
	if err := p.Close(); err != nil {
		t.Errorf("Close on the zero value = %v, want nil", err)
	}
}

// TestTrackerDetectsSplitDelimiter guards the tail-buffer logic: the
// terminating delimiter may straddle two Read calls.
func TestTrackerDetectsSplitDelimiter(t *testing.T) {
	body := "PAYLOAD--BND--"
	tr := NewTracker(strings.NewReader(body), "BND")
	// Read one byte at a time so the delimiter is always split.
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := tr.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	if string(out) != body {
		t.Errorf("tracker altered the stream: %q, want %q", out, body)
	}
	if !tr.Complete() {
		t.Error("Complete() = false; the split delimiter was not detected")
	}
}

func TestTrackerWithoutDelimiter(t *testing.T) {
	tr := NewTracker(strings.NewReader("no closing delimiter here"), "BND")
	if _, err := io.ReadAll(tr); err != nil {
		t.Fatal(err)
	}
	if tr.Complete() {
		t.Error("Complete() = true for a stream without the delimiter")
	}
}
