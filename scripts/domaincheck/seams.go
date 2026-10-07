package main

import (
	"fmt"
	"go/ast"
	"io"
	"sort"
	"strings"
)

// The seam report exists because of a classification error, not because
// something was missing.
//
// Decision 228 sorted the remaining cross-domain edges by "what kind of symbol
// does the consumer select" and concluded that everything left was a data shape
// that had to be moved, GORM entities included. Decision 230 then cut
// `flow -> scheduler` — a single interface, implemented in the *consuming*
// domain, whose only weight was one `var _ schedulerbiz.Repo = (*Repo)(nil)`.
// The list had filed it under "data shape, expensive" because the table it was
// built from could not tell an `interface` from a `struct` by name alone.
//
// So this file does what the hand table could not: it reads each edge's actual
// selectors out of the parsed tree and, for the interfaces among them, finds
// which domain holds a compile-time assertion of implementation. The
// discriminator is not "is it cheap to move" — that is a judgement — but "is
// the thing on the other side of this edge a contract or a value", which is a
// fact about the source and can be checked.
//
// The three shapes it reports:
//
//	port-opposite — the consumer selects only interfaces, AND the implementor
//	                is in the consumer's own domain. The port is named on the
//	                far side of the boundary from its only implementation,
//	                which is a naming bug: moving the interface to a shared
//	                floor lets both halves see it and the edge disappears.
//	                Decisions 227 and 230 are both this.
//	port-here     — the consumer selects only interfaces and the implementor is
//	                in the declaring domain. That is an ordinary substitutable
//	                boundary and cutting it would be a real design change.
//	mixed / data  — anything else. A struct on the far side has to move with
//	                its table, its foreign keys, and every other domain that
//	                already stores one. This is the expensive bucket.
type seamRow struct {
	from, to string
	// ifaces and others are the selected symbols, split by kind.
	ifaces, others []string
	// implementor is the domain holding a `var _ pkg.Iface = ...` assertion
	// for one of ifaces, when one was found.
	implementor string
	// weight is the number of import statements behind the edge.
	weight int
	// refs counts how many times the consumer actually *uses* each selected
	// symbol, summed across its production files, and files counts how many
	// of those files there are.
	//
	// The two numbers together are the only ones here that respond to "how
	// deep does this consumer go". Everything above answers what kind of
	// thing crosses; this answers how much of it is being read. A consumer
	// that names seven symbols and reads one field each is not in the same
	// position as one that names seven and walks all of them, and the
	// existing counters cannot tell those apart — see decision 254, where
	// two consumers of one package had identical symbol sets and wildly
	// different cuts.
	refs  map[string]int
	files int
	// bulk is the measured shape of the struct types this edge's consumer
	// selects, keyed by symbol. Only struct types appear; a selected
	// function or constant has no shape to project.
	bulk map[string]structBulk
}

// depth is the total number of symbol uses behind the edge. It is reported
// beside the weight rather than instead of it, because the two disagree in
// the direction that matters: weight counts statements, this counts work, and
// an edge can be one statement deep into a struct or forty references into a
// tree.
func (r *seamRow) depth() int {
	n := 0
	for _, c := range r.refs {
		n += c
	}
	return n
}

func (r seamRow) verdict() string {
	if len(r.ifaces) == 0 {
		return "data"
	}
	if len(r.others) > 0 {
		return "mixed"
	}
	if r.implementor == "" {
		return "port-here?"
	}
	if r.implementor == r.from {
		return "port-opposite"
	}
	return "port-here"
}

