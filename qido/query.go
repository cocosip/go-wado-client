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
// wildcards and ranges included) or Values (multiple values for one
// attribute, comma-joined into a single parameter per PS3.18 §8.3.4).
//
// Values are UID-validated only for attributes whose VR is UI; for any other
// attribute they are passed through as ordinary matching values (PS3.18
// §8.3.4.6 Multiple Value Matching, e.g. Modality=["CT","MR"]). Set
// Value to "" together with Query.EmptyValueMatching to request empty-value
// matching (§8.3.4.5).
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
			// An empty value is the §8.3.4.5 Empty Value Matching form
			// ("{attributeID}="), which is only meaningful together with the
			// emptyvaluematching toggle; without it the request would match
			// nothing on every conformant server.
			if q.EmptyValueMatching == nil || !*q.EmptyValueMatching {
				return &wado.RequestError{
					Field:  m.Attribute,
					Reason: "match requires a value (set Query.EmptyValueMatching to match on an empty value)",
				}
			}
		case len(m.Values) > 0:
			// Values are validated as UIDs only for UID-valued attributes:
			// §8.3.4.6 allows a value list for any attribute (e.g.
			// Modality=["CT","MR"]), and forcing the UID whitelist on those
			// would make Multiple Value Matching unusable.
			if isUIDAttribute(m.Attribute) {
				for _, uid := range m.Values {
					if err := checkUID(m.Attribute, uid); err != nil {
						return err
					}
				}
			} else {
				for _, v := range m.Values {
					if v == "" {
						return &wado.RequestError{Field: m.Attribute, Reason: "value list must not contain an empty value"}
					}
				}
			}
		}
	}
	for _, f := range q.IncludeFields {
		if strings.EqualFold(f, "all") {
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

// uidAttributes lists the attribute keywords whose VR is UI, i.e. the
// attributes for which PS3.18 §8.3.4.6 defines UID List matching. Values of
// these attributes are UID-validated locally; every other attribute takes
// ordinary matching values.
var uidAttributes = map[string]bool{
	"studyinstanceuid":                true,
	"seriesinstanceuid":               true,
	"sopinstanceuid":                  true,
	"referencedsopinstanceuid":        true,
	"frameofreferenceuid":             true,
	"transfersyntaxuid":               true,
	"presentationcreationinstanceuid": true,
}

// isUIDAttribute reports whether an attribute ID denotes a UID-valued
// attribute. Both keywords and tags are recognised; an unrecognised form is
// treated as a non-UID attribute so that a tag-based UID list still works
// (the server validates it).
func isUIDAttribute(attr string) bool {
	lower := strings.ToLower(attr)
	if uidAttributes[lower] {
		return true
	}
	switch strings.ToUpper(strings.ReplaceAll(attr, ".", "")) {
	case "0020000D", "0020000E", "00080018", "00080055", "00200052", "30060024", "0040A124":
		return true
	}
	return false
}

func validateAttrID(attr string) error {
	if !attrIDPattern.MatchString(attr) {
		return &wado.RequestError{Field: "AttributeID", Reason: fmt.Sprintf("invalid attribute ID %q", attr)}
	}
	return nil
}

// param is one ordered query parameter. list marks a parameter whose value is
// a comma-separated list, where the bare comma is the separator the standard's
// examples use; values of non-list parameters keep their comma escaped.
type param struct {
	key  string
	val  string
	list bool
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
		// An empty value is emitted as "{attributeID}=" (the §8.3.4.5 Empty
		// Value Matching form); the emptyvaluematching toggle that makes it
		// meaningful is appended below. Only a value list carries the bare
		// comma separator.
		ps = append(ps, param{key: m.Attribute, val: v, list: len(m.Values) > 0})
	}
	if len(q.IncludeFields) > 0 {
		// §8.3.4 specifies the literal "all"; a caller writing "ALL" or "All"
		// means the same thing, so it is normalized rather than forwarded
		// verbatim (a server accepting only the literal would answer 400).
		fields := make([]string, len(q.IncludeFields))
		for i, f := range q.IncludeFields {
			if strings.EqualFold(f, "all") {
				fields[i] = "all"
				continue
			}
			fields[i] = f
		}
		ps = append(ps, param{key: "includefield", val: strings.Join(fields, ","), list: true})
	}
	if q.FuzzyMatching != nil {
		ps = append(ps, param{key: "fuzzymatching", val: boolValue(*q.FuzzyMatching)})
	}
	if q.Limit != nil {
		ps = append(ps, param{key: "limit", val: strconv.FormatUint(*q.Limit, 10)})
	}
	if q.Offset > 0 {
		ps = append(ps, param{key: "offset", val: strconv.FormatUint(q.Offset, 10)})
	}
	if q.EmptyValueMatching != nil {
		ps = append(ps, param{key: "emptyvaluematching", val: boolValue(*q.EmptyValueMatching)})
	}
	if q.MultipleValueMatching != nil {
		ps = append(ps, param{key: "multiplevaluematching", val: boolValue(*q.MultipleValueMatching)})
	}
	if len(q.OrderBy) > 0 {
		ps = append(ps, param{key: "orderby", val: strings.Join(q.OrderBy, ","), list: true})
	}
	if len(q.AETitle) > 0 {
		ps = append(ps, param{key: "aetitle", val: strings.Join(q.AETitle, ","), list: true})
	}
	return ps
}

func boolValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
