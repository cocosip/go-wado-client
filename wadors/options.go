package wadors

// RetrieveOption customizes instance / metadata / frames / bulk data
// retrieval.
type RetrieveOption func(*retrieveCfg)

type retrieveCfg struct {
	transferSyntax string
	charset        string
	acceptOverride string
}

// WithTransferSyntax negotiates transcoding in the Accept header, e.g.
// "1.2.840.10008.1.2.1" (Explicit VR Little Endian). PS3.18 also defines the
// wildcard "*" ("any transfer syntax the server supports").
//
// The parameter applies to the multipart retrieves (study / series /
// instance / frames); metadata responses are always dicom+json and never
// carry it.
func WithTransferSyntax(uid string) RetrieveOption {
	return func(c *retrieveCfg) { c.transferSyntax = uid }
}

// WithCharset sets the Accept-Charset header.
func WithCharset(cs string) RetrieveOption {
	return func(c *retrieveCfg) { c.charset = cs }
}

// WithAccept overrides the Accept header entirely (advanced escape hatch).
func WithAccept(h string) RetrieveOption {
	return func(c *retrieveCfg) { c.acceptOverride = h }
}

func buildRetrieveCfg(opts []RetrieveOption) retrieveCfg {
	var cfg retrieveCfg
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// acceptHeader builds: multipart/related; type="..."; transfer-syntax=...
func (cfg retrieveCfg) acceptHeader(partType string) string {
	if cfg.acceptOverride != "" {
		return cfg.acceptOverride
	}
	h := `multipart/related; type="` + partType + `"`
	if cfg.transferSyntax != "" {
		h += `; transfer-syntax=` + cfg.transferSyntax
	}
	return h
}
