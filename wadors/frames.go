package wadors

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// RetrieveFrames retrieves the given frames (multipart/related,
// type=application/octet-stream). Frames are 1-based; the list is sorted and
// de-duplicated automatically to satisfy the standard's ascending-order
// requirement.
func (c *Client) RetrieveFrames(ctx context.Context, studyUID, seriesUID, sopUID string, frames []int, opts ...RetrieveOption) (*Multipart, error) {
	fl, err := framesList(frames)
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
