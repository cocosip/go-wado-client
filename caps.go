package wado

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// WADL media types of the Capabilities Description payload (PS3.18 §8.9.4):
// the WADL XML representation and its lossless JSON representation
// (PS3.18 Annex G, BadgerFish convention).
const (
	MediaTypeWADLXML  = "application/vnd.sun.wadl+xml"
	MediaTypeWADLJSON = "application/json"
)

// Capabilities is the reply of an OPTIONS-based capabilities discovery
// (PS3.18 §8.9 Retrieve Capabilities transaction; RFC 9110 §9.3.7 for the
// Allow header). The Allow header lists the methods the target resource
// supports — the discovery mechanism every HTTP origin server answers. The
// payload, when present, is the Capabilities Description: a WADL document
// describing the supported resources, methods and media types (parsed into
// WADL, or carried verbatim in Raw when the reply is not a parsable WADL).
type Capabilities struct {
	StatusCode int
	Header     http.Header
	// Allow holds the parsed Allow header values, upper-cased, de-duplicated
	// and in reply order; nil when the server sent none.
	Allow []string
	// WADL is the parsed Capabilities Description; nil when the reply
	// carries no WADL payload (Allow-only replies, unknown media types).
	WADL *WADL
	// Raw carries the verbatim payload bytes for representations this client
	// does not model.
	Raw []byte
}

// Supports reports whether the Allow header lists the method
// (case-insensitive).
func (c *Capabilities) Supports(method string) bool {
	m := strings.ToUpper(method)
	for _, a := range c.Allow {
		if a == m {
			return true
		}
	}
	return false
}

// WADLSupports reports whether the parsed Capabilities Description declares
// method on the resource at resourcePath (exact match against the flattened
// resource paths, e.g. "/studies"). It requires a WADL payload.
func (c *Capabilities) WADLSupports(resourcePath, method string) bool {
	if c.WADL == nil {
		return false
	}
	m := strings.ToUpper(method)
	for _, r := range c.WADL.Resources {
		if r.Path != resourcePath {
			continue
		}
		for _, mth := range r.Methods {
			if strings.ToUpper(mth.Name) == m {
				return true
			}
		}
	}
	return false
}

// WADL is a parsed Capabilities Description: the service's resource tree
// with the methods and response media types each resource supports.
type WADL struct {
	// Base is the base attribute of the WADL resources element (the
	// service URL the paths are relative to).
	Base string
	// Resources lists the declared resources in document order; child
	// resources are flattened and their paths joined (e.g. "/studies" →
	// "/studies/{StudyInstanceUID}").
	Resources []WADLResource
}

// WADLResource is one resource entry of a Capabilities Description.
type WADLResource struct {
	// Path is the (flattened) resource path relative to WADL.Base, as
	// declared — including URI templates in braces, e.g.
	// "/studies/{StudyInstanceUID}".
	Path    string
	Methods []WADLMethod
}

// WADLMethod is one method declaration of a resource.
type WADLMethod struct {
	// Name is the HTTP method (GET, POST, ...).
	Name string
	// MediaTypes lists the response representation media types.
	MediaTypes []string
}

// CapabilitiesOption customizes a capabilities request.
type CapabilitiesOption func(*capsCfg)

type capsCfg struct {
	resources []string
	accept    string
}

// WithCapsResource targets a sub-resource of the service instead of the
// base URL (paths join onto the base, e.g. WithCapsResource("studies") →
// OPTIONS {base}/studies).
func WithCapsResource(paths ...string) CapabilitiesOption {
	return func(c *capsCfg) { c.resources = append(c.resources, paths...) }
}

// WithCapsAccept overrides the Accept header entirely (advanced escape
// hatch). The default requests both standard representations:
// application/vnd.sun.wadl+xml and application/json (PS3.18 §8.9.4).
func WithCapsAccept(h string) CapabilitiesOption {
	return func(c *capsCfg) { c.accept = h }
}

