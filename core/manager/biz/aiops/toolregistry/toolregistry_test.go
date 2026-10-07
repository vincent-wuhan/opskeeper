package toolregistry

import (
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

func names(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.ToolName())
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// The defect this package was written for.
//
// Six tools match "file". The previous keyword path walked the tool set and
// stopped at max_results, so the five a model received were the five registered
// first — the two that actually have "file" in their *name* could be pushed out
// by four that merely mention it in a sentence. Ranking is what separates them.
func TestSearchRanksByRelevanceNotRegistration(t *testing.T) {
	// Registration order is deliberately hostile: the seven weak matches come
	// first, so a walk-and-truncate returns four tools that merely mention a
	// file in a sentence and none of the three that are about files.
	cat := NewCatalogue([]Entry{
		{Name: "alpha_latency_report", Description: "writes a summary to a file"},
		{Name: "beta_config_export", Description: "exports configuration to a file"},
		{Name: "gamma_audit_dump", Description: "dumps the audit trail to a file"},
		{Name: "delta_state_snapshot", Description: "snapshots service state into a file"},
		{Name: "epsilon_alert_digest", Description: "writes an alert digest to a file"},
		{Name: "zeta_report_bundle", Description: "bundles reports into a file"},
		{Name: "eta_export_sync", Description: "syncs an export file"},
		{Name: "backup_file_index", Description: "index the backup set"},
		{Name: "host_find_large_files", Description: "find the largest files on a host"},
		{Name: "host_stat_file", Description: "stat one file"},
	})

	hits := cat.Search("file", 3)
	got := names(hits)
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3: %v", len(got), got)
	}
	// The three name matches are the only three that survive the cut, which is
	// exactly what the registration-order truncation failed to guarantee.
	want := []string{"backup_file_index", "host_find_large_files", "host_stat_file"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("selected %v, want %v", got, want)
	}
	if strings.Join(names(cat.Search("file", 10))[:3], ",") != strings.Join(want, ",") {
		t.Errorf("ranking did not put the name matches first even without a cut")
	}
}

// A term that appears in a tool's name is stronger evidence than the same term
// in a sentence about it, and the two orders must differ.
func TestSearchPrefersTheNameOverTheDescription(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "aaa_generic", Description: "trace the request path"},
		{Name: "query_traceql", Description: "reads spans"},
	})
	hits := cat.Search("trace", 2)
	if got := names(hits); got[0] != "query_traceql" {
		t.Errorf("order = %v; a name match must outrank a description match even when the "+
			"description match sorts first alphabetically", got)
	}
}

// The routing hint is weighted between the two, because that is what it was
// written for: saying when to pick this tool over the one beside it.
func TestSearchUsesTheRoutingHintAboveTheDescription(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "aaa_query", Description: "runs a query"},
		{Name: "zzz_query", Description: "runs a query", WhenToUse: "when the user asks about spans"},
	})
	hits := cat.Search("spans", 1)
	if got := names(hits); len(got) != 1 || got[0] != "zzz_query" {
		t.Errorf("hits = %v; only the entry whose when_to_use mentions spans should match", got)
	}
}

// A term that matches one tool is stronger evidence than a term that matches
// all of them. Without this, "query" in a family of query_* tools would swamp
// the discriminating word in the same query.
func TestSearchWeighsCommonTermsDown(t *testing.T) {
	entries := []Entry{
		{Name: "query_alpha", Description: "query"},
		{Name: "query_beta", Description: "query"},
		{Name: "query_gamma", Description: "query"},
		{Name: "query_delta", Description: "query"},
	}
	// Both candidates match both terms, so the difference between them is
	// which term the name carries — and "rare" is carried by only one.
	cat := NewCatalogue(append(entries,
		Entry{Name: "zzz_generic", Description: "query rare"},
		Entry{Name: "rare_thing", Description: "query something"},
	))
	hits := cat.Search("query rare", 2)
	got := names(hits)
	if len(got) != 2 {
		t.Fatalf("got %v, want both candidates", got)
	}
	if got[0] != "rare_thing" {
		t.Errorf("order = %v; the entry whose *name* carries the rare term must lead", got)
	}
}

// The match rule is the one the previous implementation had, and it is kept
// deliberately: every whitespace token must appear. Widening it here would make
// a regression indistinguishable from an improvement.
func TestSearchKeepsTheAllTokensMustMatchRule(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "host_find_large_files", Description: "find large files on a host"},
		{Name: "find_outlier_edges", Description: "find ranking outliers among edges"},
	})
	got := names(cat.Search("find files", 5))
	if !contains(got, "host_find_large_files") {
		t.Errorf("host_find_large_files matches both tokens and must be returned: %v", got)
	}
	if contains(got, "find_outlier_edges") {
		t.Errorf("find_outlier_edges does not contain 'files' and must not match: %v", got)
	}
}

