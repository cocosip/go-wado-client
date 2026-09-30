// Command wadouri-example demonstrates the complete WADO-URI client surface:
// both transactions (Retrieve DICOM Instance and Retrieve Rendered
// Instance), the full Request parameter set, local validation, and the
// construction variants.
//
// Run it against a real gateway (the client only talks when a call is made):
//
//	go run ./examples/wadouri -endpoint https://gw.example.com/api/wado/H0001/wado-uri \
//		-study 1.2.840.113619.2.1.1.1 -series 1.2.840.113619.2.1.1.2 -object 1.2.840.113619.2.1.1.3
//
// WADO-URI has no resource path at all: the endpoint IS the service URL
// (for the deployment route api/wado/{hospitalCode}/wado-uri the endpoint is
// that route with the hospital code filled in).
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

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/wadouri"
)

func main() {
	var (
		endpoint = flag.String("endpoint", "https://gw.example.com/api/wado/H0001/wado-uri", "WADO-URI service URL")
		study    = flag.String("study", "1.2.840.113619.2.1.1.1", "Study Instance UID")
		series   = flag.String("series", "1.2.840.113619.2.1.1.2", "Series Instance UID")
		object   = flag.String("object", "1.2.840.113619.2.1.1.3", "SOP Instance UID")
		outDir   = flag.String("out", "wadouri-out", "directory for retrieved files")
		insecure = flag.Bool("insecure", false, "skip TLS verification (hospital self-signed certificates)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := run(ctx, *endpoint, *study, *series, *object, *outDir, *insecure); err != nil {
		var se *wado.StatusError
		if errors.As(err, &se) {
			log.Printf("server responded %s body=%.200s", se.Status, se.Body)
		}
		log.Fatal(err)
	}
}

func run(ctx context.Context, endpoint, studyUID, seriesUID, objectUID, outDir string, insecure bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Common path: one client per endpoint.
	u, err := wadouri.New(endpoint, clientOptions(insecure)...)
	if err != nil {
		return err
	}
	fmt.Println("endpoint:", u.Endpoint())

	// Assembly path: build the shared core explicitly so several clients
	// (here two hospitals' endpoints, in production) share one connection
	// pool — this is what multi.Registry uses internally.
	shared := wado.NewCore(clientOptions(insecure)...)
	u2, err := wadouri.NewWithCore(shared, endpoint)
	if err != nil {
		return err
	}

	// Fork: same assembly, replaced endpoint (another hospital), plus a
	// per-target option.
	u3, err := u.Fork(endpoint, wado.WithUserAgent("wadouri-example/rendered"))
	if err != nil {
		return err
	}

	// --- Local validation: no network traffic on rejection. ---
	// The window pair requires a rendered contentType, so this request is
	// rejected before it is sent.
	wc, ww := 40.0, 400.0
	if _, err := u.Retrieve(ctx, wadouri.Request{
		StudyUID: studyUID, SeriesUID: seriesUID, ObjectUID: objectUID,
		WindowCenter: &wc, WindowWidth: &ww,
	}); !errors.Is(err, wado.ErrInvalidRequest) {
		return fmt.Errorf("expected local validation error, got %v", err)
	}
	fmt.Println("local validation: windowing without a rendered contentType correctly rejected")

	// --- Transaction 1: Retrieve DICOM Instance (u2, shared-core client). ---
	resp, err := u2.Retrieve(ctx, wadouri.Request{
		StudyUID: studyUID, SeriesUID: seriesUID, ObjectUID: objectUID,
		TransferSyntax: "1.2.840.10008.1.2.1", // request Explicit VR Little Endian transcoding
		Charset:        "ISO_IR 100",
	})
	if err != nil {
		return fmt.Errorf("retrieve dicom: %w", err)
	}
	defer func() { _ = resp.Close() }()
	fmt.Printf("dicom response: %s isDICOM=%v\n", resp.ContentType, resp.IsDICOM())
	name, err := saveReader(outDir, objectUID+".dcm", resp.Body)
	if err != nil {
		return err
	}
	fmt.Println("dicom saved:", name, "| server header:", resp.Header.Get("Content-Type"))

	// --- Transaction 2: Retrieve Rendered Instance (u3, forked client) ---
	// with the full rendered parameter set.
	// Presentation state alternative (mutually exclusive with the window
	// pair; requires an existing PR object on the server):
	//   PresentationUID:       "1.2.840.10008.5.1.4.1.1.11.1",
	//   PresentationSeriesUID: "1.2.3",
	rresp, err := u3.Retrieve(ctx, wadouri.Request{
		StudyUID: studyUID, SeriesUID: seriesUID, ObjectUID: objectUID,
		ContentType:  "image/jpeg", // selects the rendered transaction
		FrameNumber:  1,
		ImageQuality: 90,                // 1..100
		Rows:         512, Columns: 512, // set together
		Region:       &[4]float64{0.1, 0.1, 0.9, 0.9}, // xmin,ymin,xmax,ymax normalized
		WindowCenter: &wc, WindowWidth: &ww,
		Annotation: []string{wadouri.AnnotationPatient, wadouri.AnnotationTechnique},
		Anonymize:  true,                              // burn in anonymity=yes (anonymize with modern names)
		Extra:      url.Values{"caller": {"example"}}, // pass-through for private gateway parameters
	})
	if err != nil {
		return fmt.Errorf("retrieve rendered: %w", err)
	}
	defer func() { _ = rresp.Close() }()
	fmt.Printf("rendered response: %s isDICOM=%v\n", rresp.ContentType, rresp.IsDICOM())
	name, err = saveReader(outDir, "wadouri-rendered.jpg", rresp.Body)
	if err != nil {
		return err
	}
	fmt.Println("rendered saved:", name)

	// WithModernParamNames() switches the 2023+ parameter naming
	// (anonymize instead of anonymity etc.) — construct the client with it
	// when the gateway speaks the newer standard:
	//   u, err := wadouri.New(endpoint, append(clientOptions(insecure), wado.WithModernParamNames())...)
	return nil
}

// clientOptions assembles the transport; the commented lines show the
// remaining knobs (identical to the WADO-RS side).
func clientOptions(insecure bool) []wado.Option {
	opts := []wado.Option{
		wado.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))),
		wado.WithRetry(wado.RetryPolicy{
			MaxAttempts:    3,
			InitialBackoff: 200 * time.Millisecond,
			MaxBackoff:     2 * time.Second,
			Jitter:         100 * time.Millisecond,
		}),
	}
	if insecure {
		opts = append(opts, wado.WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}))
	}
	// Also available:
	//   wado.WithBasicAuth("user", "pass")
	//   wado.WithBearerTokenSource(wado.StaticToken("eyJ..."))
	//   wado.WithRequestEditor(func(r *http.Request) error { ... })
	//   wado.WithHTTPClient(customClient)
	//   wado.WithLenientUID()
	//   wado.WithModernParamNames()
	//   wado.WithLogHandler(handler)
	return opts
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
