package wado

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// ParseBaseURL parses and normalizes a base URL: it must be an absolute
// http(s) URL; query and fragment are cleared and the path is cleaned with a
// trailing slash removed. An encoded slash (%2F) in the path is rejected —
// the path is cleaned decoded, so one would silently address a different
// prefix than the one configured.
//
// No assumption is made about the internal structure of the prefix (which
// segment carries the hospital code, what that segment is called). The only
// contract: the base URL ends right where the standard resource path
// (studies/...) begins.
func ParseBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("wado: empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("wado: parse URL %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("wado: URL must be absolute with http(s) scheme: %q", raw)
	}
	u.Fragment = ""
	u.RawFragment = ""
	u.RawQuery = ""
	if u.Path != "" {
		// A percent-encoded slash (%2F) in the prefix would be decoded into a
		// real path separator below (the path is cleaned decoded and RawPath
		// dropped), silently addressing a different prefix than the configured
		// one — multi.joinRoute rejects percent escapes in routes for the same
		// reason. Reject it up front instead.
		if strings.Contains(strings.ToLower(u.RawPath), "%2f") {
			return nil, fmt.Errorf("wado: URL path must not contain an encoded slash (%%2F); pass the path unescaped: %q", raw)
		}
		u.Path = path.Clean(u.Path)
		if u.Path == "/" {
			u.Path = ""
		}
		u.RawPath = ""
	}
	return u, nil
}

// ResolveReference resolves the three standard BulkDataURI forms (PS3.18):
//
//  1. an absolute URI (only http/https are supported);
//  2. an absolute-path reference ("/dicomweb/...", takes scheme+host from base);
//  3. a relative-path reference ("./bulkdata/...", resolved against the base
//     path per RFC 3986).
//
// Relative URIs carrying any other scheme are explicitly unsupported by the
// standard and return an error.
func ResolveReference(base *url.URL, ref string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, fmt.Errorf("wado: parse reference %q: %w", ref, err)
	}
	if u.IsAbs() {
		if u.Scheme == "http" || u.Scheme == "https" {
			return u, nil
		}
		return nil, fmt.Errorf("wado: unsupported reference scheme %q: %q", u.Scheme, ref)
	}
	return base.ResolveReference(u), nil
}
