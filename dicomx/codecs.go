package dicomx

// Blank imports registering the go-dicom-codecs decoder set into go-dicom's
// global registry. Most of these packages self-register in their init();
// jpeg/standard is the shared base implementation pulled in by the other JPEG
// codecs and registers no codec of its own.
// Coverage: RLE, JPEG Baseline/Extended/Lossless families, JPEG-LS,
// JPEG 2000 (incl. HTJ2K).
import (
	// The blank imports self-register the go-dicom-codecs decoders through
	// each package's init(); a missing import means the corresponding
	// transfer syntax cannot be decoded.
	_ "github.com/cocosip/go-dicom-codecs/jpeg/baseline"       // JPEG Baseline (1.2.840.10008.1.2.4.50)
	_ "github.com/cocosip/go-dicom-codecs/jpeg/extended"       // JPEG Extended (1.2.840.10008.1.2.4.51)
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless"       // JPEG Lossless (1.2.840.10008.1.2.4.57)
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless14sv1"  // JPEG Lossless, Selection Value 1 (1.2.840.10008.1.2.4.70)
	_ "github.com/cocosip/go-dicom-codecs/jpeg/standard"       // shared JPEG base implementation (no codec registration)
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/htj2k"      // High-Throughput JPEG 2000 (1.2.840.10008.1.2.4.201/202)
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossless"   // JPEG 2000 lossless (1.2.840.10008.1.2.4.90)
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossy"      // JPEG 2000 lossy (1.2.840.10008.1.2.4.91)
	_ "github.com/cocosip/go-dicom-codecs/jpegls/lossless"     // JPEG-LS lossless (1.2.840.10008.1.2.4.80)
	_ "github.com/cocosip/go-dicom-codecs/jpegls/nearlossless" // JPEG-LS near-lossless (1.2.840.10008.1.2.4.81)
	_ "github.com/cocosip/go-dicom-codecs/rle"                 // RLE lossless (1.2.840.10008.1.2.5)
)
