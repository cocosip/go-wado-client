package wadors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cocosip/go-wado-client"
	"github.com/cocosip/go-wado-client/internal/queryx"
)

// RenderedOption customizes rendered retrieval.
type RenderedOption func(*renderedCfg)

// Window LUT function keywords of the standard window parameter, which per
// PS3.18 §8.3.5 is window=center,width,function (all three values mandatory).
const (
	WindowFunctionLinear      = "linear"
	WindowFunctionLinearExact = "linear-exact"
	WindowFunctionSigmoid     = "sigmoid"
)

type renderedCfg struct {
	format      string
	accept      string
	quality     int
	viewport    [2]int
	window      [2]float64
	hasWindow   bool
	voiFunction string
	annotations []string
	icc         string
	raw         url.Values
}

// WithRenderedFormat sets the rendered media type (default image/jpeg;
// application/dicom is not allowed).
func WithRenderedFormat(m string) RenderedOption {
	return func(c *renderedCfg) { c.format = m }
}

// WithRenderedAccept overrides the Accept header entirely (advanced escape
// hatch) — e.g. `multipart/related; type="image/jpeg"` to negotiate the
// multipart reply form of a rendered response explicitly.
func WithRenderedAccept(h string) RenderedOption {
	return func(c *renderedCfg) { c.accept = h }
}

// WithViewport sets the viewport (columns, rows).
func WithViewport(cols, rows int) RenderedOption {
	return func(c *renderedCfg) { c.viewport = [2]int{cols, rows} }
}

// WithQuality sets the lossy compression quality (1..100).
func WithQuality(q int) RenderedOption {
	return func(c *renderedCfg) { c.quality = q }
}

// WithWindow sets window center/width (mutual exclusion with Presentation
// State is the caller's responsibility).
func WithWindow(center, width float64) RenderedOption {
	return func(c *renderedCfg) { c.window = [2]float64{center, width}; c.hasWindow = true }
}

// WithWindowFunction selects the VOI LUT function carried by the standard
// window parameter: WindowFunctionLinear (the default when unset),
// WindowFunctionLinearExact or WindowFunctionSigmoid. The legacy
// windowcenter/windowwidth pair has no function component, so the value is
// ignored in legacy mode (WithLegacyParamNames).
func WithWindowFunction(fn string) RenderedOption {
	return func(c *renderedCfg) { c.voiFunction = fn }
}

// WithAnnotation sets burnt-in annotations; kinds must be from
// {"patient", "technique"}.
func WithAnnotation(kinds ...string) RenderedOption {
	return func(c *renderedCfg) { c.annotations = append(c.annotations, kinds...) }
}

// WithICCProfile sets the ICC color profile.
func WithICCProfile(id string) RenderedOption {
	return func(c *renderedCfg) { c.icc = id }
}

// WithRawQuery appends/overrides query parameters (escape hatch for private
// gateways; same-name keys override the defaults).
func WithRawQuery(v url.Values) RenderedOption {
	return func(c *renderedCfg) {
		if c.raw == nil {
			c.raw = url.Values{}
		}
		for k, vs := range v {
			c.raw[k] = append([]string(nil), vs...)
		}
	}
}

