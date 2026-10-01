// Command wadors-example demonstrates the complete WADO-RS client surface:
// study / series / instance retrieval, metadata, frames, rendered images,
// bulk data, options and error handling.
//
// Run it against a real gateway (the client only talks when a call is made):
//
//	go run ./examples/wadors -base https://gw.example.com/api/wado/H0001/wado-rs \
//		-study 1.2.840.113619.2.1.1.1 -out wadors-out
//
// -base is everything before the standard resource path (studies/...). For
// the deployment route api/wado/{hospitalCode}/wado-rs the base is the whole
// route with the hospital code filled in; see examples/multi for deriving
// per-hospital clients from the route template automatically.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/io/buffer"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/dicomx"
	"github.com/cocosip/go-wado-client/wadors"
)

// transferSyntaxExplicitVRLittleEndian asks the server to transcode to
// Explicit VR Little Endian.
const transferSyntaxExplicitVRLittleEndian = "1.2.840.10008.1.2.1"

func main() {
	var (
		base     = flag.String("base", "https://gw.example.com/api/wado/H0001/wado-rs", "WADO-RS base URL: everything before /studies/...")
		study    = flag.String("study", "1.2.840.113619.2.1.1.1", "Study Instance UID")
		outDir   = flag.String("out", "wadors-out", "directory for retrieved files")
		insecure = flag.Bool("insecure", false, "skip TLS verification (hospital self-signed certificates)")
	)
	flag.Parse()

	// The UID whitelist the client applies before every request is also
	// available directly.
	if err := wado.ValidateUID(*study); err != nil {
		log.Fatalf("invalid -study: %v", err)
	}

	c, err := wadors.New(*base, clientOptions(*insecure)...)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("base URL:", c.BaseURL())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := run(ctx, c, clientOptions(*insecure), *study, *outDir); err != nil {
		var se *wado.StatusError
		if errors.As(err, &se) {
			log.Printf("server responded %s body=%.200s", se.Status, se.Body)
		}
		log.Fatal(err)
	}
}

// clientOptions assembles the transport. Every knob is pluggable; the
// commented lines show the remaining options.
func clientOptions(insecure bool) []wado.Option {
	opts := []wado.Option{
		// Logging is strictly opt-in; without it the library is silent.
		wado.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))),
		// GETs are idempotent: retry transient failures with capped backoff.
		wado.WithRetry(wado.RetryPolicy{
			MaxAttempts:    3,
			InitialBackoff: 200 * time.Millisecond,
			MaxBackoff:     2 * time.Second,
			Jitter:         100 * time.Millisecond,
		}),
	}
	if insecure {
		// Hospital gateways commonly run self-signed certificates.
		opts = append(opts, wado.WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}))
	}
	// Further knobs (any combination):
	//   wado.WithBasicAuth("user", "pass")
	//   wado.WithBearerTokenSource(wado.StaticToken("eyJ..."))
	//   wado.WithBearerTokenSource(wado.TokenSourceFunc(func() (string, error) { ...refresh... }))
	//   wado.WithRequestEditor(func(r *http.Request) error { ...private signature headers... })
	//   wado.WithHTTPClient(customClient)   // proxying, global policies
	//   wado.WithLenientUID()               // private gateways with non-conformant UIDs
	//   wado.WithModernParamNames()         // annotation/window/iccprofile/anonymize naming
	//   wado.WithLogHandler(handler)        // == WithLogger(slog.New(handler))
	return opts
}

