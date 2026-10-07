package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"sort"
	"strings"
)

// The shared-symbol report answers the question the seam report raised and
// could not: decision 231 found that every remaining edge carries data shapes,
// and 0% of aiops's outbound imports touch only a Usecase. So the lever is not
// "turn this Usecase into a port" — there is no such edge. The lever is the
// shapes themselves, and the only thing that makes a shape cheap to move is
// other domains already carrying it.
//
// A shape selected by N domains is N potential edges that one move can close,
// because the type has exactly one home and every consumer can name it there.
// A shape selected by one domain is that domain's private vocabulary and
// moving it buys nothing: the edge that selects it still selects it, just from
// a different path.
//
// That distinction is what this report is for, and it is not derivable by
// reading the graph by eye: the same symbol name appears in packages that do
// not declare it, the same import statement selects nine symbols at once, and
// "which of these is shared" changes with every edit. It is read off the
// parsed tree for the same reason the seam report is — a hand-sorted version
// of this question has already been wrong twice (decisions 228, 230).
type sharedRow struct {
	sym       string
	consumers map[string]bool
	// targets is the set of domains that DECLARE the symbol, which is one
	// in a healthy tree and more when two domains each have their own
	// same-named type — a case worth seeing, because moving one of them
	// would silently change what the other means.
	targets map[string]bool
	// shapes renders one line per declaring package: that package's field
	// list for this symbol. It is only filled for symbols with more than one
	// declaring domain, because that is the only case where "there are two of
	// these" is a question a reader has to answer before moving anything.
	shapes []string
}

func (r sharedRow) fanOut() int { return len(r.consumers) }

// printShared writes the symbols that more than one domain selects, heaviest
// first. It is a report, not a gate: the tree is allowed to have no shared
// symbols at all, and the number that matters is whether the top of the list
// is worth a move.
func printShared(w io.Writer, sources []source, r rules) {
	// symbol -> consuming domain -> declaring domain
	consumers := map[string]map[string]map[string]bool{}
	// owner[sym] is the import path the symbol was last seen declared in. A
	// name with several declaring domains is reached through several import
	// paths, and the last one wins; that is fine here because the report
	// only uses it to look up field lists for a symbol that is already known
	// to be ambiguous, and every owner is printed.
	owner := map[string]string{}
	// package -> domain, so a symbol's declaring side can be resolved
	pkgDomain := map[string]string{}
	// fields[pkg][sym] = the struct's field names, in declaration order.
	fields := map[string]map[string][]string{}
	for _, src := range sources {
		d := domainOf(src.path)
		if d == "" {
			continue
		}
		pkg := pkgKey(src)
		pkgDomain[pkg] = d
		if src.file == nil {
			continue
		}
		if fields[pkg] == nil {
			fields[pkg] = map[string][]string{}
		}
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				var names []string
				for _, f := range st.Fields.List {
					for _, id := range f.Names {
						names = append(names, id.Name)
					}
				}
				fields[pkg][ts.Name.Name] = names
			}
		}
	}

	for _, src := range sources {
		if src.test {
			continue
		}
		from := domainOf(src.path)
		if from == "" {
			continue
		}
		for imp, syms := range src.used {
			to := domainOf(imp)
			if to == "" || to == from || r.shared[to] != "" {
				continue
			}
			for sym := range syms {
				if consumers[sym] == nil {
					consumers[sym] = map[string]map[string]bool{}
				}
				if consumers[sym][from] == nil {
					consumers[sym][from] = map[string]bool{}
				}
				consumers[sym][from][to] = true
				owner[sym] = imp
			}
		}
	}

	var rows []sharedRow
	for sym, byConsumer := range consumers {
		row := sharedRow{sym: sym, consumers: map[string]bool{}, targets: map[string]bool{}}
		for from, targets := range byConsumer {
			row.consumers[from] = true
			for to := range targets {
				row.targets[to] = true
			}
		}
		if len(row.consumers) > 1 {
			if len(row.targets) > 1 {
				row.shapes = shapesOf(sym, owner[sym], fields, sources)
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if len(rows[i].consumers) != len(rows[j].consumers) {
			return len(rows[i].consumers) > len(rows[j].consumers)
		}
		return rows[i].sym < rows[j].sym
	})

	fmt.Fprintln(w, "shared symbols: types more than one bounded context selects")
	fmt.Fprintln(w, "  A shape N domains carry is N edges one move can close, because the type")
	fmt.Fprintln(w, "  would have exactly one home that all of them can name. A shape only one")
	fmt.Fprintln(w, "  domain carries is that domain's private vocabulary — moving it buys")
	fmt.Fprintln(w, "  nothing, the edge that selects it still selects it from a new path.")
	fmt.Fprintln(w, "  cols: consumers = domains that select it; targets = domains that declare it")
	fmt.Fprintln(w, "  (more than one target means two domains each have their own same-named")
	fmt.Fprintln(w, "  type, which is worth seeing before moving either).")
	fmt.Fprintln(w)
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (none — every cross-domain edge selects only private vocabulary)")
		return
	}
	for _, row := range rows {
		flag := ""
		if len(row.targets) > 1 {
			flag = "  <-- same name, several owners"
		}
		fmt.Fprintf(w, "  %2d  %-30s consumers: %-46s targets: %s%s\n",
			len(row.consumers), row.sym,
			strings.Join(sortedNames(row.consumers), " "),
			strings.Join(sortedNames(row.targets), " "),
			flag)
		// A warning that does not say why is a warning nobody can act on.
		// These are the entries a ranking by consumer count puts at the very
		// top, so the report prints what each owner actually holds: if the
		// field lists are unrelated, the name is a coincidence and the
		// symbol is not a move candidate at all.
		for _, line := range row.shapes {
			fmt.Fprintf(w, "        %s\n", line)
		}
	}
	fmt.Fprintf(w, "\n  %d symbols are selected by more than one domain\n", len(rows))
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shapesOf renders one line per declaring package for a symbol that more than
// one domain declares. A package with no struct body — an interface, an alias,
// a constant — gets said so rather than silently printing nothing, because a
// missing line would read as "this one has no fields" when the truth is that
// the tool did not look.
func shapesOf(sym, hint string, fields map[string]map[string][]string, sources []source) []string {
	pkgs := map[string]bool{}
	if hint != "" {
		pkgs[hint] = true
	}
	for _, src := range sources {
		if src.declared == nil {
			continue
		}
		if _, ok := src.declared[sym]; ok {
			pkgs[pkgKey(src)] = true
		}
	}
	names := make([]string, 0, len(pkgs))
	for p := range pkgs {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []string
	for _, p := range names {
		f := fields[p][sym]
		if len(f) == 0 {
			out = append(out, fmt.Sprintf("%s: not a struct here (interface, alias or constant)", p))
			continue
		}
		out = append(out, fmt.Sprintf("%s: {%s}", p, strings.Join(f, " ")))
	}
	return out
}
