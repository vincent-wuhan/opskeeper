package qdrantx

import (
	"fmt"
	"strings"
)

// MatchPayload reports whether payload satisfies every clause in must.
//
// It is the in-process twin of buildFilter, and the two must agree: an
// embedded backend cannot run OpsKeeper's filters server-side (their
// metadata filter is equality-only on strings), so it calls this instead.
// A predicate that selects the right documents against qdrant selects the
// same ones here.
//
// Clause value shapes, mirroring buildFilter:
//
//   - PrefixMatch    → string prefix; an array payload matches when any of
//     its elements carries the prefix.
//   - []string       → set membership: the payload equals one of the listed
//     values, or an array payload contains one of them.
//   - anything else  → equality, with the same "payload array contains the
//     value" relaxation.
//
// A clause the payload has no value for is a non-match. Empty clauses (an
// empty []string or PrefixMatch) are dropped rather than failing, which is
// what buildFilter does by skipping them — so an all-empty must means "no
// filter" on both sides.
func MatchPayload(payload, must map[string]any) bool {
	for key, want := range must {
		have, ok := payload[key]
		if !ok {
			return false
		}
		switch wv := want.(type) {
		case PrefixMatch:
			if wv.Prefix == "" {
				continue
			}
			if !hasPrefix(have, wv.Prefix) {
				return false
			}
		case []string:
			if len(wv) == 0 {
				continue
			}
			if !anyOf(have, wv) {
				return false
			}
		default:
			if !scalarOrContains(have, want) {
				return false
			}
		}
	}
	return true
}

// hasPrefix matches qdrant's match.text on a `text` payload index: a
// prefix test against the field's string value.
func hasPrefix(have any, prefix string) bool {
	switch v := have.(type) {
	case string:
		return strings.HasPrefix(v, prefix)
	case []string:
		for _, s := range v {
			if strings.HasPrefix(s, prefix) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && strings.HasPrefix(s, prefix) {
				return true
			}
		}
	}
	return false
}

// anyOf matches qdrant's match.any.
func anyOf(have any, want []string) bool {
	for _, w := range want {
		if scalarOrContains(have, w) {
			return true
		}
	}
	return false
}

// scalarOrContains matches qdrant's match.value: equal scalars match, and a
// scalar also matches an array payload that holds it (which is how qdrant
// reads a keyword index over a multi-valued field). Comparing rendered
// strings bridges the type changes a payload goes through — a uint64 from
// the caller becomes a float64 once it has been through JSON.
func scalarOrContains(have, want any) bool {
	w := fmt.Sprint(want)
	if fmt.Sprint(have) == w {
		return true
	}
	switch v := have.(type) {
	case []string:
		for _, s := range v {
			if s == w {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if fmt.Sprint(e) == w {
				return true
			}
		}
	}
	return false
}