package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The edge report prices every declared edge by what its consumer actually
// selects from its producer, and it is the first report here that looks at
// method calls rather than only at named symbols.
//
// Why that matters is a hole three earlier reports walked into. The parsed
// `used` map records the symbols a file names *through an import*: a type, a
// constant, a function. It records nothing about `e.edges.List(ctx, ...)`,
// because there the package is named once, in a struct field's type, and every
// later use is a selector on a value. A measurement built on `used` alone
// therefore reports a domain that calls four methods as a domain that calls
// none — and it did exactly that, twice in a row, before this file existed
// (decision 235).
//
// The fix is not a better regular expression. A pattern keyed on the field's
// name misses the moment one domain calls it `edges` and another calls it
// `EdgeUC`, which is precisely the pair that produced a confident zero. So the
// method layer here is resolved from the other end: every exported method the
// target domain declares, then every call to that name in the consumer's
// files. The field's name never enters the question.
//
// Two honest limits, both printed in the header rather than left for the reader
// to discover:
//
//   - A call is attributed to the edge when the calling file imports the
//     target domain. That is necessary and not sufficient: `x.List(` can be a
//     call on something else entirely that happens to share a name. Every hit
//     is printed with its file so it can be checked, and the column is labelled
//     an upper bound because it is one.
//   - Only exported methods are considered, since nothing else is reachable
//     across a package boundary. An edge carried entirely by unexported
//     methods cannot exist in Go, so nothing is lost, but an interface method
//     consumed through an embedded type will be attributed to the interface's
//     own package only if that package declares it.
//
// Unlike the regex measurements it replaces, this reads the tree, so a call
// inside a comment is not a call. That is not a nicety: the commented-out
// `// edges, _ := h.edges.List(` in server/webshell is what made a
// grep-based count report webshell as a consumer of edge when it has not been
// one since that line was commented out.

// methodDecl is one exported method declared in a domain.
type methodDecl struct {
	recv string
	pkg  string
	file string
}

// collectMethods indexes every exported method declared in a domain by its
// name, so a call site can be resolved without knowing what the value it is
// called on was named.
func collectMethods(sources []source) map[string]map[string][]methodDecl {
	out := map[string]map[string][]methodDecl{}
	for _, src := range sources {
		if src.test || src.file == nil {
			continue
		}
		d := domainOf(src.path)
		if d == "" {
			continue
		}
		if out[d] == nil {
			out[d] = map[string][]methodDecl{}
		}
		for _, decl := range src.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || !fd.Name.IsExported() {
				continue
			}
			out[d][fd.Name.Name] = append(out[d][fd.Name.Name], methodDecl{
				recv: recvTypeName(fd.Recv.List[0].Type),
				pkg:  pkgKey(src),
				file: src.path,
			})
		}
	}
	return out
}

// recvTypeName renders a receiver's base type name: `*Edge` and `Edge[T]` are
// both `Edge`. The generic parameter list is dropped because a dependent names
// the type without it.
func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return "?"
}

// closureHit is one type a cut drags along behind a type the consumer named.
type closureHit struct {
	// via is the field path that reaches it, e.g. "Report.Kind". A closure
	// printed without its path is a list of names, and a list of names is
	// not something anybody can check against the code.
	via string
	// name is the type as it was written, qualifier included. The qualifier
	// is the whole point: it is what makes a drag across a domain visible
	// instead of looking like another field of the same struct.
	name string
	// home is the domain that declares it, or "" when nothing in the
	// control plane does — a type from a module below the control plane
	// does not move when an edge is cut, so it is not a cost.
	home string
	// alsoIn names the OTHER control-plane domains that declare a type with
	// this same bare name, when there are any.
	//
	// It is printed, not used to decide anything. The walk already resolved
	// the field through its import path, so `home` is not in doubt — the
	// consumer imports one specific package and that package's type is the one
	// the cut carries. What a reader cannot see without this is that the name
	// was ambiguous to begin with, and that matters here: RootCauseJSON,
	// EvidenceItem, TimeWindow and RemediationOption are each declared by BOTH
	// aiops and loop, as two different Go types that happen to serialise under
	// the same JSON tags. A closure count that reads as a plain number cannot
	// tell a reader the number is about one of two, and a pricer that silently
	// picked one would be the same guess decision 233 is a whole section
	// about — so the resolution is package-based and the ambiguity is shown.
	alsoIn []string
}

