# go-wado-client

[![Go Reference](https://pkg.go.dev/badge/github.com/cocosip/go-wado-client.svg)](https://pkg.go.dev/github.com/cocosip/go-wado-client)

A DICOM WADO client library for Go, implementing both retrieval standards of
DICOM PS3.18 (Web Services):

- **WADO-RS** (`wadors`) — the Retrieve transaction of the Studies Service:
  Study/Series/Instance retrieval (streaming `multipart/related`), metadata
  (`application/dicom+json`), frame pixel data, rendered images and Bulk
  Data.
- **WADO-URI** (`wadouri`) — the classic URI Service: a single GET
  identified entirely by query parameters, returning a DICOM file or a
  rendered image.

DICOM processing (parsing, dicom+json, pixel codecs) is delegated to
[cocosip/go-dicom](https://github.com/cocosip/go-dicom) and
[cocosip/go-dicom-codecs](https://github.com/cocosip/go-dicom-codecs) —
never reimplemented here.

## The URL contract

The standard only fixes the *suffix* of the URL (`/studies/{study}/series/...`
and the `requestType=WADO` query string). Deployment prefixes differ per
hospital/gateway (`/api/wado/{hospitalCode}/{businessCode}`, ...) and are
**opaque** to this library: you configure the full prefix as the base URL and
the library appends only the standard part.

```
configured base (opaque)              standard part appended by the library
https://gw.example.com/api/wado/H1/RS /studies/1.2.840.../series/1.2.../instances/...
https://gw.example.com/api/wado/H1/RIS/wado-uri ?requestType=WADO&studyUID=...
```

## Install

```sh
go get github.com/cocosip/go-wado-client
```

Requires Go 1.26+.

## WADO-RS

```go
c, err := wadors.New("https://gw.example.com/api/wado/H0001/RIS",
    wado.WithBasicAuth("user", "pass"),
    wado.WithTLSClientConfig(privateCA),   // hospital self-signed CA
)
if err != nil { log.Fatal(err) }

// Stream a whole Study to disk; memory usage is independent of the study size.
mp, err := c.RetrieveStudy(ctx, "1.2.840.113619.2.1.1.1",
    wadors.WithTransferSyntax("1.2.840.10008.1.2.1"))
if err != nil {
    var se *wado.StatusError
    if errors.As(err, &se) && se.IsNotFound() { /* ... */ }
    log.Fatal(err)
}
defer mp.Close()
files, err := mp.WriteToDir(`D:\cache\H0001\study1`) // named by SOP Instance UID

// Or iterate parts lazily (Go 1.23+ range-over-func).
for p, err := range mp.Parts() {
    if err != nil { log.Fatal(err) }
    n, _ := io.Copy(w, p)
}

// Metadata: application/dicom+json parsed by go-dicom's serialization.
dss, err := c.StudyMetadata(ctx, "1.2.840.113619.2.1.1.1")
sop, _ := dss[0].GetString(tag.SOPInstanceUID)

// Frames, rendered images and Bulk Data.
frames, _ := c.RetrieveFrames(ctx, study, series, sop, []int{1, 3, 5})
img, _ := c.RetrieveRenderedInstance(ctx, study, series, sop,
    wadors.WithRenderedFormat("image/png"), wadors.WithWindow(40, 400))
bulk, _ := c.FetchBulkData(ctx, uriFromMetadata)
```

Note: frame lists travel in the URL — for very large lists (thousands of
frames) batch the calls, or gateways will answer 414 URL Too Long.

## WADO-URI

```go
u, err := wadouri.New("https://gw.example.com/api/wado/H0001/RIS/wado-uri")
if err != nil { log.Fatal(err) }

resp, err := u.Retrieve(ctx, wadouri.Request{
    StudyUID:  "1.2.840.113619.2.1.1.1",
    SeriesUID: "1.2.840.113619.2.1.1.2",
    ObjectUID: "1.2.840.113619.2.1.1.3",
    // ContentType omitted = application/dicom; use "image/jpeg" etc. for
    // the rendered transaction (rows/columns, region, windowCenter/Width...).
})
if err != nil { log.Fatal(err) }
defer resp.Close()
if resp.IsDICOM() { io.Copy(f, resp.Body) }
```

Local validation rejects up front everything the standard says a server must
answer with 400: the window pair / presentation pair and their mutual
exclusion, the rendered-only parameters (frameNumber, imageQuality,
rows/columns, region, annotation) combined with `application/dicom`, the
DICOM-only `anonymize` switch combined with a rendered content type, invalid
media types, and UID whitelist violations.

## Multiple hospitals / tenants

`Client` instances are cheap configuration holders sharing one connection
pool. `Fork` derives a new client with a different base URL, and the generic
`multi` registry maps a business key (hospital code, tenant ID, composite
struct — the semantics are yours) to cached clients:

```go
type routeKey struct{ Hospital, Biz string }

// One standard base address (no service routing) + one independent route
// template per service; placeholders are filled from the business key.
reg, err := multi.NewRegistry(
    multi.Template(
        "https://gw.example.com",
        multi.Routes{
            RS:  "/api/wado/{hospital}/{biz}/wado-rs",
            URI: "/api/wado/{hospital}/{biz}/wado-uri",
        },
        func(k routeKey) map[string]string {
            return map[string]string{"hospital": k.Hospital, "biz": k.Biz}
        },
    ),
    wado.WithTLSClientConfig(privateCA),
)
if err != nil { log.Fatal(err) }

g, err := reg.Client(ctx, routeKey{Hospital: "H0001", Biz: "RIS"})
// g.RS  -> https://gw.example.com/api/wado/H0001/RIS/wado-rs/studies/{study}/...
// g.URI -> https://gw.example.com/api/wado/H0001/RIS/wado-uri?requestType=WADO&...
```

## Parsing retrieved DICOM files

The optional `dicomx` package bridges WADO responses to go-dicom and
registers every codec of go-dicom-codecs (RLE, JPEG, JPEG-LS, JPEG 2000,
HTJ2K). Import it only when you need pixel decoding:

```go
import "github.com/cocosip/go-wado-client/dicomx"

for res, err := range dicomx.Datasets(mp) {
    if err != nil { log.Fatal(err) }
    pn, _ := res.Dataset.GetString(tag.PatientName)
}
```

## Configuration reference

Shared options (`wado.Option`, apply to both clients and the registry):
`WithBasicAuth`, `WithBearerTokenSource`, `WithRequestEditor`,
`WithHTTPClient`, `WithTLSClientConfig`, `WithRetry`, `WithUserAgent`,
`WithLogger(*slog.Logger)`, `WithLogHandler(slog.Handler)`,
`WithLenientUID`, `WithModernParamNames`.

Notes:

- **Logging** uses `log/slog` and is strictly opt-in: nothing is logged
  unless you inject a logger; the library never falls back to
  `slog.Default()`.
- **Parameter naming** defaults to the classic names spoken by deployed
  PACS today (`annotations`, `windowcenter`/`windowwidth`, `anonymity`);
  `WithModernParamNames()` switches to the current standard names
  (`annotation`, `window=center,width,function`, `anonymize` —
  `WithWindowFunction` picks the VOI LUT function, `linear` by default).
  `WithRawQuery` is the escape hatch for private gateways.
- **Transfer syntax** negotiation accepts a UID or the PS3.18 wildcard `*`
  ("any transfer syntax the server supports"); it applies to the multipart
  retrieves (instances/frames) only — metadata is always dicom+json.
- **Timeouts** are fine-grained (response header, TLS handshake); there is
  deliberately no overall client timeout — control large Study downloads
  with `context`.

## Runnable examples

Complete, runnable walkthroughs live under [`examples/`](examples/) — they
cover every implemented method of both clients. Point them at your gateway
(only the retrieval calls talk on the network):

```sh
# WADO-RS: all 13 client methods — study/series/instance retrieval,
# metadata, frames, rendered images, bulk data, options, error handling
go run ./examples/wadors -base https://gw.example.com/api/wado/H0001/wado-rs -study <studyUID>

# WADO-URI: both transactions with the full Request parameter set
go run ./examples/wadouri -endpoint https://gw.example.com/api/wado/H0001/wado-uri \
    -study <studyUID> -series <seriesUID> -object <sopUID>

# multi registry wired to the hospital route template
#   api/wado/{hospitalCode}/wado-rs + api/wado/{hospitalCode}/wado-uri
go run ./examples/multi -gateway https://gw.example.com -hospital H0001 -study <studyUID>
```

Retrieved files land in the directory given by `-out`.

## License

MS-PL (to match the go-dicom family).
