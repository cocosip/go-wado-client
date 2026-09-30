// Command multi-example wires the hospital route template
//
//	api/wado/{hospitalCode}/wado-rs
//	api/wado/{hospitalCode}/wado-uri
//
// into a multi.Registry, then walks both clients of one hospital: WADO-RS
// metadata + rendered image, WADO-URI DICOM retrieval. The full method
// surface of each client is demonstrated in examples/wadors and
// examples/wadouri.
//
// Run it against a real gateway (the registry itself never talks on the
// network — only the retrieval calls do):
//
//	go run ./examples/multi -gateway https://gw.example.com -hospital H0001 \
//		-study 1.2.840.113619.2.1.1.1
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
	"os"
	"path/filepath"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/tag"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/multi"
	"github.com/cocosip/go-wado-client/wadors"
	"github.com/cocosip/go-wado-client/wadouri"
)

func main() {
	var (
		gateway  = flag.String("gateway", "https://gw.example.com", "standard base address: scheme://host[/static-prefix], no service routing")
		hospital = flag.String("hospital", "H0001", "hospital code — the business key of this registry")
		study    = flag.String("study", "1.2.840.113619.2.1.1.1", "Study Instance UID")
		outDir   = flag.String("out", "multi-out", "directory for retrieved files")
		insecure = flag.Bool("insecure", false, "skip TLS verification (hospital self-signed certificates)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := run(ctx, *gateway, *hospital, *study, *outDir, *insecure); err != nil {
		var se *wado.StatusError
		if errors.As(err, &se) {
			log.Printf("server responded %s body=%.200s", se.Status, se.Body)
		}
		log.Fatal(err)
	}
}

func run(ctx context.Context, gateway, hospital, studyUID, outDir string, insecure bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// The registry owns one shared core (one connection pool for the whole
	// process); every hospital's clients are derived from it.
	//
	// Template fills {hospitalCode} in both routes — independently — from
	// the business key, then joins each route onto the base address.
	reg := multi.NewRegistry(
		multi.Template(gateway,
			multi.Routes{
				RS:  "/api/wado/{hospitalCode}/wado-rs",
				URI: "/api/wado/{hospitalCode}/wado-uri",
			},
			func(code string) map[string]string {
				return map[string]string{"hospitalCode": code}
			},
		),
		clientOptions(insecure)...,
	)
	// Alternative resolvers when the routes do not follow one template:
	//   multi.Static[string](map[string]multi.Endpoint{
	//       "H0001": {Base: gateway,
	//                 RSRoute:  "/api/wado/H0001/wado-rs",
	//                 URIRoute: "/api/wado/H0001/wado-uri"},
	//   })
	//   multi.ResolverFunc[string](func(ctx, key) (multi.Endpoint, error) { ...DB lookup... })

	// Lazily builds and caches the Gateway for this hospital. No network
	// happens here — clients are configuration holders.
	g, err := reg.Client(ctx, hospital)
	if err != nil {
		return err
	}
	fmt.Printf("hospital %s:\n  RS:  %s\n  URI: %s\n", hospital, g.RS.BaseURL(), g.URI.Endpoint())

	// A second hospital derives from the same template and pool; repeated
	// calls hit the cache and return the identical Gateway.
	other, err := reg.Client(ctx, "H0002")
	if err != nil {
		return err
	}
	fmt.Println("hospital H0002 RS:", other.RS.BaseURL())
	// After a route change or credential rotation:
	//   reg.Invalidate(hospital) — the next Client call rebuilds it.

	// --- WADO-RS: metadata first, then a rendered image. ---
	dss, err := g.RS.StudyMetadata(ctx, studyUID)
	if err != nil {
		return fmt.Errorf("rs study metadata: %w", err)
	}
	if len(dss) == 0 {
		return errors.New("study has no instances")
	}
	seriesUID, _ := dss[0].GetString(tag.SeriesInstanceUID)
	sopUID, _ := dss[0].GetString(tag.SOPInstanceUID)
	if pn, ok := dss[0].GetString(tag.PatientName); ok {
		fmt.Println("rs metadata: first instance patient =", pn)
	}

	img, err := g.RS.RetrieveRenderedInstance(ctx, studyUID, seriesUID, sopUID,
		wadors.WithRenderedFormat("image/png"),
		wadors.WithWindow(40, 400),
	)
	if err != nil {
		return fmt.Errorf("rs rendered: %w", err)
	}
	name, err := saveReader(outDir, "rs-rendered.png", img.Body)
	_ = img.Close()
	if err != nil {
		return err
	}
	fmt.Println("rs rendered saved:", name)

	// --- WADO-URI: the same instance through the legacy transaction. ---
	resp, err := g.URI.Retrieve(ctx, wadouri.Request{
		StudyUID: studyUID, SeriesUID: seriesUID, ObjectUID: sopUID,
	})
	if err != nil {
		return fmt.Errorf("uri retrieve: %w", err)
	}
	defer func() { _ = resp.Close() }()
	name, err = saveReader(outDir, "uri-instance.dcm", resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("uri dicom saved: %s (isDICOM=%v)\n", name, resp.IsDICOM())

	return nil
}

// clientOptions assembles the registry-wide defaults; every hospital client
// inherits them, and Endpoint.Options can override per target.
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
	// Also available (per target via Endpoint.Options as well):
	//   wado.WithBasicAuth / WithBearerTokenSource / WithRequestEditor
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
