// Package dicomjson provides the dicom+json (PS3.18 Annex F) decoding
// pieces shared by the metadata (WADO-RS) and search (QIDO-RS) clients:
// streaming item decoding of a response body and the PS3.18-relative
// BulkDataURI rewriting. Interpreting the datasets themselves stays with
// go-dicom in the callers.
//
// Errors carry no prefix: the owning service package wraps them with its
// own context.
package dicomjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
)

// Items streams the dataset items of one dicom+json response body: the
// elements of a top-level JSON array (the conformant form — PS3.18 Annex F
// encodes multiple instances as one array) or a single bare object some
// servers send per part. Array elements are decoded one at a time, so peak
// memory stays at a single item no matter how long the array is and the
// first item is available before the body has fully arrived.
type Items struct {
	dec   *json.Decoder
	array bool
	done  bool
}

// NewItems classifies the body by its first non-whitespace byte and wraps it
// in a streaming decoder. An empty body yields no items.
func NewItems(r io.Reader) (*Items, error) {
	br := bufio.NewReader(r)
	first, err := firstNonSpace(br)
	if errors.Is(err, io.EOF) {
		return &Items{done: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if first != '[' && first != '{' {
		return nil, fmt.Errorf("unexpected character %q", first)
	}
	it := &Items{
		dec:   json.NewDecoder(io.MultiReader(bytes.NewReader([]byte{first}), br)),
		array: first == '[',
	}
	if it.array {
		// Consume the opening bracket so More()/Decode() address the array
		// elements, not the array as a single value.
		if _, err := it.dec.Token(); err != nil {
			return nil, err
		}
	}
	return it, nil
}

// Next returns the next raw item; ok is false once the body is exhausted.
func (it *Items) Next() (raw json.RawMessage, ok bool, err error) {
	if it.done {
		return nil, false, nil
	}
	if !it.array {
		it.done = true
		if err := it.dec.Decode(&raw); err != nil {
			return nil, false, err
		}
		if err := requireEOF(it.dec); err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}
	if !it.dec.More() {
		it.done = true
		// Consume the closing bracket, then reject trailing content like a
		// plain json.Unmarshal of the whole body would.
		if _, err := it.dec.Token(); err != nil {
			return nil, false, err
		}
		if err := requireEOF(it.dec); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := it.dec.Decode(&raw); err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// requireEOF verifies the decoder is exhausted after a complete top-level
// value, mirroring the trailing-content strictness of json.Unmarshal.
func requireEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing content after top-level value")
	}
	return nil
}

// firstNonSpace returns the first byte of br that is not JSON whitespace.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return b, nil
		}
	}
}

// ResolveBulkDataURIs rewrites relative BulkDataURI values in a dicom+json
// item to absolute URLs. PS3.18 resolves the relative form against the URL
// of the request that produced the item — this is URL rewriting only;
// every aspect of interpreting dicom+json stays with the caller.
func ResolveBulkDataURIs(raw json.RawMessage, base *url.URL) (json.RawMessage, error) {
	// Fast paths: items not mentioning the key, and items whose every
	// BulkDataURI is scheme-absolute, skip the decode / walk / re-encode
	// round trip entirely (that pass costs several times the item size and
	// dominates response parsing when pixel data is bulk-referenced). Both
	// probes are safe superset filters: false positives just fall through to
	// the walk, which finds nothing to change.
	if !bytes.Contains(raw, []byte("BulkDataURI")) {
		return raw, nil
	}
	if !hasRelativeBulkDataURI(raw) {
		return raw, nil
	}
	var v any
	// UseNumber keeps numeric literals verbatim across the decode / re-encode
	// round trip: DICOM JSON numbers (DS precision, large IS/UL values) must
	// not be perturbed by float64 conversion.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if !resolveBulkDataValue(v, base) {
		return raw, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hasRelativeBulkDataURI reports whether raw carries a BulkDataURI value
// that is not scheme-absolute — i.e. anything the JSON walk might rewrite.
//
// For each occurrence of the key, the value bytes are classified per RFC
// 3986: in an absolute URI a valid scheme ("http:", "ftp:", ...) always
// precedes the first '/', '?', '#' or end; a conforming relative reference
// can never have one there. Any uncertain shape (odd spacing, escapes, a
// colon that is not a valid scheme) reports true so the authoritative walk
// decides.
func hasRelativeBulkDataURI(raw []byte) bool {
	const key = `"BulkDataURI"`
	for i := 0; i < len(raw); {
		j := bytes.Index(raw[i:], []byte(key))
		if j < 0 {
			return false
		}
		i += j + len(key)
		// Skip JSON whitespace, expect ':', skip whitespace again, expect
		// the opening quote of the value; anything else moves on to the next
		// occurrence (e.g. the word appearing inside another string value).
		k := skipJSONSpace(raw, i)
		if k >= len(raw) || raw[k] != ':' {
			continue
		}
		k = skipJSONSpace(raw, k+1)
		if k >= len(raw) || raw[k] != '"' {
			continue
		}
		k++
		schemeOK := true // value so far is a valid scheme prefix
		firstByte := true
		schemed := false
		for k < len(raw) {
			c := raw[k]
			if c == ':' {
				if schemeOK && !firstByte {
					schemed = true // valid scheme before the colon
				}
				break
			}
			if c == '/' || c == '?' || c == '#' || c == '"' {
				break // path, query, fragment or end of a relative reference
			}
			schemeOK = schemeOK && isSchemeChar(c, firstByte)
			firstByte = false
			k++
		}
		if !schemed {
			// Relative reference, empty value, invalid scheme, or an
			// inconclusive scan: the walk is the authority.
			return true
		}
		// Absolute value; keep scanning for further occurrences.
	}
	return false
}

// skipJSONSpace returns the index of the first byte at or after i that is
// not a JSON whitespace byte.
func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// isSchemeChar reports whether c may appear in a URI scheme, where first
// selects the stricter first-character rule (letters only) of RFC 3986.
func isSchemeChar(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9', c == '+', c == '-', c == '.':
		return !first
	default:
		return false
	}
}

// resolveBulkDataValue walks decoded JSON (maps/arrays) rewriting relative
// "BulkDataURI" string values; it reports whether anything changed.
func resolveBulkDataValue(v any, base *url.URL) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "BulkDataURI" {
				if s, ok := val.(string); ok && s != "" {
					if u, err := url.Parse(s); err == nil && !u.IsAbs() {
						if abs := base.ResolveReference(u); abs != nil {
							t[k] = abs.String()
							changed = true
						}
					}
				}
				continue
			}
			if resolveBulkDataValue(val, base) {
				changed = true
			}
		}
	case []any:
		for _, item := range t {
			if resolveBulkDataValue(item, base) {
				changed = true
			}
		}
	}
	return changed
}
