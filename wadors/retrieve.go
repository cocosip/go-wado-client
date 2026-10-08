package wadors

import (
	"context"
	"net/http"
	"net/url"
)

// RetrieveStudy retrieves an entire Study (multipart/related, one DICOM
// file per part).
func (c *Client) RetrieveStudy(ctx context.Context, studyUID string, opts ...RetrieveOption) (*Multipart, error) {
	if err := c.checkUIDs("studyUID", studyUID); err != nil {
		return nil, err
	}
	return c.retrieveMultipart(ctx, c.resourceURL("studies", studyUID), "application/dicom", opts)
}

// RetrieveSeries retrieves an entire Series.
func (c *Client) RetrieveSeries(ctx context.Context, studyUID, seriesUID string, opts ...RetrieveOption) (*Multipart, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID); err != nil {
		return nil, err
	}
	return c.retrieveMultipart(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID), "application/dicom", opts)
}

// RetrieveInstance retrieves a single instance (a single multipart part;
// use ReadAll to get the bytes).
func (c *Client) RetrieveInstance(ctx context.Context, studyUID, seriesUID, sopUID string, opts ...RetrieveOption) (*Multipart, error) {
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	return c.retrieveMultipart(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID),
		"application/dicom", opts)
}

// applyCharset applies the PS3.18 charset query parameter (§8.3.3.2, the
// hyperlink-friendly equivalent of the Accept-Charset header field);
// WADO-URI uses the same parameter name in its query string.
func applyCharset(u *url.URL, charset string) {
	if charset == "" {
		return
	}
	q := u.Query()
	q.Set("charset", charset)
	u.RawQuery = q.Encode()
}

func (c *Client) retrieveMultipart(ctx context.Context, u *url.URL, partType string, opts []RetrieveOption) (*Multipart, error) {
	cfg := buildRetrieveCfg(opts)
	// "*" is the PS3.18 wildcard ("any transfer syntax"), not a UID.
	if cfg.transferSyntax != "" && cfg.transferSyntax != "*" {
		if err := c.svc.Core().CheckUID("transferSyntax", cfg.transferSyntax); err != nil {
			return nil, err
		}
	}
	applyCharset(u, cfg.charset)
	resp, err := c.do(ctx, u, func(req *http.Request) {
		req.Header.Set("Accept", cfg.acceptHeader(partType))
	})
	if err != nil {
		return nil, err
	}
	return newMultipart(resp)
}