// A query that cannot be satisfied returns nothing rather than the nearest
// thing. An empty result is an honest answer; a plausible-looking list of
// unrelated tools is not, because the model cannot tell it apart from a real
// match.
func TestSearchWithAnUnmatchableTermReturnsNothing(t *testing.T) {
	cat := NewCatalogue([]Entry{{Name: "query_promql", Description: "evaluate PromQL"}})
	if got := cat.Search("nonexistent_planet_finder", 5); len(got) != 0 {
		t.Errorf("hits = %v, want none", names(got))
	}
	if got := cat.Search("promql planet", 5); len(got) != 0 {
		t.Errorf("one unmatched term must empty the conjunction, got %v", names(got))
	}
}

// An empty query carries no terms, and answering it with the whole catalogue
// would be a cheap way for a model to page through every schema in one call.
func TestSearchWithNoTermsReturnsNothing(t *testing.T) {
	cat := NewCatalogue([]Entry{{Name: "query_promql", Description: "evaluate PromQL"}})
	for _, query := range []string{"", "   ", "\t\n"} {
		if got := cat.Search(query, 5); len(got) != 0 {
			t.Errorf("Search(%q) returned %v, want none", query, names(got))
		}
	}
	if got := cat.Search("promql", 0); len(got) != 0 {
		t.Errorf("a zero limit returned %v; a caller asking for nothing must get nothing", names(got))
	}
}

// A space-free query — which is how Chinese arrives — must keep matching as one
// phrase. A tokenizer that split it into single characters would turn a precise
// phrase into several loose ones that every tool's text happens to contain.
func TestASpaceFreeQueryMatchesAsOnePhrase(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "query_logql", Description: "查询日志", WhenToUse: "查错误日志时使用"},
		{Name: "query_promql", Description: "查询指标"},
	})
	got := names(cat.Search("查错误日志", 5))
	if !contains(got, "query_logql") {
		t.Errorf("the phrase query did not match the tool whose routing hint carries it: %v", got)
	}
	// "查询指标" must not match the log tool, and vice versa: if the query had
	// been split into characters, every entry containing 查 would match.
	if other := names(cat.Search("查询指标", 5)); contains(other, "query_logql") {
		t.Errorf("a metric phrase matched the log tool: %v", other)
	}
}

// Two identical requests must produce two identical lists. The previous order
// was registration order, which is stable by accident; this one is stable by
// construction, including when the inputs arrive in a different order.
func TestSearchOrderDoesNotDependOnRegistrationOrder(t *testing.T) {
	forward := []Entry{
		{Name: "query_promql", Description: "query metrics"},
		{Name: "query_logql", Description: "query logs"},
		{Name: "query_traceql", Description: "query traces"},
	}
	backward := []Entry{forward[2], forward[1], forward[0]}

	a := names(NewCatalogue(forward).Search("query", 10))
	b := names(NewCatalogue(backward).Search("query", 10))
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Errorf("the same catalogue in two orders produced %v and %v", a, b)
	}
	if strings.Join(a, ",") != "query_logql,query_promql,query_traceql" {
		t.Errorf("order = %v, want the name-order tie-break", a)
	}
}

// An entry with no name can never be selected by anyone, so it is not in the
// catalogue rather than in it as a row nothing resolves.
func TestANamelessEntryIsNotCatalogued(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "", Description: "orphan"},
		{Name: "   ", Description: "whitespace name"},
		{Name: "query_promql", Description: "the only real tool"},
	})
	if cat.Len() != 1 {
		t.Fatalf("catalogue holds %d entries, want 1", cat.Len())
	}
	if !contains(names(cat.Search("orphan", 3)), "query_promql") == false {
		// The orphan's description must not be searchable either: it is not
		// in the catalogue at all.
	}
	if got := cat.Search("orphan", 3); len(got) != 0 {
		t.Errorf("a dropped entry is still searchable: %v", names(got))
	}
}

// Entries is a copy. One caller sorting "just its own view" of a shared
// backing array is how a model's tool list starts depending on another user's
// request.
func TestEntriesReturnsACopy(t *testing.T) {
	cat := NewCatalogue([]Entry{{Name: "a"}, {Name: "b"}})
	got := cat.Entries()
	got[0].Name = "mutated"
	if cat.entries[0].Name != "a" {
		t.Error("Entries returned the backing array")
	}
}