func run(ctx context.Context, c *wadors.Client, opts []wado.Option, studyUID, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// --- Study metadata first: cheap, and it drives everything below. ---
	dss, err := c.StudyMetadata(ctx, studyUID, wadors.WithCharset("ISO_IR 100"))
	if err != nil {
		return fmt.Errorf("study metadata: %w", err)
	}
	fmt.Printf("study has %d instances:\n", len(dss))
	for i, ds := range dss {
		printSummary(i, ds)
	}
	if len(dss) == 0 {
		return errors.New("study has no instances")
	}
	seriesUID, _ := dss[0].GetString(tag.SeriesInstanceUID)
	sopUID, _ := dss[0].GetString(tag.SOPInstanceUID)

	// --- Whole study: streamed straight to disk, memory stays flat. ---
	smp, err := c.RetrieveStudy(ctx, studyUID,
		wadors.WithTransferSyntax(transferSyntaxExplicitVRLittleEndian))
	if err != nil {
		return fmt.Errorf("retrieve study: %w", err)
	}
	defer func() { _ = smp.Close() }()
	fmt.Println("study part media type:", smp.PartMediaType)
	files, err := smp.WriteToDir(outDir) // named by SOP Instance UID (Content-Location)
	if err != nil {
		return fmt.Errorf("write study: %w", err)
	}
	fmt.Printf("study: wrote %d files\n", len(files))

	// --- Series metadata + manual multipart iteration. ---
	smds, err := c.SeriesMetadata(ctx, studyUID, seriesUID)
	if err != nil {
		return fmt.Errorf("series metadata: %w", err)
	}
	fmt.Printf("series %s has %d instances\n", seriesUID, len(smds))

	rmp, err := c.RetrieveSeries(ctx, studyUID, seriesUID,
		wadors.WithAccept(`multipart/related; type="application/dicom"`)) // WithAccept: full escape hatch
	if err != nil {
		return fmt.Errorf("retrieve series: %w", err)
	}
	defer func() { _ = rmp.Close() }()
	p, err := rmp.Next() // io.EOF marks the end of the response
	if errors.Is(err, io.EOF) {
		return errors.New("series response had no parts")
	}
	if err != nil {
		return fmt.Errorf("series part: %w", err)
	}
	fmt.Printf("series part #%d %s loc=%s\n", p.Index(), p.ContentType(), p.ContentLocation())
	// A real consumer keeps calling Next until io.EOF; stopping early is
	// fine — Close releases the connection.

	// --- Instance level, on a forked client (same assembly, new UA). ---
	ic, err := c.Fork(c.BaseURL(), wado.WithUserAgent("wadors-example/instance"))
	if err != nil {
		return fmt.Errorf("fork: %w", err)
	}
	if err := runInstance(ctx, ic, studyUID, seriesUID, sopUID, outDir); err != nil {
		return err
	}
	return runRendered(ctx, c, ic, opts, studyUID, seriesUID, sopUID, outDir)
}

