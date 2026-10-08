package wadors

import (
	"context"

	"github.com/cocosip/go-wado-client"
)

// Capabilities performs OPTIONS-based capabilities discovery on this
// service's base URL (PS3.18 §8.9 Retrieve Capabilities): the reply carries
// the Allow header methods and, when the server provides it, the WADL
// Capabilities Description parsed into wado.WADL.
func (c *Client) Capabilities(ctx context.Context, opts ...wado.CapabilitiesOption) (*wado.Capabilities, error) {
	return c.svc.Capabilities(ctx, opts...)
}
