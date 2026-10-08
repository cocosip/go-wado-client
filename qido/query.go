package qido

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cocosip/go-wado-client"
)

// Query holds the parameters of one search (PS3.18 §10.6.1 request, §8.3.4
// common query parameters). Zero fields are omitted from the request; the
// server defaults apply.
type Query struct {
	// Match holds the matching keys ({attributeID}={value} pairs) in caller
	// order; the attribute ID may be a tag ("0020000D"), a keyword
	// ("StudyInstanceUID") or a dotted sequence path
	// ("00101002.00100020"). Match values follow the C-FIND matching rules:
	// exact, wildcard (* ?), and date/time ranges ("20130509-20130510",
	// open-ended as "20130509-" / "-20130510").
	Match []Match

	// IncludeFields requests additional return attributes: each entry is an
	// attribute ID or the literal "all".
	IncludeFields []string

	// FuzzyMatching toggles fuzzy person-name matching; nil omits the
	// parameter and leaves the server default in effect.
	FuzzyMatching *bool

	// Limit caps the number of results; nil omits the parameter (the server
	// returns its own maximum). One results page per request — paging is
	// client-driven per the standard: advance Offset by the page size and
	// watch Results.AdditionalResults.
	Limit *uint64

	// Offset skips results at the start of the result list (0, the default,
	// omits the parameter).
	Offset uint64

	// EmptyValueMatching toggles matching on empty values; nil omits the
	// parameter (PS3.18 §8.3.4 common query parameter set).
	EmptyValueMatching *bool

	// MultipleValueMatching toggles matching of multi-valued attributes; nil
	// omits the parameter.
	MultipleValueMatching *bool

	// OrderBy sorts the results: entries are attribute IDs, a leading "-"
	// requests descending order ("-StudyDate"). Emitted as one
	// comma-joined orderby parameter (dcm4chee syntax). Not part of the
	// PS3.18 common query parameter table — a widely supported ecosystem
	// extension; servers that ignore it answer unsorted.
	OrderBy []string

	// AETitle restricts the search to the given Retrieve AE Title(s)
	// (0008,0054), comma-joined into one aetitle parameter (dcm4chee
	// syntax). Ecosystem extension like OrderBy.
	AETitle []string

	// Extra passes through private gateway parameters; entries override
	// same-name standard parameters (escape hatch, cf. wadors.WithRawQuery).
	Extra url.Values
}

// Match is one matching key. Set exactly one of Value (single value,
// wildcards and ranges included) or Values (UID List matching — the UIDs are
// comma-joined into a single value per PS3.18 §8.3.4).
type Match struct {
	Attribute string
	Value     string
	Values    []string
}

// Bool returns a pointer to b: the helper for the tri-state Query flags
// (FuzzyMatching etc. — nil omits the parameter).
func Bool(b bool) *bool { return &b }

// SearchOption customizes a search request.
type SearchOption func(*searchCfg)

type searchCfg struct {
	accept string
}

// WithAccept overrides the Accept header entirely (advanced escape hatch).
// The default is application/dicom+json; this client parses dicom+json only.
func WithAccept(h string) SearchOption {
	return func(c *searchCfg) { c.accept = h }
}

func buildSearchCfg(opts []SearchOption) searchCfg {
	var cfg searchCfg
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// attrIDPattern matches the PS3.18 §8.3.4 attribute ID forms: tag
// ("0020000D"), keyword ("StudyInstanceUID") and dotted sequence paths
// ("00101002.00100020"). It is a whitelist for everything the client places
// in a query key position (attribute IDs, orderby entries).
var attrIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

// validate performs the local checks: cases the standard mandates a
// server-side 400 for are rejected up front, and every key-position string
// is whitelisted (it reaches the request URL unescaped).
func (q Query) validate(checkUID func(field, uid string) error) error {
	for _, m := range q.Match {
		if err := validateAttrID(m.Attribute); err != nil {
			return err
		}
		switch {
		case m.Value != "" && len(m.Values) > 0:
			return &wado.RequestError{Field: m.Attribute, Reason: "set either Value or Values, not both"}
		case m.Value == "" && len(m.Values) == 0:
			return &wado.RequestError{Field: m.Attribute, Reason: "match requires a value"}
		case len(m.Values) > 0:
			for _, uid := range m.Values {
				if err := checkUID(m.Attribute, uid); err != nil {
					return err
				}
			}
		}
	}
	for _, f := range q.IncludeFields {
		if f == "all" {
			continue
		}
		if err := validateAttrID(f); err != nil {
			return err
		}
	}
	if q.Limit != nil && *q.Limit == 0 {
		return &wado.RequestError{Field: "Limit", Reason: "must be >= 1"}
	}
	for _, o := range q.OrderBy {
		if err := validateAttrID(strings.TrimPrefix(o, "-")); err != nil {
			return err
		}
	}
	for _, ae := range q.AETitle {
		if ae == "" || strings.ContainsFunc(ae, func(r rune) bool { return r < ' ' || r > '~' }) {
			return &wado.RequestError{Field: "AETitle", Reason: "must be non-empty printable ASCII"}
		}
	}
	return nil
}

func validateAttrID(attr string) error {
	if !attrIDPattern.MatchString(attr) {
		return &wado.RequestError{Field: "AttributeID", Reason: fmt.Sprintf("invalid attribute ID %q", attr)}
	}
	return nil
}

// param is one ordered query parameter.
type param struct {
	key string
	val string
}

// params renders the query parameters in a stable order: matching keys
// (caller order), includefield, fuzzymatching, limit, offset,
// emptyvaluematching, multiplevaluematching, orderby, aetitle. List-valued
// parameters (includefield, UID List matches, orderby, aetitle) are
// comma-joined into a single parameter, the form used by the PS3.18
// examples.
func (q Query) params() []param {
	ps := make([]param, 0, len(q.Match)+len(q.IncludeFields)+8)
	for _, m := range q.Match {
		v := m.Value
		if len(m.Values) > 0 {
			v = strings.Join(m.Values, ",")
		}
		ps = append(ps, param{m.Attribute, v})
	}
	if len(q.IncludeFields) > 0 {
		ps = append(ps, param{"includefield", strings.Join(q.IncludeFields, ",")})
	}
	if q.FuzzyMatching != nil {
		ps = append(ps, param{"fuzzymatching", boolValue(*q.FuzzyMatching)})
	}
	if q.Limit != nil {
		ps = append(ps, param{"limit", strconv.FormatUint(*q.Limit, 10)})
	}
	if q.Offset > 0 {
		ps = append(ps, param{"offset", strconv.FormatUint(q.Offset, 10)})
	}
	if q.EmptyValueMatching != nil {
		ps = append(ps, param{"emptyvaluematching", boolValue(*q.EmptyValueMatching)})
	}
	if q.MultipleValueMatching != nil {
		ps = append(ps, param{"multiplevaluematching", boolValue(*q.MultipleValueMatching)})
	}
	if len(q.OrderBy) > 0 {
		ps = append(ps, param{"orderby", strings.Join(q.OrderBy, ",")})
	}
	if len(q.AETitle) > 0 {
		ps = append(ps, param{"aetitle", strings.Join(q.AETitle, ",")})
	}
	return ps
}

func boolValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
