// Package toolregistry is the catalogue of tools a deployment can reach, and
// the retrieval that turns a sentence into the shortlist worth showing.
//
// It exists because the plan's item 6 is a scale claim, not a taste one: at
// 88 tools a model can be handed a flat list, and at several hundred it
// cannot. Something has to decide which of them a given turn sees, and the
// thing that decides has to derive its answer from what each tool says about
// itself — otherwise every new plugin means editing a Go map in the host.
//
// Three consumers, one catalogue:
//
//   - ToolSearch, so the model's keyword query is answered by relevance
//     rather than by registration order (see Search's doc for the defect
//     that replaces).
//   - The console and a future MCP surface, which need to ask "what can this
//     deployment do about slow queries" without owning a tool instance.
//   - Any planner that wants a shortlist before a turn starts.
//
// What this package is NOT is a policy layer. It ranks, and it filters by
// declared metadata; it never decides what a caller is *allowed* to reach.
// The bag a turn may use is resolved upstream by role, persona and the live
// write gate, and retrieval runs on that already-narrowed set — so a defect
// here can hide a tool from a turn, and can never hand one over. That
// asymmetry is why this is allowed to be a ranking library rather than a gate.
package toolregistry

import (
	"math"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// Entry is one tool as the catalogue sees it.
//
// It is a value type rather than a `basetool.BaseTool` on purpose. The
// catalogue is asked questions by callers that have no tool to hand (a console
// listing capabilities, a planner deciding what to load), and an interface
// would force every one of them to construct a fake tool in order to ask about
// a real one. EntryFromToolInfo is the only place that reads a BaseTool.
type Entry struct {
	// Name is the wire name.
	Name string `json:"name"`
	// Description is what the tool does.
	Description string `json:"description"`
	// WhenToUse is the routing hint its siblings do not carry.
	WhenToUse string `json:"when_to_use,omitempty"`
	// Class is read / write / destructive.
	Class string `json:"class,omitempty"`
	// Origin is builtin / mcp / skill / plugin.
	Origin string `json:"origin,omitempty"`
}

// Hit is one ranked entry.
//
// Score is exported because a caller that wants a cut-off needs the number
// rather than the ordering, and because "the relevant tool outranks the
// irrelevant one" is a weaker claim than "and by this much". Matched lists the
// query terms the entry matched, in query order — a relevance ranking an
// operator cannot interrogate is indistinguishable from a random one when it
// is wrong.
type Hit struct {
	Entry   Entry    `json:"entry"`
	Score   float64  `json:"score"`
	Matched []string `json:"matched,omitempty"`
}

// ToolName returns the hit's wire name. Convenience for the callers that only
// need names, which is most of them.
func (h Hit) ToolName() string { return h.Entry.Name }

// EntryFromToolInfo flattens a host tool's metadata.
//
// It is the single adapter, so the catalogue the model searches and the
// catalogue a console renders can never be two slightly different lists. A nil
// or nameless info is refused rather than indexed as an empty entry: a row
// nothing can ever resolve is how a console ends up rendering a blank line an
// operator cannot act on.
func EntryFromToolInfo(info *basetool.ToolInfo) (Entry, bool) {
	if info == nil || strings.TrimSpace(info.Name) == "" {
		return Entry{}, false
	}
	return Entry{
		Name:        info.Name,
		Description: info.Description,
		WhenToUse:   info.WhenToUse,
		Class:       info.Class,
		Origin:      info.Origin,
	}, true
}

// Field weights.
//
// These are ordinals, not probabilities, and they say one thing: a term in a
// tool's name is stronger evidence than the same term in its when-to-use, which
// is stronger than in its description. The absolute values do not matter,
// because the output is used for ordering and for a caller-chosen cut-off, never
// against a threshold baked in here.
const (
	weightName        = 3.0
	weightDescription = 1.0
	weightWhenToUse   = 2.0
)

// Catalogue is a set of tools that can be searched.
//
// It is built from a plain value slice and holds no reference to the tools
// themselves, so the same type serves the model's search, a console's listing,
// and a test with no tool implementation at all.
type Catalogue struct {
	entries []Entry
	// haystacks is the lower-cased text each entry is matched against,
	// precomputed once. It is derived, never authoritative: Search never
	// re-reads the fields, so there is exactly one place where "what is
	// searchable" is decided.
	haystacks []string
	// names is the lower-cased name, kept separate because matching the
	// name is worth more and asking that question per token is on the hot
	// path.
	names []string
	// whenToUse is the lower-cased routing hint, same reason.
	whenToUse []string
}

// NewCatalogue builds the catalogue, dropping entries that could never be
// selected.
func NewCatalogue(entries []Entry) *Catalogue {
	cat := &Catalogue{
		entries:   make([]Entry, 0, len(entries)),
		haystacks: make([]string, 0, len(entries)),
		names:     make([]string, 0, len(entries)),
		whenToUse: make([]string, 0, len(entries)),
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Name) == "" {
			continue
		}
		name := strings.ToLower(entry.Name)
		hint := strings.ToLower(entry.WhenToUse)
		cat.entries = append(cat.entries, entry)
		cat.names = append(cat.names, name)
		cat.whenToUse = append(cat.whenToUse, hint)
		cat.haystacks = append(cat.haystacks, name+"\n"+strings.ToLower(entry.Description)+"\n"+hint)
	}
	return cat
}