// builtinTypeNames are the predeclared identifiers. They are excluded from
// every closure walk because a struct field typed `string` is not code
// anybody has to carry to core/domain.
var builtinTypeNames = map[string]bool{
	"bool": true, "string": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true, "byte": true, "rune": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"error": true, "any": true,
}

// fieldRef is one struct field, with the type it was declared as rather than
// the name it was given.
//
// The name is what collectStructFields already indexes, and it is exactly the
// thing that cannot answer this question: the closure is reached through
// `Kind chatruntime.ContainerKind`, and neither half of that line is the
// field's name.
type fieldRef struct {
	field string
	typ   string
	// qual is the package qualifier when the type was written as
	// `pkg.Name`, empty when it was a bare identifier.
	qual string
	// imp is that qualifier RESOLVED to an import path, and empty when the
	// type was written bare.
	//
	// This field exists because of a wrong answer the report printed, not
	// because a struct needed one more column. `typ` is a bare name, and a
	// bare name does not say which package it lives in — so a walk that
	// resolved homes by name alone could not tell a control-plane type from a
	// floor type, and when the control plane happened to contain a same-named
	// type it picked that one. `federation.Member` has two fields whose types
	// are `core/floor/federation.Bundle` and `.Outcome`; the control plane
	// also declares a `Bundle` in aiops' correlate tool and an `Outcome` in
	// aiops' crystallize ledger, so the walk resolved both to aiops and the
	// report printed
	//
	//     closure federation.Bundle  via Member.IssuedBundle  <-- lives in aiops
	//
	// as though cutting the edge would drag an aiops type behind it. It would
	// not: the field is a floor type, cutting the edge does not move it, and
	// the documented rule ("a hit nothing declares is dropped, because it
	// belongs to a module below the control plane") says to drop it. The rule
	// was being applied to a name rather than to a type, and a name that some
	// other domain also declares is never "nothing".
	//
	// The direction of that error matters more than its size. The closure
	// column drives the ranking, and a hit attributed to a third domain is
	// what the report says means "the cut does not stay inside the edge it was
	// priced for" — so this one column was sending the next cut at a
	// federation/aiops seam that does not exist, and inflating the one edge
	// that is cheapest to cut. A pricer that misprices the cheapest edge is
	// worse than a pricer that prices nothing, because the ranking is the only
	// reason to read it.
	imp string
}

// collectFieldTypes indexes every struct field by package, struct and field
// name, keeping the declared type alongside the declared name.
func collectFieldTypes(sources []source) map[string]map[string][]fieldRef {
	out := map[string]map[string][]fieldRef{}
	for _, src := range sources {
		if src.test || src.file == nil {
			continue
		}
		pkg := pkgKey(src)
		if out[pkg] == nil {
			out[pkg] = map[string][]fieldRef{}
		}
		quals := importQualifiers(src.file)
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
				for _, f := range st.Fields.List {
					typ, qual := baseTypeOf(f.Type)
					if typ == "" {
						continue
					}
					for _, id := range f.Names {
						// An unexported field is not part of this edge.
						// A consumer in another package cannot read it, so
						// cutting the edge does not move it, and counting
						// it made `audit.Usecase` look like it dragged
						// nineteen domains behind a four-field struct —
						// every one of them reached through `repo` and
						// `log`. The rest of this file already ignores
						// unexported names for exactly this reason, and a
						// closure that counted them was measuring a
						// different question from the one it is printed
						// under.
						if !id.IsExported() {
							continue
						}
						out[pkg][ts.Name.Name] = append(out[pkg][ts.Name.Name], fieldRef{
							field: id.Name, typ: typ, qual: qual, imp: quals[qual],
						})
					}
				}
			}
		}
	}
	return out
}

