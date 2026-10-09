# go-wado-client

[![Go Reference](https://pkg.go.dev/badge/github.com/cocosip/go-wado-client.svg)](https://pkg.go.dev/github.com/cocosip/go-wado-client)

A DICOMweb client library for Go, implementing the DICOM PS3.18 (Web
Services) transactions:

- **WADO-RS** (`wadors`) — the Retrieve transaction of the Studies Service:
  Study/Series/Instance retrieval (streaming `multipart/related`), metadata
  (`application/dicom+json`), frame pixel data, rendered images and Bulk
  Data.
- **QIDO-RS** (`qido`) — the Search transaction of the Studies Service:
  attribute-based search over studies/series/instances returning
  `application/dicom+json` datasets, with client-driven limit/offset paging
  and the Warning 299 additional-results signal.
- **WADO-URI** (`wadouri`) — the classic URI Service: a single GET
  identified entirely by query parameters, returning a DICOM file or a
  rendered image.
- **Capabilities discovery** (`wado.Service.Capabilities` and a
  `Capabilities` method on all three service clients) — the OPTIONS-based
  Retrieve Capabilities transaction (PS3.18 §8.9): the `Allow` header method
  list every HTTP origin server answers, plus the WADL Capabilities
  Description (§8.9.4: `application/vnd.sun.wadl+xml` and its JSON
  representation) parsed into a uniform model. The URI Service is not
  required to implement the transaction, so its client surfaces gateways
  without it as 404/405 errors.

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
rimgs, _ := c.RetrieveRenderedFrames(ctx, study, series, sop, []int{1, 2})
// rimgs is a cursor over every image — a conformant server may answer
// multipart/related with one image part per frame (PS3.18 §10.4.4).
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
exclusions, `frameNumber` combined with a presentation state, `rows` /
`columns` only as a pair, the rendered-only parameters (frameNumber,
imageQuality, rows/columns, region, annotation) combined with
`application/dicom`, the DICOM-only `anonymize` switch combined with a
rendered content type, invalid media types, and UID whitelist violations.

## QIDO-RS (search)

```go
c, err := qido.New("https://gw.example.com/api/wado/H0001/RIS")
if err != nil { log.Fatal(err) }

// One page per call; paging is client-driven per the standard (limit/offset).
limit := uint64(25)
res, err := c.SearchStudies(ctx, qido.Query{
    Match: []qido.Match{
        {Attribute: "PatientID", Value: "11235813"},
        {Attribute: "StudyDate", Value: "20240101-20241231"}, // C-FIND range
        {Attribute: "SeriesInstanceUID", Values: []string{"1.2.3", "1.2.4"}}, // UID list
    },
    IncludeFields: []string{"00081048", "00081060"},
    FuzzyMatching: qido.Bool(false),
    Limit:         &limit,
    // OrderBy:       []string{"-StudyDate"},  // ecosystem extension (dcm4chee)
    // AETitle:       []string{"AE1"},
})
if err != nil { log.Fatal(err) }
for _, ds := range res.Datasets {
    uid, _ := ds.GetString(tag.StudyInstanceUID)
}
additional, more := res.AdditionalResults() // Warning 299: N results remain
```

Match attribute IDs accept tags (`0020000D`), keywords (`StudyInstanceUID`)
and dotted sequence paths (`00101002.00100020`); match values follow the
C-FIND matching rules (wildcards `*`/`?`, open-ended date/time ranges).
`204 No Content` is a success: an empty, non-nil `Datasets` slice. Large
result sets stream dataset by dataset via the `*Stream` variants
(`SearchStudiesStream`, ...), and every standard resource has a method:
`SearchStudies`, `SearchSeries`, `SearchStudyInstances`,
`SearchSeriesInstances` and the relational `SearchAllSeries` /
`SearchAllInstances` (optional server-side).

## Capabilities discovery (HTTP OPTIONS)

```go
caps, err := c.Capabilities(ctx) // c = *wadors.Client, *qido.Client or *wadouri.Client
if err != nil {
    if wado.IsCapabilitiesUnsupported(err) { /* 405/501: legacy gateway */ }
    log.Fatal(err)
}
fmt.Println(caps.Allow)              // e.g. [GET HEAD OPTIONS] (RFC 9110)
fmt.Println(caps.WADLSupports("/studies", "GET")) // WADL payload, if returned
```

`OPTIONS` targets the service base URL (or a sub-resource via
`wado.WithCapsResource("studies")`). The reply always carries the parsed
`Allow` header; when the server implements the PS3.18 §8.9 transaction, the
WADL Capabilities Description is parsed from either standard representation
(`application/vnd.sun.wadl+xml` or `application/json`) into
`caps.WADL` — resource paths, methods and response media types. Payloads
under an unknown content type are passed through verbatim in `caps.Raw`; a
payload declared as XML or JSON that is not a WADL document (a gateway error
page, for instance) is an error.

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
            RS:   "/api/wado/{hospital}/{biz}/wado-rs",
            URI:  "/api/wado/{hospital}/{biz}/wado-uri",
            QIDO: "/api/wado/{hospital}/{biz}/wado-rs", // usually the RS route
        },
        func(k routeKey) map[string]string {
            return map[string]string{"hospital": k.Hospital, "biz": k.Biz}
        },
    ),
    wado.WithTLSClientConfig(privateCA),
)
if err != nil { log.Fatal(err) }

g, err := reg.Client(ctx, routeKey{Hospital: "H0001", Biz: "RIS"})
// g.RS   -> https://gw.example.com/api/wado/H0001/RIS/wado-rs/studies/{study}/...
// g.URI  -> https://gw.example.com/api/wado/H0001/RIS/wado-uri?requestType=WADO&...
// g.Qido -> https://gw.example.com/api/wado/H0001/RIS/wado-rs/studies?... (set QidoRoute; usually the RS route)
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

Shared options (`wado.Option`, apply to every client and the registry):
`WithBasicAuth`, `WithBearerTokenSource`, `WithRequestEditor`,
`WithHTTPClient`, `WithTLSClientConfig`, `WithRetry`, `WithUserAgent`,
`WithLogger(*slog.Logger)`, `WithLogHandler(slog.Handler)`,
`WithMaxIdleConnsPerHost`, `WithLenientUID`, `WithLegacyParamNames`.

Notes:

- **Logging** uses `log/slog` and is strictly opt-in: nothing is logged
  unless you inject a logger; the library never falls back to
  `slog.Default()`.
- **Parameter naming** follows the current PS3.18 names by default
  (`annotation`, `window=center,width,function`, `iccprofile`, `anonymize`;
  `WithWindowFunction` picks the VOI LUT function, `linear` by default).
  `WithLegacyParamNames()` switches to the retired WADO-WS-era dialect
  (`annotations`, `windowcenter`/`windowwidth`, `icccolorspace`, `anonymity`)
  for private gateways that only answer to it — those names never appeared
  in any published WADO-RS edition. `WithRawQuery` is the escape hatch for
  anything else.
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

# QIDO-RS: attribute search with paging + OPTIONS capabilities discovery
go run ./examples/qido -base https://gw.example.com/api/wado/H0001/wado-rs \
    -patient 11235813 -limit 25

# multi registry wired to the hospital route template
#   api/wado/{hospitalCode}/wado-rs + api/wado/{hospitalCode}/wado-uri
go run ./examples/multi -gateway https://gw.example.com -hospital H0001 -study <studyUID>
```

Retrieved files land in the directory given by `-out`.

## License

MS-PL (to match the go-dicom family).
