package wadors

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// frameListWarnLimit is the rendered FrameList length (in characters) above
// which a warning is logged: the list travels in the URL, and gateways start
// answering 414 URL Too Long in this magnitude. Batch the calls beyond it.
const frameListWarnLimit = 2048

// RetrieveFrames retrieves the given frames (multipart/related,
// type=application/octet-stream). Frames are 1-based; the list is sorted and
// de-duplicated automatically to satisfy the standard's ascending-order
// requirement (PS3.18 §10.1.1: {frames} is "a comma-separated list of Frame
// numbers, in ascending order").
func (c *Client) RetrieveFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RetrieveOption) (*Multipart, error) {
	fl, err := c.buildFrameList(frames)
	if err != nil {
		return nil, err
	}
	if err := c.checkUIDs("studyUID", studyUID, "seriesUID", seriesUID, "sopInstanceUID", sopUID); err != nil {
		return nil, err
	}
	return c.retrieveMultipart(ctx,
		c.resourceURL("studies", studyUID, "series", seriesUID, "instances", sopUID, "frames", fl),
		"application/octet-stream", opts)
}

// buildFrameList renders the wire form of a frame list and warns when it
// grows towards the URL-length limits of typical gateways.
func (c *Client) buildFrameList(frames []int) (string, error) {
	fl, err := framesList(frames)
	if err != nil {
		return "", err
	}
	if len(fl) > frameListWarnLimit {
		c.core.Logger().Warn("wadors: very long frame list; gateways may answer 414 URL Too Long, consider batching",
			"length", len(fl), "limit", frameListWarnLimit)
	}
	return fl, nil
}

// framesList validates, sorts and de-duplicates frame numbers and renders
// the comma-separated frame list.
func framesList(frames []int) (string, error) {
	if len(frames) == 0 {
		return "", &wado.RequestError{Field: "frames", Reason: "empty frame list"}
	}
	fl := slices.Clone(frames)
	slices.Sort(fl)
	fl = slices.Compact(fl)
	for _, f := range fl {
		if f < 1 {
			return "", &wado.RequestError{Field: "frames", Reason: fmt.Sprintf("frame number %d < 1", f)}
		}
	}
	parts := make([]string, len(fl))
	for i, f := range fl {
		parts[i] = strconv.Itoa(f)
	}
	return strings.Join(parts, ","), nil
}
