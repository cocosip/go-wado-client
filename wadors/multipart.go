package wadors

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// Multipart is a streaming cursor over a WADO-RS multipart/related response:
// parts are consumed while downloading, so memory usage is independent of
// the total response size. Close must be called when done.
// Non-multipart responses are tolerated as a single part (some servers are
// not strictly conformant).
type Multipart struct {
	resp          *http.Response
	mr            *multipart.Reader
	single        bool
	singleCT      string
	PartMediaType string // the type parameter of the response Content-Type (per-part media type)
	next          int
}

func newMultipart(resp *http.Response) (*Multipart, error) {
	ct := resp.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt, params = ct, map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("wadors: multipart response without boundary: %q", ct)
		}
		return &Multipart{
			resp:          resp,
			mr:            multipart.NewReader(resp.Body, boundary),
			PartMediaType: params["type"],
		}, nil
	}
	return &Multipart{resp: resp, single: true, singleCT: mt, PartMediaType: mt}, nil
}

// Next returns the next part; it returns io.EOF when the response is
// exhausted.
func (m *Multipart) Next() (*Part, error) {
	if m.single {
		if m.next > 0 {
			return nil, io.EOF
		}
		m.next = 1
		return &Part{r: m.resp.Body, ct: m.singleCT, n: 1}, nil
	}
	m.next++
	p, err := m.mr.NextRawPart()
	if err != nil {
		// mime/multipart may wrap io.EOF on repeated exhaustion; normalize
		// it so callers always see plain io.EOF at the end.
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	ct := p.Header.Get("Content-Type")
	if ct == "" {
		ct = m.PartMediaType
	}
	return &Part{
		r:   p,
		ct:  ct,
		loc: p.Header.Get("Content-Location"),
		n:   m.next,
	}, nil
}

// Parts iterates in iterator style (Go 1.23+); breaking out of the loop
// stops consumption. Errors encountered during iteration are yielded as the
// second value.
func (m *Multipart) Parts() iter.Seq2[*Part, error] {
	return func(yield func(*Part, error) bool) {
		for {
			p, err := m.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(p, nil) {
				return
			}
		}
	}
}

// ReadAll reads the content of every part (convenience for small payloads;
// stream via Parts for large ones).
func (m *Multipart) ReadAll() ([][]byte, error) {
	var out [][]byte
	for p, err := range m.Parts() {
		if err != nil {
			return out, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return out, err
		}
		out = append(out, b)
	}
	return out, nil
}

// WriteToDir writes each part to dir as a .dcm file; the file name prefers
// the trailing segment of Content-Location (usually the SOP Instance UID)
// and falls back to the part index. It returns the paths written.
func (m *Multipart) WriteToDir(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var files []string
	written := map[string]bool{}
	for p, err := range m.Parts() {
		if err != nil {
			return files, err
		}
		full := filepath.Join(dir, partFilename(p))
		if written[full] {
			// Duplicate Content-Location (a server returning the same
			// instance twice): fall back to the unique part index instead of
			// silently overwriting the earlier part. The indexed name can
			// never collide in turn — "part-..." is not a valid UID.
			full = filepath.Join(dir, fmt.Sprintf("part-%06d.dcm", p.n))
		}
		f, err := os.Create(full)
		if err != nil {
			return files, err
		}
		_, cpErr := io.Copy(f, p)
		clErr := f.Close()
		if cpErr != nil {
			_ = os.Remove(full) // never leave a truncated file behind
			return files, cpErr
		}
		if clErr != nil {
			return files, clErr
		}
		written[full] = true
		files = append(files, full)
	}
	return files, nil
}

// Header returns the header of the underlying HTTP response (warning
// headers etc.).
func (m *Multipart) Header() http.Header { return m.resp.Header }

// Close closes the underlying response.
func (m *Multipart) Close() error { return m.resp.Body.Close() }

// Part is a single response part: it implements io.Reader and exposes the
// media type and Content-Location.
type Part struct {
	r   io.Reader
	ct  string
	loc string
	n   int
}

func (p *Part) Read(b []byte) (int, error) { return p.r.Read(b) }

// ContentType returns the media type of the part.
func (p *Part) ContentType() string { return p.ct }

// ContentLocation returns the Content-Location of the part (servers usually
// point it at the instance resource URI).
func (p *Part) ContentLocation() string { return p.loc }

// Index returns the 1-based part index.
func (p *Part) Index() int { return p.n }

func partFilename(p *Part) string {
	if loc := p.ContentLocation(); loc != "" {
		// Content-Location may carry a query or fragment; only the path's
		// trailing segment can name a file.
		if u, err := url.Parse(loc); err == nil {
			loc = u.Path
		}
		seg := loc[strings.LastIndexByte(loc, '/')+1:]
		seg = strings.TrimSuffix(seg, ".dcm")
		if seg != "" && wado.ValidateUID(seg) == nil {
			return seg + ".dcm"
		}
	}
	return fmt.Sprintf("part-%06d.dcm", p.n)
}
