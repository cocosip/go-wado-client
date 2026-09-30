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

func (c *Client) retrieveMultipart(ctx context.Context, u *url.URL, partType string, opts []RetrieveOption) (*Multipart, error) {
	cfg := buildRetrieveCfg(opts)
	if cfg.transferSyntax != "" {
		if err := c.core.CheckUID("transferSyntax", cfg.transferSyntax); err != nil {
			return nil, err
		}
	}
	resp, err := c.do(ctx, u, func(req *http.Request) {
		req.Header.Set("Accept", cfg.acceptHeader(partType))
		if cfg.charset != "" {
			req.Header.Set("Accept-Charset", cfg.charset)
		}
	})
	if err != nil {
		return nil, err
	}
	return newMultipart(resp)
}
