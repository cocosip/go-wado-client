package wado

import (
	"net/url"
	"testing"
)

func TestParseBaseURL(t *testing.T) {
	const host = "https://h.cn"
	cases := []struct{ in, want string }{
		{host, host},
		{host + "/", host},
		{"https://h.cn/api/wado/H1/RIS", "https://h.cn/api/wado/H1/RIS"},
		{"https://h.cn/api//wado/../wado", "https://h.cn/api/wado"},
		{"http://10.0.0.1:8443/dicomweb/", "http://10.0.0.1:8443/dicomweb"},
		{host + "/p?x=1#frag", host + "/p"},
	}
	for _, c := range cases {
		u, err := ParseBaseURL(c.in)
		if err != nil {
			t.Fatalf("ParseBaseURL(%q): %v", c.in, err)
		}
		if got := u.String(); got != c.want {
			t.Errorf("ParseBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bad := []string{"", "   ", "localhost/x", "ftp://h/x", "://x", "/api/wado"}
	for _, in := range bad {
		if _, err := ParseBaseURL(in); err == nil {
			t.Errorf("ParseBaseURL(%q) expected error", in)
		}
	}
}

func TestResolveReference(t *testing.T) {
	base, err := url.Parse("https://h.cn/api/wado/H1/studies/1.2/series/2.3/instances/4.5/metadata")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ ref, want string }{
		// 1. absolute URI
		{"https://other.cn/dicomweb/studies/x/bulkdata/1", "https://other.cn/dicomweb/studies/x/bulkdata/1"},
		// 2. absolute-path reference (scheme+host of base)
		{"/dicomweb/studies/x/bulkdata/1", "https://h.cn/dicomweb/studies/x/bulkdata/1"},
		// 3. relative-path reference (RFC 3986 against the base path)
		{"./bulkdata/00282000", "https://h.cn/api/wado/H1/studies/1.2/series/2.3/instances/4.5/bulkdata/00282000"},
	}
	for _, c := range cases {
		u, err := ResolveReference(base, c.ref)
		if err != nil {
			t.Fatalf("ResolveReference(%q): %v", c.ref, err)
		}
		if got := u.String(); got != c.want {
			t.Errorf("ResolveReference(%q) = %q, want %q", c.ref, got, c.want)
		}
	}

	// Unsupported scheme form is rejected per the standard.
	if _, err := ResolveReference(base, "dicomweb://x/y"); err == nil {
		t.Error("ResolveReference with dicomweb:// scheme expected error")
	}
}