// importQualifiers maps the local name of every import in a file to its full
// import path, so a field written `federation.Bundle` can be attributed to the
// package that declares Bundle rather than to whichever domain happens to
// declare a Bundle too.
//
// The two shapes that cannot be resolved are skipped rather than guessed. A
// dot-import puts the file's declarations in this file's namespace, and an
// underscore import deliberately binds nothing, so neither can be attributed
// to a package by the qualifier — and a guess in either direction is the bug
// this function exists to remove. A type reached through one of those two
// forms is not walked, which is the same honest limit the rest of the closure
// walk already has and the report already prints.
func importQualifiers(f *ast.File) map[string]string {
	out := map[string]string{}
	if f == nil {
		return out
	}
	for _, imp := range f.Imports {
		if imp.Name != nil {
			// `.` and `_` bind nothing a qualifier can name.
			if imp.Name.Name == "." || imp.Name.Name == "_" {
				continue
			}
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		local := ""
		if imp.Name != nil {
			local = imp.Name.Name
		} else if i := strings.LastIndex(path, "/"); i >= 0 {
			local = path[i+1:]
		} else {
			local = path
		}
		out[local] = path
	}
	return out
}

// baseTypeOf renders the named type a field's type expression bottoms out in,
// unwrapping the pointers, slices, maps and type parameters that sit between
// the field and the type, and reporting the qualifier when the type was
// written as `pkg.Name`.
//
// It returns "" for a type expression with no name in it — an inline struct,
// an inline interface, a function type — because there is nothing to carry.
// That is also the honest limit of this walk and the report prints it: a type
// reached only through an inline composite literal is invisible here.
func baseTypeOf(expr ast.Expr) (typ, qual string) {
	switch t := expr.(type) {
	case *ast.Ident:
		if builtinTypeNames[t.Name] {
			return "", ""
		}
		return t.Name, ""
	case *ast.StarExpr:
		return baseTypeOf(t.X)
	case *ast.ArrayType:
		return baseTypeOf(t.Elt)
	case *ast.MapType:
		// Both halves, because map[V]Key and map[Key]V are both legal and
		// which one names the interesting type is not something this
		// function can know. Deduplication happens in the walk.
		if typ, qual = baseTypeOf(t.Key); typ != "" {
			return typ, qual
		}
		return baseTypeOf(t.Value)
	case *ast.SelectorExpr:
		name := t.Sel.Name
		if id, ok := t.X.(*ast.Ident); ok {
			return name, id.Name
		}
		return name, ""
	case *ast.IndexExpr:
		return baseTypeOf(t.X)
	case *ast.IndexListExpr:
		return baseTypeOf(t.X)
	case *ast.ChanType:
		return baseTypeOf(t.Value)
	case *ast.ParenExpr:
		return baseTypeOf(t.X)
	}
	return "", ""
}

func dedupeStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// home reports which bounded context declares the type this field refers to,
// and whether it is one of them at all.
//
// The second answer is the one the price depends on, and it is why this is a
// method on the reference rather than a lookup in a name table: "is this a
// control-plane type" and "does some domain declare a type with this name" are
// different questions, and only the first one is about the code. domainOf
// returns "" for an import path outside the control plane — the floor, the
// edge agent, a third-party module — and those are exactly the types a cut
// does not move.
//
// For an unqualified reference imp is the declaring package itself, which the
// fold in closureOf fills in, so this returns the producer's own domain and
// the walk continues.
func (r fieldRef) home() (domain string, inControlPlane bool) {
	if r.imp == "" {
		// Not resolved and not folded: there is no package to attribute it
		// to, so it is dropped rather than assumed. Reachable only through a
		// dot-import, which importQualifiers deliberately does not resolve.
		return "", false
	}
	d := domainOf(r.imp)
	if d == "" {
		return "", false
	}
	return d, true
}

// nameOwners indexes, for every declared name in the control plane, the
// domains that declare it.
//
// It exists to say "that name is ambiguous", and for nothing else. It was
// once the thing that DECIDED a type's home, which is how a floor type came
// back attributed to a domain that merely declared something of the same name
// — see fieldRef.imp for that one. Deciding by name is not available any more
// and cannot be made available: a bare name does not say which package it came
// from, so any resolution built on one is a guess.
//
// A name with two owners is therefore not resolved here either. It is
// reported, and the report says which package the walk actually followed.
func nameOwners(kind map[string]map[string]declKind) map[string][]string {
	out := map[string][]string{}
	for pkg, names := range kind {
		d := domainOf(pkg)
		if d == "" {
			continue
		}
		for name := range names {
			out[name] = append(out[name], d)
		}
	}
	for name, ds := range out {
		sort.Strings(ds)
		out[name] = dedupeStrings(ds)
	}
	return out
}

// otherOwners is the domains declaring this name apart from the one the walk
// resolved to, which is empty in the ordinary case.
//
// The point of returning it is that a name owned by two domains is a fact
// about the tree, not about the walk: the walk did the right thing by
// following the import, and printing nothing would leave a reader believing
// the number was about a name with one owner. RootCauseJSON and its three
// siblings are declared by both aiops and loop, as two different Go types
// under the same JSON tags, so this column is where that becomes visible
// instead of being something a maintainer finds out from a failed
// deserialisation.
func otherOwners(all []string, resolved string) []string {
	var out []string
	for _, d := range all {
		if d != resolved {
			out = append(out, d)
		}
	}
	return out
}

// closureOf walks from the types a consumer names into the types those types
// reach through their fields, and returns everything a cut would have to
// carry along with them.
//
// Three rules, each of which is a decision rather than a default:
//
//   - Only fields are followed. A type reached solely through a method
//     signature is not counted, and the report says so, because a method
//     signature is a different shape of dependency from a field and mixing
//     them would make the column mean two things.
//   - A hit whose home is a *different* domain is recorded and not walked
//     further. That is the drag: it means cutting this edge does not stay
//     inside this edge.
//   - A hit that is not a control-plane type is dropped, and "is not" is
//     decided by the field's RESOLVED import path rather than by its name.
//     It is a type from a module below the control plane or from a
//     third-party module, and cutting an edge does not move it, so counting
//     it would inflate the price with something nobody has to carry. The
//     name-based version of this rule was wrong in one direction only, and
//     wrong it in the direction that matters: see fieldRef.imp.
func closureOf(selected []string, to string, ftypes map[string]map[string][]fieldRef, owners map[string][]string) []closureHit {
	// The producer's own fields, folded across its packages: a domain is
	// reached through one package or several and the closure is the same
	// walk either way.
	prod := map[string][]fieldRef{}
	for pkg, byType := range ftypes {
		if domainOf(pkg) != to {
			continue
		}
		for name, refs := range byType {
			for _, r := range refs {
				// An unqualified field type is declared in the same package
				// as the struct that holds it, and folding the producer's
				// packages together would otherwise lose that — which is the
				// one case where "same domain" is not the same answer as
				// "this package", so it has to be recovered here rather than
				// guessed below.
				if r.imp == "" {
					r.imp = pkg
				}
				prod[name] = append(prod[name], r)
			}
		}
	}
	seen := map[string]bool{}
	for _, s := range selected {
		seen[s] = true
	}
	var out []closureHit
	var walk func(name, path string, depth int)
	walk = func(name, path string, depth int) {
		// Bounded because a struct cannot contain itself by value, but two
		// types can name each other through a pointer and an unbounded
		// walk over that is a hang rather than an answer.
		if depth > 8 {
			return
		}
		for _, ref := range prod[name] {
			// The identity of a reached type is its package plus its name, and
			// the package is what decides everything below. Keying `seen` on
			// the bare name is the same collision this function used to
			// misattribute homes by, and it would silently skip a control-plane
			// type because a floor type of the same name was reached first.
			key := ref.imp + "." + ref.typ
			if seen[key] {
				continue
			}
			seen[key] = true

			home, inControlPlane := ref.home()
			if !inControlPlane {
				// Not a control-plane type at all — a floor type, a shared
				// module, or a third party. Cutting an edge does not move it,
				// so counting it would inflate the price with something
				// nobody has to carry. This is the rule that used to be
				// applied to a name, which meant a floor type escaped it
				// whenever the control plane declared something of the same
				// name. See fieldRef.imp.
				continue
			}
			disp := ref.typ
			if ref.qual != "" {
				disp = ref.qual + "." + ref.typ
			}
			via := path + "." + ref.field
			out = append(out, closureHit{
				via: via, name: disp, home: home,
				alsoIn: otherOwners(owners[ref.typ], home),
			})
			if home == to {
				walk(ref.typ, via, depth+1)
			}
		}
	}
	for _, s := range selected {
		walk(s, s, 0)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].via != out[j].via {
			return out[i].via < out[j].via
		}
		return out[i].name < out[j].name
	})
	return out
}

