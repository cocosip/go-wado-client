package wado

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capabilitiesRequest records what the fake server received.
type capabilitiesRequest struct {
	method string
	path   string
	accept string
}

func newTestService(t *testing.T, base string) *Service {
	t.Helper()
	core, err := NewCore()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(core, base)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newCapabilitiesServer(t *testing.T, status int, ct string, body string, allow string) (*httptest.Server, *capabilitiesRequest) {
	t.Helper()
	got := &capabilitiesRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.accept = r.Header.Get("Accept")
		if allow != "" {
			w.Header().Set("Allow", allow)
		}
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

const sampleWADLXML = `<?xml version="1.0" encoding="UTF-8"?>
<application xmlns="http://wadl.dev.java.net/2009/02">
  <resources base="https://srv.example.com/dicomweb">
    <resource path="/studies">
      <method name="GET">
        <response>
          <representation mediaType="application/dicom+json"/>
          <representation mediaType="multipart/related;type=&quot;application/dicom&quot;"/>
        </response>
      </method>
      <resource path="{StudyInstanceUID}">
        <method name="GET">
          <response>
            <representation mediaType="multipart/related;type=&quot;application/dicom&quot;"/>
          </response>
        </method>
        <resource path="series">
          <method name="GET"/>
        </resource>
      </resource>
    </resource>
    <resource path="/aetitles">
      <method name="GET"/>
    </resource>
  </resources>
</application>`

const sampleWADLJSON = `{
  "application": {
    "resources": {
      "@base": "https://srv.example.com/dicomweb",
      "resource": [
        {
          "@path": "/studies",
          "method": [
            {
              "@name": "GET",
              "response": {
                "representation": [
                  {"@mediaType": "application/dicom+json"},
                  {"@mediaType": "multipart/related;type=\"application/dicom\""}
                ]
              }
            },
            {"@name": "POST"}
          ],
          "resource": [
            {"@path": "{StudyInstanceUID}", "method": [{"@name": "GET"}]}
          ]
        }
      ]
    }
  }
}`

func TestCapabilitiesAllowHeader(t *testing.T) {
	srv, got := newCapabilitiesServer(t, http.StatusOK, "", "", "get, post , GET")
	s := newTestService(t, srv.URL)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodOptions {
		t.Errorf("method = %s, want OPTIONS", got.method)
	}
	if got.path != "" && got.path != "/" {
		t.Errorf("path = %q, want the base URL path", got.path)
	}
	wantAccept := MediaTypeWADLXML + ", " + MediaTypeWADLJSON
	if got.accept != wantAccept {
		t.Errorf("accept = %q, want %q", got.accept, wantAccept)
	}
	if len(caps.Allow) != 2 || caps.Allow[0] != methodGet || caps.Allow[1] != "POST" {
		t.Errorf("Allow = %v, want [GET POST]", caps.Allow)
	}
	if !caps.Supports("get") || caps.Supports("DELETE") {
		t.Error("Supports is case-insensitive and negative for absent methods")
	}
	if caps.WADL != nil {
		t.Errorf("WADL = %+v, want nil without a payload", caps.WADL)
	}
}

func TestCapabilitiesWADLXML(t *testing.T) {
	srv, _ := newCapabilitiesServer(t, http.StatusOK, MediaTypeWADLXML, sampleWADLXML, "OPTIONS, GET")
	s := newTestService(t, srv.URL+"/dicomweb")
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.WADL == nil {
		t.Fatal("WADL not parsed from XML payload")
	}
	if caps.WADL.Base != "https://srv.example.com/dicomweb" {
		t.Errorf("Base = %q", caps.WADL.Base)
	}
	var studies *WADLResource
	for i := range caps.WADL.Resources {
		if caps.WADL.Resources[i].Path == "/studies" {
			studies = &caps.WADL.Resources[i]
		}
	}
	if studies == nil {
		t.Fatalf("resources = %+v, want /studies", caps.WADL.Resources)
	}
	if len(studies.Methods) != 1 || studies.Methods[0].Name != methodGet {
		t.Errorf("studies methods = %+v", studies.Methods)
	}
	want := []string{"application/dicom+json", `multipart/related;type="application/dicom"`}
	if len(studies.Methods[0].MediaTypes) != 2 || studies.Methods[0].MediaTypes[0] != want[0] || studies.Methods[0].MediaTypes[1] != want[1] {
		t.Errorf("media types = %v, want %v", studies.Methods[0].MediaTypes, want)
	}
	// Flattened child resources join their parent paths.
	for _, wantPath := range []string{"/studies/{StudyInstanceUID}", "/studies/{StudyInstanceUID}/series", "/aetitles"} {
		if !caps.WADLSupports(wantPath, methodGet) {
			t.Errorf("WADLSupports(%q, GET) = false; resources = %+v", wantPath, caps.WADL.Resources)
		}
	}
	if caps.WADLSupports("/aetitles", "POST") {
		t.Error("WADLSupports reported an absent method")
	}
	if len(caps.Raw) == 0 {
		t.Error("Raw payload not retained")
	}
}

func TestCapabilitiesWADLJSON(t *testing.T) {
	srv, _ := newCapabilitiesServer(t, http.StatusOK, MediaTypeWADLJSON, sampleWADLJSON, "")
	s := newTestService(t, srv.URL+"/dicomweb")
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.WADL == nil {
		t.Fatal("WADL not parsed from JSON payload")
	}
	if caps.WADL.Base != "https://srv.example.com/dicomweb" {
		t.Errorf("Base = %q", caps.WADL.Base)
	}
	var gotPaths []string
	for _, r := range caps.WADL.Resources {
		gotPaths = append(gotPaths, r.Path)
	}
	// The JSON sample declares the /studies tree (the /aetitles entry exists
	// only in the XML sample).
	want := "/studies,/studies/{StudyInstanceUID}"
	if joinPaths(gotPaths) != want {
		t.Errorf("resources = %v, want %s", gotPaths, want)
	}
	if !caps.WADLSupports("/studies", "POST") {
		t.Error("WADLSupports(/studies, POST) = false")
	}
}

func joinPaths(ps []string) string {
	out := ""
	for i, p := range ps {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func TestCapabilitiesUnknownPayload(t *testing.T) {
	srv, _ := newCapabilitiesServer(t, http.StatusOK, "text/html", "<html>gateway page</html>", methodGet)
	s := newTestService(t, srv.URL)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.WADL != nil || string(caps.Raw) != "<html>gateway page</html>" {
		t.Errorf("WADL = %+v, Raw = %q; want nil WADL with verbatim Raw", caps.WADL, caps.Raw)
	}
}

func TestCapabilities204(t *testing.T) {
	srv, _ := newCapabilitiesServer(t, http.StatusNoContent, "", "", "GET, OPTIONS")
	s := newTestService(t, srv.URL)
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.StatusCode != http.StatusNoContent || len(caps.Allow) != 2 {
		t.Errorf("status = %d, allow = %v", caps.StatusCode, caps.Allow)
	}
}

func TestCapabilitiesSubResource(t *testing.T) {
	srv, got := newCapabilitiesServer(t, http.StatusOK, "", "", methodGet)
	s := newTestService(t, srv.URL+"/dicomweb")
	if _, err := s.Capabilities(context.Background(), WithCapsResource("studies")); err != nil {
		t.Fatal(err)
	}
	if got.path != "/dicomweb/studies" {
		t.Errorf("path = %q, want /dicomweb/studies", got.path)
	}
}

func TestCapabilitiesAcceptOverride(t *testing.T) {
	srv, got := newCapabilitiesServer(t, http.StatusOK, "", "", methodGet)
	s := newTestService(t, srv.URL)
	if _, err := s.Capabilities(context.Background(), WithCapsAccept("application/json")); err != nil {
		t.Fatal(err)
	}
	if got.accept != "application/json" {
		t.Errorf("accept = %q", got.accept)
	}
}

func TestCapabilitiesUnsupported(t *testing.T) {
	srv, _ := newCapabilitiesServer(t, http.StatusNotImplemented, "text/plain", "no OPTIONS here", "")
	s := newTestService(t, srv.URL)
	_, err := s.Capabilities(context.Background())
	if !IsCapabilitiesUnsupported(err) {
		t.Fatalf("err = %v, want IsCapabilitiesUnsupported", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotImplemented {
		t.Errorf("err = %v, want a 501 *StatusError", err)
	}
}

const methodGet = "GET"

func TestParseAllowForms(t *testing.T) {
	h := http.Header{}
	h.Set("Allow", "GET, POST")
	h.Add("Allow", "options")
	got := parseAllow(h)
	if len(got) != 3 || got[0] != methodGet || got[1] != "POST" || got[2] != "OPTIONS" {
		t.Errorf("parseAllow = %v, want [GET POST OPTIONS]", got)
	}
}