// runInstance demonstrates the instance-level WADO-RS methods: metadata,
// retrieval (ReadAll and the dicomx parse bridge), frames and bulk data.
func runInstance(ctx context.Context, ic *wadors.Client, studyUID, seriesUID, sopUID, outDir string) error {
	ids, err := ic.InstanceMetadata(ctx, studyUID, seriesUID, sopUID)
	if err != nil {
		return fmt.Errorf("instance metadata: %w", err)
	}
	fmt.Printf("instance %s: %s\n", sopUID, dims(ids))

	imp, err := ic.RetrieveInstance(ctx, studyUID, seriesUID, sopUID)
	if err != nil {
		return fmt.Errorf("retrieve instance: %w", err)
	}
	defer func() { _ = imp.Close() }()
	parts, err := imp.ReadAll() // convenience for small payloads
	if err != nil {
		return fmt.Errorf("read instance: %w", err)
	}
	fmt.Printf("instance: %d part(s), first is %d bytes\n", len(parts), len(parts[0]))

	// Parse the same instance with go-dicom via the optional dicomx bridge.
	imp2, err := ic.RetrieveInstance(ctx, studyUID, seriesUID, sopUID)
	if err != nil {
		return fmt.Errorf("retrieve instance: %w", err)
	}
	defer func() { _ = imp2.Close() }()
	fmt.Println("parsing instance with go-dicom:")
	for res, err := range dicomx.Datasets(imp2) {
		if err != nil {
			return fmt.Errorf("parse instance: %w", err)
		}
		if pn, ok := res.Dataset.GetString(tag.PatientName); ok {
			fmt.Println("  patient:", pn)
		}
	}

	// --- Frames: parts stream out as raw pixel fragments. ---
	fmp, err := ic.RetrieveFrames(ctx, studyUID, seriesUID, sopUID, []int{3, 1, 1}) // → frames 1,3
	if err != nil {
		return fmt.Errorf("retrieve frames: %w", err)
	}
	defer func() { _ = fmp.Close() }()
	for p, err := range fmp.Parts() {
		if err != nil {
			return fmt.Errorf("frame part: %w", err)
		}
		fname, err := saveReader(outDir, fmt.Sprintf("frame-%02d.raw", p.Index()), p)
		if err != nil {
			return err
		}
		fmt.Println("frame saved:", fname)
	}

	// --- Bulk data: fetch the pixel data the metadata pointed at. ---
	if uri := pixelDataBulkURI(ids); uri != "" {
		rc, err := ic.FetchBulkData(ctx, uri)
		if err != nil {
			return fmt.Errorf("fetch bulk data: %w", err)
		}
		name, err := saveReader(outDir, "bulk-pixeldata.bin", rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		fmt.Println("bulk data saved:", name)
	} else {
		fmt.Println("pixel data is inline in the metadata (no BulkDataURI) — nothing to fetch")
	}
	return nil
}

// runRendered demonstrates both rendered transactions and every
// RenderedOption.
func runRendered(ctx context.Context, c, ic *wadors.Client, opts []wado.Option, studyUID, seriesUID, sopUID, outDir string) error {
	// Rendered on a separately assembled client: wado.NewCore +
	// NewWithCore is the explicit assembly multi.Registry uses internally,
	// handy when several clients must share one connection pool.
	core, err := wado.NewCore(opts...)
	if err != nil {
		return err
	}
	cc2, err := wadors.NewWithCore(core, c.BaseURL())
	if err != nil {
		return err
	}
	img, err := cc2.RetrieveRenderedInstance(ctx, studyUID, seriesUID, sopUID,
		wadors.WithRenderedFormat("image/png"),
		wadors.WithWindow(40, 400),
		wadors.WithViewport(512, 512),
		wadors.WithQuality(90),
		wadors.WithAnnotation("patient", "technique"),
		wadors.WithICCProfile("sRGB"),
	)
	if err != nil {
		return fmt.Errorf("rendered instance: %w", err)
	}
	defer func() { _ = img.Close() }()
	name, err := saveReader(outDir, "rendered-instance.png", img.Body)
	if err != nil {
		return err
	}
	fmt.Printf("rendered instance: %s (response %s) → %s\n",
		img.ContentType, img.Header.Get("Content-Type"), name)

	rimg, err := ic.RetrieveRenderedFrames(ctx, studyUID, seriesUID, sopUID, []int{1},
		wadors.WithRenderedFormat("image/jpeg"),
		wadors.WithRawQuery(url.Values{"quality": {"75"}}), // escape hatch: any private query parameter
	)
	if err != nil {
		return fmt.Errorf("rendered frames: %w", err)
	}
	defer func() { _ = rimg.Close() }()
	name, err = saveReader(outDir, "rendered-frame-1.jpg", rimg.Body)
	if err != nil {
		return err
	}
	fmt.Println("rendered frame saved:", name)
	return nil
}

func printSummary(i int, ds *dataset.Dataset) {
	sop, _ := ds.GetString(tag.SOPInstanceUID)
	name, _ := ds.GetString(tag.PatientName)
	modality, _ := ds.GetString(tag.Modality)
	fmt.Printf("  #%d sop=%s patient=%q modality=%s\n", i, orDash(sop), orDash(name), orDash(modality))
}

// dims renders the Rows x Columns of an image dataset, when present.
func dims(ds *dataset.Dataset) string {
	rows, errRows := ds.GetUInt16(tag.Rows, 0)
	cols, errCols := ds.GetUInt16(tag.Columns, 0)
	if errRows != nil || errCols != nil {
		return "rows/columns not in metadata"
	}
	return fmt.Sprintf("%dx%d px", rows, cols)
}

// pixelDataBulkURI extracts the BulkDataURI of the pixel data element, empty
// when the element is absent or served inline.
func pixelDataBulkURI(ds *dataset.Dataset) string {
	elem, ok := ds.Get(tag.PixelData)
	if !ok {
		return ""
	}
	bb, ok := elem.Buffer().(*buffer.BulkDataURIByteBuffer)
	if !ok {
		return ""
	}
	return bb.BulkDataURI()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// saveReader streams r into dir/name without buffering it in memory.
func saveReader(dir, name string, r io.Reader) (string, error) {
	full := filepath.Join(dir, name)
	f, err := os.Create(full)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(full)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return full, nil
}
