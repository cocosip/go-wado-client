// Package dicomx is the integration glue between the WADO client and
// cocosip/go-dicom (plus go-dicom-codecs).
//
// DICOM parsing/serialization is fully delegated to go-dicom and never
// reimplemented here:
//   - metadata (dicom+json) goes through go-dicom's serialization package
//     directly from the wadors package;
//   - DICOM file parsing goes through this package's streaming bridge over
//     multipart responses (Datasets);
//   - pixel codecs (RLE/JPEG/JPEG-LS/JPEG 2000/HTJ2K) are registered into
//     go-dicom's global registry via the blank imports in codecs.go.
//
// Omitting this package keeps the binary light when no pixel codecs are
// needed.
package dicomx

import (
	"fmt"
	"iter"

	dparser "github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-wado-client/wadors"
)

// Datasets parses each DICOM part of a WADO-RS multipart response in a
// streaming fashion. opts are passed through to the go-dicom parser (e.g.
// parser.WithReadOption(parser.SkipLargeTags)). Iteration stops at the first
// part that fails to parse, yielding that error.
func Datasets(mp *wadors.Multipart, opts ...dparser.Option) iter.Seq2[*dparser.ParseResult, error] {
	return func(yield func(*dparser.ParseResult, error) bool) {
		for part, err := range mp.Parts() {
			if err != nil {
				yield(nil, err)
				return
			}
			res, err := dparser.Parse(part, opts...)
			if err != nil {
				yield(nil, fmt.Errorf("dicomx: parse part %d: %w", part.Index(), err))
				return
			}
			if !yield(res, nil) {
				return
			}
		}
	}
}
