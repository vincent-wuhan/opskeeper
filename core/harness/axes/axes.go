// Package axes derives what a golden case declares about the three diagnostic
// axes — Localization, Identification and Reason.
//
// It used to live inside cmd/opskeeper-eval, and that location is what made the
// divergence it caused possible: the loop harness in core/harness/runner scores
// the same cases through the same judge and never carried the three axes,
// because `core/harness/runner` cannot import a `cmd` package and the
// derivation was over there. Two harnesses, one judge, and one of them scoring
// a case without the axes the other one measures it on. The numbers were not
// merely different — one path could not flag an ungrounded answer at all.
//
// So the derivation moved here, to a place both can import, and the two
// bridges became two lines each. What this package is: the single place where
// "where the fault is" is defined for the corpus.
//
// The three declarations are derived, not asked for in a separate field, and
// the derivation is the whole point: the three sources are the case's own
// authored material, so no case has to be rewritten to be scored:
//
//	Localization   the case id's family segment (`pg/lock-waits` → `pg`) plus
//	               the injection's identity parameters (table: orders,
//	               namespace: test, topic: order.events, …).
//	Identification the case id's fault segment, tokenized (`lock-waits` →
//	               ["lock", "waits"])
//	Reason         the case's expected root-cause lines — the observations a
//	               grounded reasoning trace has to contain
//
// Why the case id and not the injection type: the id is the authored name of
// the fault ("lock waits"), while the injection type names the script that
// produced it (`pg.inject_lock_chain`, `pg.begin_txn_hold` for
// `pg/long-running-tx`). Which script ran is an implementation detail of the
// injector; the fault's name is what the corpus promises the agent will find.
//
// Five of the twenty shipped cases name no sub-resource at all: the fault was
// injected on "the host", "the replica", "the redis instance", and the case
// says nothing narrower. Their localization is measured on the family alone
// and reported as coarsened (`Thin`) rather than propped up with a name the
// injector never used.
//
// A token shorter than three characters is dropped. Two-character fragments
// like the `tx` in `long-running-tx` are not words an agent can be held to —
// "transaction" does not contain "tx" — and keeping them would fail a correct
// answer for a spelling reason.
package axes

import (
	"strconv"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

const minimumAxisTokenLen = 3

// locusIdentityKeys are the injection parameter keys whose values name a
// resource rather than a knob.
//
// The distinction is what makes localization discriminating: `table: orders`
// names the thing the fault happened to and can be missed, while `cores: 4`
// and `target_load: 98` describe how hard the fault was driven and are
// numbers a correct answer has no reason to repeat. A new case that injects
// through a key not listed here still scores — it just scores on its family —
// and `axes` prints the token count, so the coarseness is visible rather than
// assumed.
var locusIdentityKeys = []string{
	"namespace", "deployment", "pod", "pvc", "node", "target_node",
	"service", "host", "instance", "database",
	"table", "tables", "topic", "consumer_group", "queue",
	"broker_id", "key", "path",
}

// Expectations is one case's declaration of the three axes.
type Expectations struct {
	CaseID    string   `json:"case_id"`
	Locus     []string `json:"locus"`
	FaultType []string `json:"fault_type"`
	Evidence  []string `json:"evidence"`
	// Thin marks a case whose locus is the family alone. Such a case still
	// scores, but every answer that names its own resource family scores 1.0,
	// so the number carries almost nothing. It is reported rather than fixed
	// by inventing a target the injector never used.
	Thin bool `json:"thin,omitempty"`
}

// diagnosticExpectationsOf derives the three axes from a case.
func Of(c *schema.Case) Expectations {
	out := Expectations{CaseID: c.ID}
	// The family is not put through axisTokens: the two-character families
	// ("pg", "mq") are exactly the ones the minimum-length rule exists to drop
	// from prose, and dropping the family would leave those cases with no
	// locus at all.
	out.Locus = append(out.Locus, strings.ToLower(strings.TrimSpace(caseFamily(c.ID))))
	for _, param := range c.Inject {
		for _, key := range locusIdentityKeys {
			value, ok := param.Params[key]
			if !ok {
				continue
			}
			for _, token := range scalarTokens(value) {
				out.Locus = append(out.Locus, token)
			}
		}
	}
	out.Locus = uniqueTokens(out.Locus)

	out.FaultType = uniqueTokens(axisTokens(caseFaultName(c.ID)))
	// The family token alone is a locus no answer can miss.
	out.Thin = len(out.Locus) <= 1
	out.Evidence = append([]string(nil), c.Expect.RootCauseLines...)
	return out
}

// caseFamily is the first path segment of the case id.
func caseFamily(id string) string {
	if index := strings.IndexByte(id, '/'); index > 0 {
		return id[:index]
	}
	return ""
}

// caseFaultName is the last path segment of the case id.
func caseFaultName(id string) string {
	if index := strings.LastIndexByte(id, '/'); index >= 0 {
		return id[index+1:]
	}
	return id
}

// axisTokens lower-cases, splits on separators and drops fragments too short
// to be words.
func axisTokens(raw string) []string {
	fields := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len([]rune(field)) < minimumAxisTokenLen {
			continue
		}
		out = append(out, field)
	}
	return out
}

// scalarTokens flattens an injection parameter value into strings. Booleans
// are dropped: `simulate_network_partition: true` is a mode, not a name.
func scalarTokens(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case int:
		return []string{strconv.Itoa(typed)}
	case int64:
		return []string{strconv.FormatInt(typed, 10)}
	case float64:
		return []string{strconv.FormatFloat(typed, 'f', -1, 64)}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, scalarTokens(item)...)
		}
		return out
	default:
		return nil
	}
}

func uniqueTokens(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		lowered := strings.ToLower(strings.TrimSpace(token))
		if lowered == "" || seen[lowered] {
			continue
		}
		seen[lowered] = true
		out = append(out, lowered)
	}
	return out
}
