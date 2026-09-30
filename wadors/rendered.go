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
)

// RenderedOption customizes rendered retrieval.
type RenderedOption func(*renderedCfg)

type renderedCfg struct {
	format      string
	quality     int
	viewport    [2]int
	window      [2]float64
	hasWindow   bool
	annotations []string
	icc         string
	raw         url.Values
}

// WithRenderedFormat sets the rendered media type (default image/jpeg;
// application/dicom is not allowed).
func WithRenderedFormat(m string) RenderedOption {
	return func(c *renderedCfg) { c.format = m }
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

// query builds the rendered query parameters; names switch between classic
// and modern according to the core setting.
func (cfg renderedCfg) query(modern bool) url.Values {
	q := url.Values{}
	if modern {
		if len(cfg.annotations) > 0 {
			q.Set("annotation", strings.Join(cfg.annotations, ","))
		}
		if cfg.hasWindow {
			q.Set("window", formatFloat(cfg.window[0])+","+formatFloat(cfg.window[1]))
		}
		if cfg.icc != "" {
			q.Set("iccprofile", cfg.icc)
		}
	} else {
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
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Rendered is a rendered response (a single-part image stream such as
// image/png).
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
	return c.retrieveRendered(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "rendered"), opts)
}

// RetrieveRenderedFrames retrieves the rendered image of the given frames
// (1-based, sorted and de-duplicated automatically).
func (c *Client) RetrieveRenderedFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RenderedOption) (*Rendered, error) {
	fl, err := framesList(frames)
	if err != nil {
		return nil, err
	}
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	return c.retrieveRendered(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "frames", fl, "rendered"), opts)
}

func (c *Client) retrieveRendered(ctx context.Context, u *url.URL, opts []RenderedOption) (*Rendered, error) {
	cfg := buildRenderedCfg(opts)
	format := cfg.format
	if format == "" {
		format = "image/jpeg"
	}
	if format == "application/dicom" {
		return nil, &wado.RequestError{Field: "format", Reason: "rendered media type must not be application/dicom"}
	}
	for _, a := range cfg.annotations {
		if a != "patient" && a != "technique" {
			return nil, &wado.RequestError{Field: "annotation", Reason: fmt.Sprintf("unknown kind %q", a)}
		}
	}
	if cfg.quality != 0 && (cfg.quality < 1 || cfg.quality > 100) {
		return nil, &wado.RequestError{Field: "quality", Reason: "must be within 1..100"}
	}
	u.RawQuery = cfg.query(c.core.ModernParams()).Encode()

	resp, err := c.do(ctx, u, func(req *http.Request) { req.Header.Set("Accept", format) })
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	// Tolerance: a few servers wrap rendered images in multipart; take the
	// first part.
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
		return &Rendered{ContentType: ct, Body: p, Header: resp.Header, resp: resp}, nil
	}
	return &Rendered{ContentType: ct, Body: resp.Body, Header: resp.Header, resp: resp}, nil
}