// printSeams writes the per-edge classification. It is a report, not a gate:
// the tree is allowed to contain any of these verdicts, and the point is to
// show which edges are worth a knife before anyone spends one.
func printSeams(w io.Writer, sources []source, r rules) {
	declared := map[string]map[string]declKind{}
	// bulk is keyed by import path for the same reason declared is: a
	// consumer selects types declared in the *producer's* files, so the
	// shapes have to be collected across the producer's package and then
	// looked up by the path this consumer imported, not read off the
	// consumer's own source.
	bulkByPkg := map[string]map[string]structBulk{}
	for _, src := range sources {
		if src.declared != nil {
			declared[pkgKey(src)] = src.declared
		}
		if len(src.bulk) > 0 {
			key := pkgKey(src)
			if bulkByPkg[key] == nil {
				bulkByPkg[key] = map[string]structBulk{}
			}
			for sym, b := range src.bulk {
				if cur, seen := bulkByPkg[key][sym]; !seen || b.fields > cur.fields {
					bulkByPkg[key][sym] = b
				}
			}
		}
	}

	// assertions[importPath][symbol] = the domain that asserts it implements
	// the interface. Only `var _ pkg.Iface = ...` counts: that is the form
	// this tree uses everywhere, and it is the only one that is checked by
	// the compiler, so it is the only one that can be read as fact rather
	// than as a guess about intent.
	assertions := map[string]map[string]string{}
	for _, src := range sources {
		if src.test || src.file == nil {
			continue
		}
		alias := importAliases(src.file)
		ast.Inspect(src.file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				return true
			}
			if vs.Names[0].Name != "_" {
				return true
			}
			sel, ok := vs.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgID, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			path, ok := alias[pkgID.Name]
			if !ok {
				return true
			}
			if assertions[path] == nil {
				assertions[path] = map[string]string{}
			}
			assertions[path][sel.Sel.Name] = domainOf(src.path)
			return true
		})
	}

	rows := map[edge]*seamRow{}
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
			e := edge{from, to}
			row := rows[e]
			if row == nil {
				row = &seamRow{from: from, to: to}
				rows[e] = row
			}
			row.weight++
			row.files++
			// Only the symbols this consumer actually selected. Reporting the
			// producer package's whole exported surface would be a fact about
			// the package, not about the edge, and it buries the one number
			// that matters — the shape of the thing being carried.
			for sym := range syms {
				b, ok := bulkByPkg[imp][sym]
				if !ok {
					continue
				}
				if row.bulk == nil {
					row.bulk = map[string]structBulk{}
				}
				// Widest wins when a symbol is declared in several files of
				// the same package, which is the only way two declarations
				// can exist for one name and still compile.
				if cur, seen := row.bulk[sym]; !seen || b.fields > cur.fields {
					row.bulk[sym] = b
				}
			}
			for sym, uses := range src.refs[imp] {
				if row.refs == nil {
					row.refs = map[string]int{}
				}
				row.refs[sym] += uses
			}
			for sym := range syms {
				if declared[imp][sym] == kindInterface {
					row.ifaces = append(row.ifaces, sym)
					if who := assertions[imp][sym]; who != "" && row.implementor == "" {
						row.implementor = who
					}
				} else {
					row.others = append(row.others, sym)
				}
			}
		}
	}

	// The import statement count is on the graph, not on `used` — a file that
	// imports a package without selecting from it is still a dependency the
	// build has to honour. Reuse the graph's own weight so the two reports
	// can never disagree.
	g := buildGraph(sources, r)
	for e := range g.weight {
		if row := rows[e]; row != nil {
			row.weight = g.weight[e]
		}
	}

	keys := make([]edge, 0, len(rows))
	for e := range rows {
		keys = append(keys, e)
	}
	sort.Slice(keys, func(i, j int) bool {
		if rows[keys[i]].verdict() != rows[keys[j]].verdict() {
			return rows[keys[i]].verdict() < rows[keys[j]].verdict()
		}
		if rows[keys[i]].weight != rows[keys[j]].weight {
			return rows[keys[i]].weight > rows[keys[j]].weight
		}
		return keys[i].to < keys[j].to
	})

	fmt.Fprintln(w, "seams: what each cross-domain edge actually carries")
	fmt.Fprintln(w, "  port-opposite  the consumer selects only interfaces and holds the")
	fmt.Fprintln(w, "                implementation itself — the port is named on the far")
	fmt.Fprintln(w, "                side of the boundary from its only implementor. Moving")
	fmt.Fprintln(w, "                it to a shared floor removes the edge (decisions 227, 230).")
	fmt.Fprintln(w, "  port-here     interfaces only, implemented where they are declared. An")
	fmt.Fprintln(w, "                ordinary substitutable boundary; cutting it is a design change.")
	fmt.Fprintln(w, "  mixed         interfaces alongside values. The values have to move too.")
	fmt.Fprintln(w, "  data          no interface at all. Structs, entities, tables.")
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Two more axes, and neither is the one the weight is. Everything above says")
	fmt.Fprintln(w, "  what kind of thing crosses; the next two say how much of it is being read.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  refs  counts uses of each selected symbol, and cannot see inside a value.")
	fmt.Fprintln(w, "       A low number here is not cheap: decision 254 found a consumer that")
	fmt.Fprintln(w, "       selected seven symbols, used them seven times, and walked a parsed")
	fmt.Fprintln(w, "       object graph through field selections no package-qualified name")
	fmt.Fprintln(w, "       ever mentions. Read refs and shape together.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  shape is the measured size of the struct types on the edge: own fields,")
	fmt.Fprintln(w, "       how many of those are containers (slice/array/map/pointer) and how")
	fmt.Fprintln(w, "       many are nested structs. This is the number that separates a")
	fmt.Fprintln(w, "       projection of four scalars from a projection of a tree, and it is")
	fmt.Fprintln(w, "       what a projection has to be sized against.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Both are report-only and neither is a gate. refs counts occurrences, not")
	fmt.Fprintln(w, "  work — forty uses in one loop is not forty uses in straight-line code —")
	fmt.Fprintln(w, "  and shape cannot resolve a qualified field type, so a time.Time is not")
	fmt.Fprintln(w, "  counted as a container. A gauge that is wrong in a known direction is")
	fmt.Fprintln(w, "  worth printing; a gate built on it would train its reader to skip it.")
	fmt.Fprintln(w)
	for _, e := range keys {
		row := rows[e]
		fmt.Fprintf(w, "  %-13s %3d  %-14s -> %-14s", row.verdict(), row.weight, row.from, row.to)
		if row.implementor != "" {
			fmt.Fprintf(w, " impl-in=%s", row.implementor)
		}
		fmt.Fprintln(w)
		if len(row.ifaces) > 0 {
			fmt.Fprintf(w, "                 iface: %s\n", strings.Join(dedupe(row.ifaces), ", "))
		}
		if len(row.others) > 0 {
			fmt.Fprintf(w, "                 value: %s\n", strings.Join(dedupe(sortedCopy(row.others)), ", "))
		}
		if len(row.refs) > 0 {
			fmt.Fprintf(w, "                 refs:  %d uses in %d file(s) — %s\n",
				row.depth(), row.files, describeRefs(row.refs))
		}
		if len(row.bulk) > 0 {
			fmt.Fprintf(w, "                 shape: %s\n", describeBulk(row.bulk))
		}
	}
}

