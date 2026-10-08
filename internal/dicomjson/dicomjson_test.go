// Package dicomjson tests the shared dicom+json item streaming and
// BulkDataURI rewriting moved out of the wadors metadata client.
package dicomjson

import (
	"bytes"
	"encoding/json"
	"net/url"
	"testing"
)

// resolvedBulk7FE00010 is the absolute form of "./bulkdata/7FE00010"
// resolved against the metadata request URL used in the tests.
const resolvedBulk7FE00010 = "https://h/dicomweb/studies/1.2/bulkdata/7FE00010"

// TestResolveBulkDataURIsJSONNumberPrecision guards the decode / re-encode
// round trip: numeric literals must survive verbatim (no float64 round-off)
// while the relative BulkDataURI is rewritten.
func TestResolveBulkDataURIsJSONNumberPrecision(t *testing.T) {
	base, err := url.Parse("https://h/dicomweb/studies/1.2/metadata")
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"00181060":{"vr":"IS","Value":[9007199254740993]},` +
		`"00209222":{"vr":"DS","Value":[0.1234567890123456789]},` +
		`"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/7FE00010"}}`)

	out, err := ResolveBulkDataURIs(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{"9007199254740993", "0.1234567890123456789"} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Errorf("numeric literal %s lost in %s", literal, out)
		}
	}
	want := resolvedBulk7FE00010
	if !bytes.Contains(out, []byte(`"BulkDataURI":"`+want+`"`)) {
		t.Errorf("BulkDataURI not resolved in %s", out)
	}
}

// TestResolveBulkDataURIsJSONProbe pins the superset probes of
// resolveBulkDataURIsJSON: items whose every BulkDataURI is scheme-absolute
// (or absent) come back byte-identical without the decode/rewrite/re-encode
// pass, while every relative form — including bare relative paths and
// query/fragment references — is rewritten.
func TestResolveBulkDataURIsJSONProbe(t *testing.T) {
	base, err := url.Parse("https://h/dicomweb/studies/1.2/metadata")
	if err != nil {
		t.Fatal(err)
	}

	rewritten := map[string]string{
		`{"7FE00010":{"vr":"OW","BulkDataURI":"./bulkdata/7FE00010"}}`:                           resolvedBulk7FE00010,
		`{"7FE00010":{"vr":"OW","BulkDataURI":"/dicomweb/bulk/1"}}`:                              "https://h/dicomweb/bulk/1",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"../pixeldata/9"}}`:                                "https://h/dicomweb/studies/pixeldata/9",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"bulkdata/7FE00010"}}`:                             resolvedBulk7FE00010,
		`{"7FE00010":{"vr":"OW","BulkDataURI":"bulk?x:y"}}`:                                      "https://h/dicomweb/studies/1.2/bulk?x:y",
		`{"7FE00010":{"vr":"OW","BulkDataURI":"part#f"}}`:                                        "https://h/dicomweb/studies/1.2/part#f",
		`{"7FE00010" : {"vr":"OW", "BulkDataURI" : "./spaced/1"}}`:                               "https://h/dicomweb/studies/1.2/spaced/1",
		`{"00089123":{"vr":"SQ","Value":[{"7FE00010":{"vr":"OW","BulkDataURI":"./nested/1"}}]}}`: "https://h/dicomweb/studies/1.2/nested/1",
		// A relative value after an absolute one exercises the probe's
		// continue-after-scheme path.
		`{"A":{"vr":"OW","BulkDataURI":"https://elsewhere/x"},"B":{"vr":"OW","BulkDataURI":"./after/1"}}`: "https://h/dicomweb/studies/1.2/after/1",
	}
	for raw, want := range rewritten {
		out, err := ResolveBulkDataURIs(json.RawMessage(raw), base)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !bytes.Contains(out, []byte(`"BulkDataURI":"`+want+`"`)) {
			t.Errorf("%s: BulkDataURI not rewritten to %q in %s", raw, want, out)
		}
	}

	untouched := []string{
		// Fast path: no key at all.
		`{"00080018":{"vr":"UI","Value":["1.2.3"]},"7FE00010":{"vr":"OW","InlineBinary":"AAAA"}}`,
		// Single absolute value.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"https://h/dicomweb/bulk/1"}}`,
		// Two absolute values: the probe must keep scanning past the first.
		`{"A":{"vr":"OW","BulkDataURI":"https://h/a"},"B":{"vr":"OW","BulkDataURI":"http://x/b"}}`,
		// A non-http scheme is absolute too (rejected later at fetch time).
		`{"7FE00010":{"vr":"OW","BulkDataURI":"ftp://h/bulk"}}`,
		// A colon inside the query of a schemed URI does not make it
		// relative; see the rewritten list for the relative query form.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"https://h/a?x:y"}}`,
		// Non-string value: the walk never rewrites it.
		`{"7FE00010":{"vr":"OW","BulkDataURI":42}}`,
		// Empty value: skipped by the walk.
		`{"7FE00010":{"vr":"OW","BulkDataURI":""}}`,
		// The word appearing inside another string value is not a key.
		`{"00311030":{"vr":"LO","Value":["mentions BulkDataURI in prose"]}}`,
		// url.Parse rejects this shape; the probe defers to the walk, which
		// leaves it untouched.
		`{"7FE00010":{"vr":"OW","BulkDataURI":"1a:b"}}`,
	}
	for _, raw := range untouched {
		out, err := ResolveBulkDataURIs(json.RawMessage(raw), base)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !bytes.Equal(out, []byte(raw)) {
			t.Errorf("%s: unexpectedly rewritten to %s", raw, out)
		}
	}
}