// edgeCost is one declared edge, priced.
type edgeCost struct {
	from, to string
	// reason is the declared justification. It is carried because an edge
	// that selects nothing and has a reason reads as a stale declaration,
	// while an edge that selects nothing and has no reason reads as a
	// measurement bug — and telling those two apart is the reader's job,
	// not the report's.
	reason string
	// types are the symbols the consumer names through an import of the
	// producer's domain, with the interface flag resolved.
	types []string
	// ifc counts how many of those are interfaces, because a consumer
	// already holding an interface is holding something that could be
	// re-pointed without moving anything.
	ifc int
	// methods are the producer's exported method names called from the
	// consumer's files, with the file each was seen in.
	methods []string
	// methodSites maps a method name to the consumer file it was seen in.
	methodSites map[string]string
	// closure are the types a cut has to carry even though the consumer
	// never names them, because it reaches them through a field of one it
	// does name. See closureOf for why the price below is not the number of
	// symbols the consumer wrote.
	closure []closureHit
}

func (e edgeCost) symbols() int { return len(e.types) + len(e.methods) }

// price is what cutting this edge actually costs: the symbols the consumer
// names, plus the ones it would have to carry behind them.
//
// It is the sort key rather than symbols() because symbols() is the number
// that decided the marketplace -> pluginimport edge was "two symbols" when
// moving it means moving six, two of them out of a third domain. A ranking
// built on the direct count sends the next cut at the wrong edge, which is
// the one thing this report exists to prevent.
func (e edgeCost) price() int { return e.symbols() + len(e.closure) }