// importAliases maps the identifier a file refers to an imported package by
// to the import path. An import with no explicit name is referred to by the
// last path segment, which is what the compiler does and therefore what the
// assertions in this tree use.
func importAliases(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		if name == "" {
			parts := strings.Split(path, "/")
			name = parts[len(parts)-1]
		}
		out[name] = path
	}
	return out
}

// describeRefs renders the per-symbol use counts, hottest first, so the
// symbol that dominates the edge is the first thing a reader sees. Ties are
// broken by name because a report that reorders itself between runs cannot be
// diffed.
func describeRefs(refs map[string]int) string {
	type kv struct {
		sym string
		n   int
	}
	pairs := make([]kv, 0, len(refs))
	for s, n := range refs {
		pairs = append(pairs, kv{s, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].sym < pairs[j].sym
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s×%d", p.sym, p.n))
	}
	return strings.Join(parts, ", ")
}

// describeBulk renders the fat types on an edge first, because a wide struct
// with containers is what makes a narrow-looking edge expensive. Ties break on
// the name so two runs diff cleanly.
func describeBulk(bulk map[string]structBulk) string {
	type kv struct {
		sym string
		b   structBulk
	}
	pairs := make([]kv, 0, len(bulk))
	for s, b := range bulk {
		pairs = append(pairs, kv{s, b})
	}
	// A weight rather than a plain field sort: containers and nested structs
	// are what a projection cannot drop, so they outrank raw width.
	weight := func(b structBulk) int { return b.fields + 2*b.containers + 2*b.nested }
	sort.Slice(pairs, func(i, j int) bool {
		if weight(pairs[i].b) != weight(pairs[j].b) {
			return weight(pairs[i].b) > weight(pairs[j].b)
		}
		return pairs[i].sym < pairs[j].sym
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s(%df %dc %dn)", p.sym, p.b.fields, p.b.containers, p.b.nested))
	}
	return strings.Join(parts, ", ")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