// Filter answers "which write tools does this deployment have" without the
// package growing a query language.
func TestFilterSelectsByDeclaredMetadata(t *testing.T) {
	cat := NewCatalogue([]Entry{
		{Name: "query_promql", Class: "read", Origin: ""},
		{Name: "restart_service", Class: "destructive", Origin: "plugin"},
		{Name: "silence_alert", Class: "write", Origin: ""},
	})
	writes := cat.Filter(func(e Entry) bool { return e.Class != "read" })
	if len(writes) != 2 {
		t.Errorf("Filter kept %d entries, want 2", len(writes))
	}
	plugins := cat.Filter(func(e Entry) bool { return e.Origin == "plugin" })
	if len(plugins) != 1 || plugins[0].Name != "restart_service" {
		t.Errorf("Filter by origin = %v", plugins)
	}
}

// Fuse is the hybrid-retrieval seam. Two rankers that agree on a candidate
// must outrank one that only a single ranker saw, which is the entire reason to
// fuse rather than to pick one list.
func TestFuseRewardsAgreement(t *testing.T) {
	lexical := []Ranked{{Name: "a", Rank: 1}, {Name: "b", Rank: 2}, {Name: "c", Rank: 3}}
	vector := []Ranked{{Name: "c", Rank: 1}, {Name: "d", Rank: 2}}

	hits := Fuse(lexical, vector)
	got := names(hits)
	if len(got) != 4 {
		t.Fatalf("fused %d candidates, want 4: %v", len(got), got)
	}
	// c is 3rd lexically but 1st by the second ranker, so it must beat both b
	// (2nd in one list only) and d (2nd in one list only).
	if got[0] != "c" {
		t.Errorf("order = %v; the candidate both rankers surfaced must lead", got)
	}
}

// A ranker that votes twice for the same tool would silently double its weight
// against every other list.
func TestFuseCountsOneRankerOnce(t *testing.T) {
	once := Fuse([]Ranked{{Name: "a", Rank: 5}})
	twice := Fuse([]Ranked{{Name: "a", Rank: 5}, {Name: "a", Rank: 5}})
	if once[0].Score != twice[0].Score {
		t.Errorf("a duplicated vote changed the score: %v vs %v", once[0].Score, twice[0].Score)
	}
}

// A zero or negative rank means "this ranker did not surface it", not "best".
func TestFuseIgnoresAbsentRanks(t *testing.T) {
	hits := Fuse([]Ranked{{Name: "a", Rank: 0}, {Name: "b", Rank: -1}, {Name: "", Rank: 1}, {Name: "c", Rank: 1}})
	if got := names(hits); len(got) != 1 || got[0] != "c" {
		t.Errorf("hits = %v, want only c", got)
	}
}

// Fuse ties are broken by name for the same reason SortHits does it.
func TestFuseBreaksTiesByName(t *testing.T) {
	got := names(Fuse([]Ranked{{Name: "zeta", Rank: 7}, {Name: "alpha", Rank: 7}}))
	if got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("order = %v, want alpha before zeta", got)
	}
}

// The adapter is the only place that reads a tool, and it refuses the one shape
// that would put an unresolvable row in the catalogue.
func TestEntryFromToolInfoRefusesANamelessTool(t *testing.T) {
	if _, ok := EntryFromToolInfo(nil); ok {
		t.Error("a nil ToolInfo produced an entry")
	}
	if _, ok := EntryFromToolInfo(&basetool.ToolInfo{Name: "  "}); ok {
		t.Error("a nameless ToolInfo produced an entry")
	}
	entry, ok := EntryFromToolInfo(&basetool.ToolInfo{
		Name: "query_promql", Description: "evaluate PromQL",
		WhenToUse: "for metric questions", Class: "read", Origin: "mcp",
	})
	if !ok {
		t.Fatal("a well-formed ToolInfo was refused")
	}
	if entry.Class != "read" || entry.Origin != "mcp" || entry.WhenToUse != "for metric questions" {
		t.Errorf("entry = %+v; the adapter dropped a field policy reads", entry)
	}
}

// A term in a name is worth more than a term that only appears in the entry's
// own haystack by coincidence, and the two weights must be ordered.
func TestFieldWeightsAreOrdered(t *testing.T) {
	if !(weightName > weightWhenToUse && weightWhenToUse > weightDescription) {
		t.Fatalf("field weights lost their order: name=%v when_to_use=%v description=%v",
			weightName, weightWhenToUse, weightDescription)
	}
}