// foreignDomains lists the domains a cut reaches into besides the producer's,
// sorted and deduplicated.
//
// Every entry here is a domain the cut drags a type into, and each one used to
// be printed with the two halves of the same name joined by a pipe — the
// ambiguity was standing in for an answer instead of being reported next to
// one. Now the walk resolves the field through its import path, so a hit has
// exactly one home, and a name with a second owner is carried on the hit
// itself as alsoIn. The list is therefore the set of domains this cut really
// reaches, which is the number that says whether a cut stays inside its edge.
//
// The comparison is membership and not string inequality. `Decision` is
// declared in pluginimport and in three other domains, so printing it as
// "declared elsewhere" would be false in the direction that matters most — it
// would tell a reader the type has to come from somewhere it already is.
func (e edgeCost) foreignDomains() []string {
	var out []string
	for _, h := range e.closure {
		if e.ownsType(h.home) {
			continue
		}
		out = append(out, h.home)
	}
	sort.Strings(out)
	return dedupeStrings(out)
}

// ownsType reports whether a reached type is the producer's own.
//
// It used to split `home` on "|" because a name could resolve to several
// domains at once, and a split that had to be read as "owned if any of them
// is mine" is exactly the ambiguity this file stopped having: the walk now
// follows one import path, so `home` is one domain and this is an equality.
func (e edgeCost) ownsType(home string) bool {
	return home == e.to
}