func buildRenderedCfg(opts []RenderedOption) renderedCfg {
	var cfg renderedCfg
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// query builds the rendered query parameters; the PS3.18 names are the
// default, WithLegacyParamNames switches to the retired WADO-WS-era dialect.
func (cfg renderedCfg) query(legacy bool) url.Values {
	q := url.Values{}
	if legacy {
		// Not part of any published WADO-RS edition; some deployed gateways
		// only answer to these names.
		if len(cfg.annotations) > 0 {
			q.Set("annotations", strings.Join(cfg.annotations, ","))
		}
		if cfg.hasWindow {
			q.Set("windowcenter", formatFloat(cfg.window[0]))
			q.Set("windowwidth", formatFloat(cfg.window[1]))
		}
		if cfg.icc != "" {
			q.Set("icccolorspace", cfg.icc)
		}
	} else {
		if len(cfg.annotations) > 0 {
			q.Set("annotation", strings.Join(cfg.annotations, ","))
		}
		if cfg.hasWindow {
			// PS3.18 §8.3.5: window=center,width,function, all three values
			// mandatory.
			fn := cfg.voiFunction
			if fn == "" {
				fn = WindowFunctionLinear
			}
			q.Set("window", formatFloat(cfg.window[0])+","+formatFloat(cfg.window[1])+","+fn)
		}
		if cfg.icc != "" {
			q.Set("iccprofile", cfg.icc)
		}
	}
	if cfg.quality > 0 {
		q.Set("quality", strconv.Itoa(cfg.quality))
	}
	if cfg.viewport != [2]int{} {
		q.Set("viewport", fmt.Sprintf("%d,%d", cfg.viewport[0], cfg.viewport[1]))
	}
	for k, vs := range cfg.raw {
		q[k] = append([]string(nil), vs...)
	}
	return q
}

// formatFloat renders a float without exponent notation — some servers fail
// to parse values like "1e+07".
func formatFloat(f float64) string { return queryx.FormatFloat(f) }

// iccProfileValues are the keyword values of the iccprofile parameter
// (PS3.18 §8.3.5.1.5).
var iccProfileValues = map[string]bool{
	"no": true, "yes": true, "srgb": true,
	"adobergb": true, "rommrgb": true, "displayp3": true,
}

// Rendered is a rendered response (a single-part image stream such as
// image/png).
//
// It represents exactly one image: when the server answers a rendered
// request with multipart/related, only the first part is delivered. That is
// always correct for a single-frame target, but a multi-image response
// (PS3.18 §10.4.3.3.3: one rendering per valid instance — e.g. a
// Presentation State target referencing many frames, §8.3.5.1.6) needs
// RetrieveRenderedFrames-style iteration instead.
type Rendered struct {
	ContentType string
	Body        io.Reader
	Header      http.Header
	resp        *http.Response
}

// Close closes the underlying response.
func (r *Rendered) Close() error {
	if r.resp != nil {
		return r.resp.Body.Close()
	}
	if rc, ok := r.Body.(io.Closer); ok {
		return rc.Close()
	}
	return nil
}

// RetrieveRenderedInstance retrieves the rendered image of an instance.
func (c *Client) RetrieveRenderedInstance(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RenderedOption) (*Rendered, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	resp, err := c.prepareRendered(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "rendered"), opts)
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	// Tolerance: some servers wrap rendered images in multipart; take the
	// first part (see the Rendered doc for the multi-image caveat).
	if strings.HasPrefix(ct, "multipart/") {
		mp, err := newMultipart(resp)
		if err != nil {
			return nil, err
		}
		p, err := mp.Next()
		if err != nil {
			_ = mp.Close()
			return nil, fmt.Errorf("wadors: rendered response: %w", err)
		}
		if p.ct != "" {
			ct = p.ct
		}
		return &Rendered{ContentType: ct, Body: p, Header: resp.Header.Clone(), resp: resp}, nil
	}
	return &Rendered{ContentType: ct, Body: resp.Body, Header: resp.Header.Clone(), resp: resp}, nil
}

// RetrieveRenderedFrames retrieves rendered images of the given frames
// (1-based, sorted and de-duplicated automatically).
//
// The returned cursor covers both reply forms a conformant origin server may
// choose (PS3.18 §10.4.4): a single-part image, or multipart/related with
// one image part per frame (the payload "shall contain a rendering of all
// valid Instances", §10.4.3.3.3) — every image is delivered, none silently
// dropped. Use WithRenderedAccept to negotiate the multipart form
// explicitly.
func (c *Client) RetrieveRenderedFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RenderedOption) (*Multipart, error) {
	fl, err := c.buildFrameList(frames)
	if err != nil {
		return nil, err
	}
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	resp, err := c.prepareRendered(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "frames", fl, "rendered"), opts)
	if err != nil {
		return nil, err
	}
	return newMultipart(resp)
}

// prepareRendered validates the rendered options, builds the query string
// and performs the GET.
func (c *Client) prepareRendered(ctx context.Context, u *url.URL, opts []RenderedOption) (*http.Response, error) {
	cfg := buildRenderedCfg(opts)
	format := cfg.accept
	if format == "" {
		format = cfg.format
	}
	if format == "" {
		format = "image/jpeg"
	}
	if cfg.format == "application/dicom" {
		return nil, &wado.RequestError{Field: "format", Reason: "rendered media type must not be application/dicom"}
	}
	for _, a := range cfg.annotations {
		if a != "patient" && a != "technique" {
			return nil, &wado.RequestError{Field: "annotation", Reason: fmt.Sprintf("unknown kind %q", a)}
		}
	}
	switch cfg.voiFunction {
	case "", WindowFunctionLinear, WindowFunctionLinearExact, WindowFunctionSigmoid:
	default:
		return nil, &wado.RequestError{Field: "windowFunction", Reason: fmt.Sprintf("unknown function %q", cfg.voiFunction)}
	}
	if cfg.quality != 0 && (cfg.quality < 1 || cfg.quality > 100) {
		return nil, &wado.RequestError{Field: "quality", Reason: "must be within 1..100"}
	}
	// §8.3.5.1.5 fixes the keyword values; legacy mode (WithLegacyParamNames)
	// speaks a private dialect whose icccolorspace values are the gateway's
	// own, so it passes through unvalidated.
	if cfg.icc != "" && !c.svc.Core().LegacyParams() && !iccProfileValues[cfg.icc] {
		return nil, &wado.RequestError{Field: "iccprofile", Reason: fmt.Sprintf("unknown value %q", cfg.icc)}
	}
	u.RawQuery = cfg.query(c.svc.Core().LegacyParams()).Encode()

	return c.do(ctx, u, func(req *http.Request) { req.Header.Set("Accept", format) })
}