// Capabilities performs the OPTIONS-based capabilities discovery on the
// service (PS3.18 §8.9): OPTIONS on the base URL, or on a sub-resource when
// WithCapsResource is given. Servers that do not implement the transaction
// answer 405/501 and yield a *StatusError; a server may also answer 204
// (no payload — the Allow header then carries the discovery result).
func (s *Service) Capabilities(ctx context.Context, opts ...CapabilitiesOption) (*Capabilities, error) {
	cfg := capsCfg{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	u := s.ResourceURL(cfg.resources...)
	resp, err := s.Do(ctx, http.MethodOptions, u, func(req *http.Request) {
		accept := cfg.accept
		if accept == "" {
			accept = MediaTypeWADLXML + ", " + MediaTypeWADLJSON
		}
		req.Header.Set("Accept", accept)
	})
	if err != nil {
		return nil, err
	}
	return newCapabilities(resp)
}

// newCapabilities consumes the reply and parses Allow plus the WADL
// payload.
func newCapabilities(resp *http.Response) (*Capabilities, error) {
	defer func() { _ = resp.Body.Close() }()
	c := &Capabilities{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Allow:      parseAllow(resp.Header),
	}
	if resp.StatusCode == http.StatusNoContent {
		return c, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wado: read capabilities payload: %w", err)
	}
	c.Raw = body
	wadl, err := parseWADL(body, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("wado: parse capabilities description: %w", err)
	}
	c.WADL = wadl
	return c, nil
}

// parseAllow parses the Allow header values (comma-joined in one value
// and/or repeated header fields), upper-cased and de-duplicated.
func parseAllow(h http.Header) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range h.Values("Allow") {
		for _, m := range strings.Split(v, ",") {
			m = strings.ToUpper(strings.TrimSpace(m))
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// parseWADL decodes a Capabilities Description by content type: WADL XML,
// its JSON representation, or a sniffed fallback. A declared XML/JSON payload
// that is not a WADL document — unparsable bytes, or valid syntax without the
// application/resources element (a gateway error page served under a declared
// type, for instance) — is an error, so a caller can never mistake it for an
// empty WADL; an unrelated content type yields (nil, nil) with the bytes kept
// in Capabilities.Raw.
func parseWADL(body []byte, ct string) (*WADL, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = ct
	}
	mt = strings.TrimSpace(strings.ToLower(mt))
	switch {
	case strings.HasSuffix(mt, "+xml"), mt == "text/xml", mt == "application/xml", mt == MediaTypeWADLXML:
		return parseWADLXML(body)
	case strings.HasSuffix(mt, "+json"), mt == MediaTypeWADLJSON:
		return parseWADLJSON(body)
	default:
		// Unknown media type: no guesswork, the raw bytes stay in
		// Capabilities.Raw.
		return nil, nil
	}
}

// --- WADL XML (application/vnd.sun.wadl+xml) ---

type wadlXMLApplication struct {
	XMLName   xml.Name          `xml:"application"`
	Resources *wadlXMLResources `xml:"resources"`
}

type wadlXMLResources struct {
	Base     string            `xml:"base,attr"`
	Resource []wadlXMLResource `xml:"resource"`
}

type wadlXMLResource struct {
	Path     string            `xml:"path,attr"`
	Method   []wadlXMLMethod   `xml:"method"`
	Resource []wadlXMLResource `xml:"resource"`
}

type wadlXMLMethod struct {
	Name     string `xml:"name,attr"`
	Response struct {
		Representation []struct {
			MediaType string `xml:"mediaType,attr"`
		} `xml:"representation"`
	} `xml:"response"`
}

func parseWADLXML(body []byte) (*WADL, error) {
	var doc wadlXMLApplication
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	// A document without a resources element is not a Capabilities
	// Description: it must not masquerade as an empty WADL (zero resources
	// is what a server with no endpoints would legitimately declare via
	// <resources/>).
	if doc.Resources == nil {
		return nil, errors.New("not a WADL document (no application/resources element)")
	}
	return &WADL{
		Base:      doc.Resources.Base,
		Resources: flattenXMLResources("", doc.Resources.Resource),
	}, nil
}

func flattenXMLResources(parent string, rs []wadlXMLResource) []WADLResource {
	var out []WADLResource
	for _, r := range rs {
		p := joinWADLPath(parent, r.Path)
		out = append(out, WADLResource{Path: p, Methods: xmlMethods(r.Method)})
		out = append(out, flattenXMLResources(p, r.Resource)...)
	}
	return out
}

func xmlMethods(ms []wadlXMLMethod) []WADLMethod {
	out := make([]WADLMethod, 0, len(ms))
	for _, m := range ms {
		wm := WADLMethod{Name: m.Name}
		for _, rep := range m.Response.Representation {
			wm.MediaTypes = append(wm.MediaTypes, rep.MediaType)
		}
		out = append(out, wm)
	}
	return out
}

// --- WADL JSON (the PS3.18 Annex G representation, BadgerFish convention:
// attributes carry a "@" prefix, repeated elements become arrays, unique
// elements objects, text content a "value" field) ---

type wadlJSONApplication struct {
	Application struct {
		Resources *wadlJSONResources `json:"resources"`
	} `json:"application"`
}

type wadlJSONResources struct {
	Base     string             `json:"@base"`
	Resource []wadlJSONResource `json:"resource"`
}

type wadlJSONResource struct {
	Path     string             `json:"@path"`
	Method   []wadlJSONMethod   `json:"method"`
	Resource []wadlJSONResource `json:"resource"`
}

type wadlJSONMethod struct {
	Name     string `json:"@name"`
	Response struct {
		Representation []struct {
			MediaType string `json:"@mediaType"`
		} `json:"representation"`
	} `json:"response"`
}

func parseWADLJSON(body []byte) (*WADL, error) {
	var doc wadlJSONApplication
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	// Any valid JSON object unmarshals into the shape above (absent fields
	// stay zero), so the application/resources element is what tells a
	// Capabilities Description apart from an unrelated JSON document (a 200
	// gateway error page served as application/json, for instance): without
	// it the payload is an error, not an empty WADL.
	if doc.Application.Resources == nil {
		return nil, errors.New("not a WADL document (no application/resources element)")
	}
	res := doc.Application.Resources
	return &WADL{
		Base:      res.Base,
		Resources: flattenJSONResources("", res.Resource),
	}, nil
}

func flattenJSONResources(parent string, rs []wadlJSONResource) []WADLResource {
	var out []WADLResource
	for _, r := range rs {
		p := joinWADLPath(parent, r.Path)
		methods := make([]WADLMethod, 0, len(r.Method))
		for _, m := range r.Method {
			wm := WADLMethod{Name: m.Name}
			for _, rep := range m.Response.Representation {
				wm.MediaTypes = append(wm.MediaTypes, rep.MediaType)
			}
			methods = append(methods, wm)
		}
		out = append(out, WADLResource{Path: p, Methods: methods})
		out = append(out, flattenJSONResources(p, r.Resource)...)
	}
	return out
}

// joinWADLPath joins a child resource path onto its parent's; a child path
// may be absolute (replacing the parent path).
func joinWADLPath(parent, child string) string {
	if strings.HasPrefix(child, "/") {
		return child
	}
	if parent == "" {
		return "/" + strings.TrimPrefix(child, "/")
	}
	return path.Join(parent, child)
}

// IsCapabilitiesUnsupported reports whether err is a 405 (Method Not
// Allowed) or 501 (Not Implemented) StatusError — the replies of origin
// servers that do not implement OPTIONS-based discovery.
func IsCapabilitiesUnsupported(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode == http.StatusMethodNotAllowed ||
		se.StatusCode == http.StatusNotImplemented
}