// printEdges writes every declared edge with what the consumer selects from the
// producer, cheapest first. It is a report, not a gate: the ordering is the
// deliverable, and the question it exists to answer is "which of the remaining
// edges is small enough to cut today".
func printEdges(w io.Writer, sources []source, r rules) {
	methods := collectMethods(sources)
	// The field lists and the interface flags are tree-wide facts, so they
	// are read once. Recomputing them per edge would walk every file once
	// per declared edge, which for forty edges is a minute of work to
	// print a table nobody reads twice.
	fields := collectStructFields(sources)
	kind := collectDeclKinds(sources)
	ftypes := collectFieldTypes(sources)
	owners := nameOwners(kind)

	var costs []edgeCost
	for e, reason := range r.edges {
		if r.shared[e.to] != "" {
			continue
		}
		c := edgeCost{from: e.from, to: e.to, reason: reason, methodSites: map[string]string{}}
		seenType := map[string]bool{}
		for _, src := range sources {
			if src.test {
				continue
			}
			if domainOf(src.path) != e.from {
				continue
			}
			for imp, syms := range src.used {
				if domainOf(imp) != e.to {
					continue
				}
				for sym := range syms {
					if seenType[sym] {
						continue
					}
					seenType[sym] = true
					c.types = append(c.types, sym)
					if kind[imp] != nil && kind[imp][sym] == kindInterface {
						c.ifc++
					}
				}
			}
			// The method layer. Restricted to files that import the
			// producer, because a call on an unrelated value that happens
			// to share a method name is not this edge.
			imports := false
			for _, imp := range src.imports {
				if domainOf(imp) == e.to {
					imports = true
					break
				}
			}
			if !imports {
				continue
			}
			ast.Inspect(src.file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if _, ok := methods[e.to][sel.Sel.Name]; !ok {
					return true
				}
				if _, seen := c.methodSites[sel.Sel.Name]; !seen {
					c.methods = append(c.methods, sel.Sel.Name)
					c.methodSites[sel.Sel.Name] = src.path
				}
				return true
			})
		}
		sort.Strings(c.types)
		sort.Strings(c.methods)
		// The closure is computed after the walk rather than inside it,
		// because it starts from the *set* of selected types: a type two
		// selected structs both reach is one piece of work, not two, and
		// per-file computation would count it once per file that names it.
		c.closure = closureOf(c.types, e.to, ftypes, owners)
		costs = append(costs, c)
	}
	sort.Slice(costs, func(i, j int) bool {
		if costs[i].price() != costs[j].price() {
			return costs[i].price() < costs[j].price()
		}
		if costs[i].from != costs[j].from {
			return costs[i].from < costs[j].from
		}
		return costs[i].to < costs[j].to
	})

	fmt.Fprintln(w, "declared edges, priced by what cutting one would actually take")
	fmt.Fprintln(w, "  cheapest first: an edge that carries one or two types is a candidate")
	fmt.Fprintln(w, "  for a port, and an edge that carries a wide struct plus a spread of")
	fmt.Fprintln(w, "  methods is a relocation, not a port.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  cols: price = named types + called methods + closure; ifc = how many of")
	fmt.Fprintln(w, "  the named types are interfaces already; * = the producer declares more")
	fmt.Fprintln(w, "  than one package with that name.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  the closure is the part the consumer never writes down. Naming a type")
	fmt.Fprintln(w, "  whose field names another type means moving both, so price counts the")
	fmt.Fprintln(w, "  second one too. Counting only what was named is how an edge carrying")
	fmt.Fprintln(w, "  six types was reported as carrying two, and the ranking that follows")
	fmt.Fprintln(w, "  from that number sends the next cut at the wrong edge.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  methods are resolved from the producer's exported method set, not")
	fmt.Fprintln(w, "  from the field name, so a renamed field cannot hide a call. They are")
	fmt.Fprintln(w, "  attributed to files that import the producer, which makes the column an")
	fmt.Fprintln(w, "  UPPER BOUND: a same-named call on an unrelated value would be counted.")
	fmt.Fprintln(w, "  Each hit names its file so it can be checked.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  three limits of the closure, all of them lower bounds:")
	fmt.Fprintln(w, "    - it follows struct FIELDS only, so a type reachable only through a")
	fmt.Fprintln(w, "      method signature or an inline composite literal is not counted")
	fmt.Fprintln(w, "    - a hit declared by another domain is recorded and not walked into,")
	fmt.Fprintln(w, "      which is why a closure line naming a third domain means the cut")
	fmt.Fprintln(w, "      does not stay inside the edge it was priced for")
	fmt.Fprintln(w, "    - a type no domain declares is dropped, because it belongs to a module")
	fmt.Fprintln(w, "      below the control plane and cutting an edge does not move it")
	fmt.Fprintln(w, "    - only EXPORTED fields are followed, while the shape column below lists")
	fmt.Fprintln(w, "      every field, so an unexported one appears in a shape and never in a")
	fmt.Fprintln(w, "      price. That is not an inconsistency: a dependent in another package")
	fmt.Fprintln(w, "      cannot read an unexported field, so cutting the edge keeps it.")
	fmt.Fprintln(w)
	if len(costs) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	typeOnly, methodOnly, both, dragged := 0, 0, 0, 0
	for _, c := range costs {
		// The breakdown prints the direct count next to the price, because
		// the two disagreeing is the finding. A reader who sees only the
		// price cannot tell a two-symbol edge from a two-symbol edge that
		// drags four more, and those two want opposite decisions.
		extra := ""
		if n := len(c.closure); n > 0 {
			extra = fmt.Sprintf(" + %d closure", n)
			if homes := c.foreignDomains(); len(homes) > 0 {
				dragged++
				extra += fmt.Sprintf(" (reaches %s)", strings.Join(homes, " "))
			}
		}
		switch {
		case len(c.types) == 0 && len(c.methods) == 0:
			// Declared but nothing selected: the edge is drawn by a
			// reason in the table, not by code. It is printed because a
			// declared edge nothing uses is a different problem from an
			// expensive one.
			fmt.Fprintf(w, "  %2d  %-18s -> %-16s  (declared, nothing selected: %s)\n", c.price(), c.from, c.to, c.reason)
		case len(c.types) == 0:
			methodOnly++
			fmt.Fprintf(w, "  %2d  %-18s -> %-16s  %d method(s), no type%s\n", c.price(), c.from, c.to, len(c.methods), extra)
		case len(c.methods) == 0:
			typeOnly++
			fmt.Fprintf(w, "  %2d  %-18s -> %-16s  %d type(s), no method%s\n", c.price(), c.from, c.to, len(c.types), extra)
		default:
			both++
			fmt.Fprintf(w, "  %2d  %-18s -> %-16s  %d type(s) (%d ifc) + %d method(s)%s\n",
				c.price(), c.from, c.to, len(c.types), c.ifc, len(c.methods), extra)
		}
		for _, t := range c.types {
			mark := ""
			if shape := shapeOf(t, c.to, fields, kind); shape != "" {
				mark = "  " + shape
			}
			if dup := dupOwners(t, c.to, kind); len(dup) > 1 {
				mark += fmt.Sprintf("  <-- also declared in %s", strings.Join(dup, " "))
			}
			fmt.Fprintf(w, "        type   %-24s%s\n", t, mark)
		}
		for _, h := range c.closure {
			mark := ""
			switch {
			case !c.ownsType(h.home):
				// The cross-domain case, named rather than summarised:
				// this is the line that says the cut is bigger than the
				// edge it was priced under.
				mark = fmt.Sprintf("  <-- lives in %s, not in %s", h.home, c.to)
			case len(h.alsoIn) > 0:
				// The producer owns this one, so the cut stays inside its
				// edge — but another domain declares a type of the same
				// name, and the two are not the same type. Printed because
				// the price counts one shape and a reader has no other way
				// to learn there are two.
				mark = fmt.Sprintf("  <-- %s also declares this name; the shape above is %s's",
					strings.Join(h.alsoIn, " "), h.home)
			}
			fmt.Fprintf(w, "        closure %-24s via %s%s\n", h.name, h.via, mark)
		}
		for _, m := range c.methods {
			recv := "?"
			if d, ok := methods[c.to][m]; ok && len(d) > 0 {
				recv = d[0].recv
			}
			fmt.Fprintf(w, "        method %-24s on %-14s %s\n", m, recv, c.methodSites[m])
		}
	}
	fmt.Fprintf(w, "\n  %d edges: %d carry types only, %d carry methods only, %d carry both\n",
		len(costs), typeOnly, methodOnly, both)
	if dragged > 0 {
		// Counted on purpose rather than left to the reader: "an edge that
		// reaches into a third domain" is a different kind of work from one
		// that does not, and the count is what tells a planner how many
		// there are.
		fmt.Fprintf(w, "  %d of them drag a type out of a domain other than the producer's,\n", dragged)
		fmt.Fprintf(w, "  so cutting those is not the single-edge job the price column implies.\n")
	}
}

