package qido

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/serialization"

	"github.com/cocosip/go-wado-client/internal/dicomjson"
	"github.com/cocosip/go-wado-client/internal/multipartx"
)

// Results is the aggregate reply of a search: the matching datasets plus the
// response header (the Warning 299 additional-results signal, if any, rides
// there). A search without matches yields an empty, non-nil Datasets slice —
// 204 No Content is a success per PS3.18 §10.6.3, not an error.
type Results struct {
	Datasets []*dataset.Dataset
	Header   http.Header
}

// AdditionalResults reports the "N additional results that can be
// requested" count announced by the server's Warning 299 header (PS3.18
// §10.6): ok is false when the server did not announce more results. Paging
// is client-driven: request the next page with Query.Offset advanced by the
// consumed page size.
func (r *Results) AdditionalResults() (int64, bool) {
	return additionalResults(r.Header)
}

// ResultsStream is a streaming cursor over a search reply: response parts
// are read and parsed only as the caller advances, and each top-level JSON
// array is decoded element by element, so peak memory stays at a single
// dataset. Close must be called when done.
type ResultsStream struct {
	parts  *multipartx.Parts
	base   *url.URL
	header http.Header
	resp   *http.Response // the 204 form: kept open only to be closed
	items  *dicomjson.Items
	done   bool
}

// Header returns the header of the underlying HTTP response.
func (s *ResultsStream) Header() http.Header { return s.header }

// AdditionalResults reports the Warning 299 additional-results count; see
// Results.AdditionalResults.
func (s *ResultsStream) AdditionalResults() (int64, bool) {
	return additionalResults(s.header)
}

// Next returns the next matching dataset; it returns io.EOF when the reply
// is exhausted. Relative BulkDataURIs are resolved against the search
// request URL per PS3.18.
func (s *ResultsStream) Next() (*dataset.Dataset, error) {
	for {
		if s.items != nil {
			raw, ok, err := s.items.Next()
			if err != nil {
				return nil, fmt.Errorf("qido: decode search response: %w", err)
			}
			if ok {
				return parseDataset(raw, s.base)
			}
			s.items = nil
		}
		if s.done {
			return nil, io.EOF
		}
		r, ct, err := s.parts.Next()
		if errors.Is(err, io.EOF) {
			s.done = true
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		if err := guardXML(ct); err != nil {
			return nil, err
		}
		if s.items, err = dicomjson.NewItems(r); err != nil {
			return nil, fmt.Errorf("qido: decode search response: %w", err)
		}
	}
}

// Close closes the underlying response.
func (s *ResultsStream) Close() error {
	if s.parts != nil {
		return s.parts.Close()
	}
	if s.resp != nil {
		return s.resp.Body.Close()
	}
	return nil
}

// collect drains the stream into the aggregate slice (never nil).
func (s *ResultsStream) collect() ([]*dataset.Dataset, error) {
	out := make([]*dataset.Dataset, 0, 16)
	for {
		ds, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ds)
	}
}

// parseDataset parses one dicom+json item into a go-dicom dataset, resolving
// relative BulkDataURIs against the search request URL first.
func parseDataset(raw json.RawMessage, base *url.URL) (*dataset.Dataset, error) {
	fixed, err := dicomjson.ResolveBulkDataURIs(raw, base)
	if err != nil {
		return nil, fmt.Errorf("qido: rewrite BulkDataURI: %w", err)
	}
	ds, err := serialization.FromJSON(fixed)
	if err != nil {
		return nil, fmt.Errorf("qido: parse dicom+json: %w", err)
	}
	return ds, nil
}

// guardXML rejects an explicitly negotiated dicom+xml reply: this client
// parses dicom+json only (the search Accept is dicom+json by default; the
// only way to reach the XML form is a WithAccept override).
func guardXML(ct string) error {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil // no parseable type: the item decoder reports the real problem
	}
	if mt == "application/dicom+xml" ||
		(strings.HasPrefix(mt, "multipart/") && params["type"] == "application/dicom+xml") {
		return errors.New("qido: server answered application/dicom+xml; this client parses application/dicom+json only (do not negotiate dicom+xml via WithAccept)")
	}
	return nil
}

// additionalResults parses the Warning header for the 299 code and its "N
// additional results that can be requested" text; ok is false when no 299
// warning is present. Values of a single comma-joined Warning header beyond
// the first code are not inspected.
func additionalResults(h http.Header) (int64, bool) {
	for _, v := range h.Values("Warning") {
		code, rest, found := strings.Cut(strings.TrimSpace(v), " ")
		if !found || code != "299" {
			continue
		}
		rest = strings.Trim(strings.TrimSpace(rest), `"`)
		if m := additionalRe.FindStringSubmatch(rest); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			return n, true
		}
		return 0, true
	}
	return 0, false
}

// additionalRe extracts the result count from the Warning 299 text ("There
// are 42 additional results that can be requested").
var additionalRe = regexp.MustCompile(`(?i)(\d+)\s+additional`)