// Len reports how many entries the catalogue holds.
func (c *Catalogue) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Entries returns a copy of the catalogue.
//
// A copy, because the catalogue is shared by every concurrent turn and one
// caller sorting "just its own view" of the same backing array is how a
// model's tool list starts depending on another user's request.
func (c *Catalogue) Entries() []Entry {
	if c == nil {
		return nil
	}
	return append([]Entry(nil), c.entries...)
}

// Search answers a keyword query with the most relevant entries.
//
// Two properties, and the second is what this package was written for:
//
//  1. **Match semantics are unchanged from the keyword path it replaces.**
//     Every whitespace-separated query token, lower-cased, must appear as a
//     substring somewhere in the entry's name, description or when-to-use.
//     That is exactly the predicate ToolSearch already had, and it is kept
//     deliberately: this change is about *ordering*, and a change that also
//     widened what matches would make a regression indistinguishable from an
//     improvement. It also means a Chinese query, which arrives as one
//     space-free run, keeps matching by substring rather than being split
//     into single characters — a tokenizer that split it would turn "查错误
//     日志" from one precise phrase into several loose ones.
//
//  2. **The order is relevance, not registration.** The path this replaces
//     walked the tool set and stopped at max_results, which the code said out
//     loud ("no scoring / fuzzy ranking in v1") and which means the five
//     schemas a model receives for a query matching fifty tools are the five
//     that happened to be registered first. Two tools equally able to answer
//     the question were separated by nothing but their position in a slice.
//
// An empty query matches nothing. It carries no terms, and returning the whole
// catalogue for it would be a way for a model to page through every schema with
// one cheap call.
func (c *Catalogue) Search(query string, limit int) []Hit {
	if c == nil || limit <= 0 || len(c.entries) == 0 {
		return nil
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil
	}

	// Document frequency, over the catalogue, for the inverse-document-
	// frequency weight below. Without it every common word — "query", "get" —
	// would dominate the ranking, which is the failure mode of ranking by the
	// number of terms matched.
	documentFrequency := make(map[string]int, len(terms))
	for _, term := range terms {
		count := 0
		for _, haystack := range c.haystacks {
			if strings.Contains(haystack, term) {
				count++
			}
		}
		if count == 0 {
			// A term nothing matches makes the conjunction empty. This is the
			// answer the previous implementation gave too, reached without
			// scanning for a score.
			return nil
		}
		documentFrequency[term] = count
	}

	hits := make([]Hit, 0, 8)
	for position, haystack := range c.haystacks {
		score := 0.0
		matched := make([]string, 0, len(terms))
		all := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				all = false
				break
			}
			score += idf(len(c.entries), documentFrequency[term]) * c.fieldWeight(position, term)
			matched = append(matched, term)
		}
		if !all {
			continue
		}
		hits = append(hits, Hit{Entry: c.entries[position], Score: score, Matched: matched})
	}
	SortHits(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// fieldWeight reports the strongest field a term appeared in, for one entry.
//
// Strongest-wins rather than summed: a term that is both the name and the
// description of a tool is one piece of evidence, and adding the two would let
// a verbose description outrank a tool whose *name* is the thing being asked
// for.
func (c *Catalogue) fieldWeight(position int, term string) float64 {
	switch {
	case strings.Contains(c.names[position], term):
		return weightName
	case strings.Contains(c.whenToUse[position], term):
		return weightWhenToUse
	default:
		return weightDescription
	}
}

// idf is the standard inverse document frequency, smoothed so a term that
// appears everywhere still scores above zero and a term that appears once
// scores highest.
func idf(documents, documentFrequency int) float64 {
	if documents == 0 || documentFrequency == 0 {
		return 0
	}
	return math.Log(1 + float64(documents)/float64(documentFrequency))
}

// SortHits orders by score, then by name.
//
// The tie-break is not cosmetic. Scores collide whenever two tools share
// vocabulary, which is the normal case in a family of siblings (query_promql
// and query_logql both carry "query"), and an order that depended on map
// iteration or on registration would make the model's tool list differ between
// two identical requests. Name order is arbitrary but it is *stable*, and
// stability is the property that lets a caller's answer be reproduced in a bug
// report.
func SortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Entry.Name < hits[j].Entry.Name
	})
}