// shapeOf renders a selected struct's field list, or says why it cannot.
func shapeOf(sym, to string, fields map[string]map[string][]string, kind map[string]map[string]declKind) string {
	pkgs := packagesOf(to, fields)
	for _, p := range pkgs {
		if f := fields[p][sym]; len(f) > 0 {
			return fmt.Sprintf("{%s}", strings.Join(f, " "))
		}
		if kind[p] != nil {
			if _, ok := kind[p][sym]; ok {
				return "not a struct (interface, alias or constant)"
			}
		}
	}
	return ""
}

// dupOwners lists the producer packages that declare a symbol, when there is
// more than one. A name with two owners is the case decision 233 wrote a whole
// section about, so the report says so instead of printing one of them.
func dupOwners(sym, to string, kind map[string]map[string]declKind) []string {
	var out []string
	for _, p := range packagesOf(to, kind) {
		if kind[p] != nil {
			if _, ok := kind[p][sym]; ok {
				out = append(out, strings.TrimPrefix(p, managerPrefix+"/"))
			}
		}
	}
	sort.Strings(out)
	return out
}

// packagesOf lists the import paths belonging to a domain. pkgKey already
// returns an import path in the same namespace src.path uses, so the domain of
// a package key is read with the same function every other path goes through
// rather than by re-deriving it from the prefix.
func packagesOf[V any](to string, keys map[string]map[string]V) []string {
	var out []string
	for p := range keys {
		if domainOf(p) == to {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// collectStructFields indexes every struct's field list by package and name, so
// a selected type can be printed with the shape a mover would have to carry
// instead of just its name.
func collectStructFields(sources []source) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, src := range sources {
		if src.test || src.file == nil {
			continue
		}
		pkg := pkgKey(src)
		if out[pkg] == nil {
			out[pkg] = map[string][]string{}
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
				out[pkg][ts.Name.Name] = names
			}
		}
	}
	return out
}

// collectDeclKinds folds the per-file declared maps into per-package ones, so
// a name declared in a sibling file of the same package is still found.
func collectDeclKinds(sources []source) map[string]map[string]declKind {
	out := map[string]map[string]declKind{}
	for _, src := range sources {
		if src.declared == nil {
			continue
		}
		p := pkgKey(src)
		if out[p] == nil {
			out[p] = map[string]declKind{}
		}
		for name, k := range src.declared {
			out[p][name] = k
		}
	}
	return out
}
