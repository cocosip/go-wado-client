// Command qido-example demonstrates the QIDO-RS client (the Search
// transaction, PS3.18 §10.6) and the OPTIONS-based capabilities discovery
// (PS3.18 §8.9): attribute search at the study/series/instance levels,
// client-driven limit/offset paging with the Warning 299 additional-results
// signal, streaming results, and OPTIONS discovery.
//
// Run it against a real gateway (the client only talks when a call is made):
//
//	go run ./examples/qido -base https://gw.example.com/api/wado/H0001/wado-rs \
//		-patient 11235813 -limit 25
//
// -base is everything before the standard resource path (studies/...);
// QIDO-RS is usually served under the same route prefix as WADO-RS.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"

	"github.com/cocosip/go-dicom/pkg/dicom/tag"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/qido"
)

func main() {
	var (
		base     = flag.String("base", "https://gw.example.com/api/wado/H0001/wado-rs", "QIDO-RS base URL: everything before /studies/...")
		patient  = flag.String("patient", "", "PatientID match (exact)")
		dateFrom = flag.String("from", "", "StudyDate range start (yyyymmdd)")
		dateTo   = flag.String("to", "", "StudyDate range end (yyyymmdd)")
		limit    = flag.Uint64("limit", 25, "page size (client-driven paging via limit/offset)")
		insecure = flag.Bool("insecure", false, "skip TLS verification (hospital self-signed certificates)")
	)
	flag.Parse()

	c, err := qido.New(*base, clientOptions(*insecure)...)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	// Capabilities discovery: OPTIONS on the service base URL. Every HTTP
	// origin server answers with the Allow header; PS3.18 §8.9 servers
	// additionally return a WADL Capabilities Description (§8.9.4 media
	// types: application/vnd.sun.wadl+xml and its JSON representation).
	if caps, err := c.Capabilities(ctx); err == nil {
		fmt.Printf("OPTIONS %s -> %d, Allow: %v\n", c.BaseURL(), caps.StatusCode, caps.Allow)
		if caps.WADL != nil {
			fmt.Printf("  capabilities description: %d resources, base %s\n",
				len(caps.WADL.Resources), caps.WADL.Base)
			// A concrete question the WADL answers: does the server offer
			// the search resource, and with which response media types?
			if caps.WADLSupports("/studies", "GET") {
				fmt.Println("  /studies GET: supported")
			}
		}
	} else if wado.IsCapabilitiesUnsupported(err) {
		fmt.Println("server does not implement OPTIONS discovery (allowed: legacy gateways)")
	} else {
		log.Printf("capabilities probe failed: %v", err)
	}

	q := qido.Query{
		Match: matchSet(*patient, *dateFrom, *dateTo),
		Limit: limit,
	}

	// Search for studies: one page per call; the standard's paging is
	// client-driven (limit/offset), and the Warning 299 header announces
	// how many additional results remain.
	offset := uint64(0)
	for page := 0; ; page++ {
		q.Offset = offset
		res, err := c.SearchStudies(ctx, q)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("page %d: %d studies\n", page, len(res.Datasets))
		for _, ds := range res.Datasets {
			uid, _ := ds.GetString(tag.StudyInstanceUID)
			pid, _ := ds.GetString(tag.PatientID)
			date, _ := ds.GetString(tag.StudyDate)
			fmt.Printf("  %s  %s  %s\n", uid, pid, date)
		}
		additional, more := res.AdditionalResults()
		if !more || len(res.Datasets) == 0 {
			break
		}
		fmt.Printf("  (%d additional results on the server)\n", additional)
		offset += uint64(len(res.Datasets))
	}

	// Large result sets: stream dataset by dataset instead of collecting.
	rs, err := c.SearchStudiesStream(ctx, qido.Query{Limit: limit})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	for {
		ds, err := rs.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		uid, _ := ds.GetString(tag.StudyInstanceUID)
		fmt.Printf("streamed %s\n", uid)
	}

	// Scoped searches descend the hierarchy; relational (all-level) searches
	// are optional server-side and may answer 4xx:
	//
	//	res, err := c.SearchSeries(ctx, studyUID, qido.Query{})
	//	res, err := c.SearchSeriesInstances(ctx, studyUID, seriesUID, qido.Query{})
	//	res, err := c.SearchAllStudies... (SearchStudies IS the all-level search)
	_ = c
}

// matchSet assembles the match keys: exact PatientID and an (optionally
// open-ended) StudyDate range — C-FIND range semantics per PS3.18 §8.3.4.
func matchSet(patient, from, to string) []qido.Match {
	var ms []qido.Match
	if patient != "" {
		ms = append(ms, qido.Match{Attribute: "PatientID", Value: patient})
	}
	if from != "" || to != "" {
		ms = append(ms, qido.Match{Attribute: "StudyDate", Value: from + "-" + to})
	}
	return ms
}

func clientOptions(insecure bool) []wado.Option {
	var opts []wado.Option
	if insecure {
		opts = append(opts, wado.WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}))
	}
	return opts
}