// Filter returns the entries a predicate admits, preserving order.
//
// It exists so the catalogue can answer "which write tools does this
// deployment have" without a second type: a caller narrows by class or origin
// and reads Entries, rather than the package growing a query language.
func (c *Catalogue) Filter(keep func(Entry) bool) []Entry {
	if c == nil || keep == nil {
		return nil
	}
	out := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		if keep(entry) {
			out = append(out, entry)
		}
	}
	return out
}

func queryTerms(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	if len(fields) == 0 {
		return nil
	}
	// De-duplicated while keeping query order: a word repeated in a query is
	// one piece of evidence, not two, or "log log log" would outrank "log".
	terms := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !seen[field] {
			seen[field] = true
			terms = append(terms, field)
		}
	}
	return terms
}

// RRFConstant is the rank-fusion smoothing constant.
//
// It is 60 because that is the value the knowledge layer's incident recall
// already uses (core/manager/control/incident/memory.go), and the same number
// in both places is not a coincidence to be maintained separately — it is the
// published default for reciprocal rank fusion, and a second value here would
// make two hybrid retrievals in one product behave differently for no stated
// reason.
const RRFConstant = 60.0

// Ranked is one candidate as a ranker scored it: the tool's name and its
// 1-based rank in that ranker's own output (1 = best). Zero means the ranker
// did not surface it.
type Ranked struct {
	Name string
	Rank int
}

// Fuse merges several rankers' outputs into one ordering by reciprocal rank
// fusion.
//
// This is the hybrid-retrieval seam the plan asks for: the lexical ranker is
// Search above, and the second list is whatever a caller has — an embedding
// search over tool descriptions, a rule-based ranker, a model's own shortlist.
// Fusing *ranks* rather than scores is the point: two rankers almost never
// produce comparable numbers, and every attempt to normalise them imports a
// calibration nobody can justify.
//
// As of decision 189 no production code passes a second list: the only caller
// in the tree is this package's own test, and `scripts/deadcode` reports
// `Fuse:test-only` for this file. So there is a working lexical ranker and a
// tested fusion, and **no hybrid retrieval actually running**. The seam is
// ready for the second ranker; the plan's premise for needing it -- plugins in
// the hundreds -- is not what this fleet is at, and building the second ranker
// early would be a guess about a scale this repository has not reached. The
// sentence above is a promise about what Fuse accepts, not a report that
// anything supplies it.
//
// What it is not is a second implementation of the incident recall's
// selection. That one is coupled to `runbook:` / `knowledge:` source
// priorities, which is a policy about incident evidence and has no meaning for
// a tool name. Sharing the constant and the formula is the reuse; sharing the
// tie-break would be a category error.
func Fuse(lists ...[]Ranked) []Hit {
	scores := make(map[string]float64, 16)
	for _, list := range lists {
		seen := make(map[string]bool, len(list))
		for _, candidate := range list {
			if candidate.Name == "" || candidate.Rank <= 0 {
				continue
			}
			// One ranker voting twice for the same tool is a defect in that
			// ranker, and counting it twice would silently double its weight
			// against every other list.
			if seen[candidate.Name] {
				continue
			}
			seen[candidate.Name] = true
			scores[candidate.Name] += 1 / (RRFConstant + float64(candidate.Rank))
		}
	}
	hits := make([]Hit, 0, len(scores))
	for name, score := range scores {
		hits = append(hits, Hit{Entry: Entry{Name: name}, Score: score})
	}
	SortHits(hits)
	return hits
}
