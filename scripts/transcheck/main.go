// Command transcheck reports hand-written struct translations and the columns
// they leave behind.
//
// The ledger has named this measurement as the one it cannot take. A
// translation is a composite literal of a type from another package whose
// fields are copied one by one from a value of a struct here: `return
// svc.RuleInput{ RuleKey: in.RuleKey, ... }`. It compiles, it looks reviewed,
// and the day somebody adds a column to the source struct the translation
// keeps compiling and ships the new column as a zero value. The shape that
// makes it invisible is that the omission is not an error anywhere — nothing
// in Go says a struct literal must mention every field, and nothing in this
// repository's gates could see it until decision 259 wrote the guard by hand
// for one call site.
//
// So this is the generalisation of that guard: find the sites, resolve the
// source struct where the receiver is nameable, and print the columns the
// literal does not set. One site is already guarded by hand
// (cmd/opskeeper/alert_rule_wiring.go); the point of this command is that the
// other 120-odd are not guarded at all.
//
// It reports and exits 0, and it is deliberately not a gate. The first two
// sites it resolved by hand were both false positives, and both for a reason
// that is a property of the code rather than of the analysis:
//
//   - a value passed as a separate argument, not as a field. IssueAgentTeamsToken
//     does not copy TTLSeconds into the claims, and must not: the signer takes
//     the TTL as its second parameter. A name-based reading calls that a
//     dropped column.
//   - a renamed column. hitl serialises Payload into PayloadJSON, so the
//     literal sets a field whose name differs from the source's. Same verdict,
//     wrong reason.
//
// Two more classes are structural rather than accidental, and they account for
// most of the rest of what the first run flagged:
//
//   - narrowing. A literal built inside a type switch copies the fields of the
//     branch's own case out of a wider struct, so the other cases' fields are
//     "unset" by construction. Three such sites in service/aiops alone.
//   - flattening. A nested source struct is spread across several flat
//     destination columns — autonomyAuditRow turns Row.Trigger into the wire's
//     Kind/Metric/Threshold triple, and the reason is written in the function's
//     own comment: a nested struct on the wire would be a second place for the
//     trigger vocabulary to drift.
//
// Reading all fourteen flagged sites settled the rest, and the tally is the
// argument for keeping this a report: twelve are structural, two were real.
// The six further classes are all in the list below — a derived identity
// column, an envelope projection, a closure capture, a fan-out across sibling
// constructors, an assignment after the literal, and a rename that is only a
// rename because the source column is spelled differently.
//
// All ten are printed on every run, so a reader meets them before the numbers
// rather than after. A gate that reports two of two wrong teaches people to
// write "trust me" next to it, and the ledger has a rule about exactly that.
// The classes are also what an extension has to handle: each one is a shape
// the analysis can learn to see, and until it does, every flag is a place to
// look rather than a defect to fix.
//
// The second version of this command reads both ends, and the reason is in
// the numbers: every real defect found on this tree was in the direction the
// first version did not measure. os_version and disk_total_bytes were zero on
// every device row because the WIRE struct had no column to carry them;
// cache_write_tokens was missing from the usage frame because the frame had
// no column for it; and a reconnected console's approval card came back with
// no arguments, no blast radius and no target because Open rebuilt five
// columns and never decoded the payload the row had been storing all along.
// None of those is a source column left unset — they are destination columns
// with nothing to fill them, which is a hole in a contract rather than an
// omission in a copy.
//
// The destination direction needs no receiver, so it resolves where the
// source direction cannot: 124 of 128 sites against 27. Its own false
// positives are mostly the classes above recurring — a create statement for a
// persisted row is not a translation, it is the first half of a two-step
// write — and one of them, the node-side gate that never fills its own
// Summary and Target, is a real defect this direction has found and that the
// next decision has to decide rather than guess.
//
// What it cannot see, in full:
//
//  1. Renames and derivations, as above. The receiver's column appears under
//     another name, or is serialised, or is computed in a helper call.
//  2. Values that travel as extra parameters to the enclosing call rather
//     than as fields of the literal.
//  3. The receiver is usually not a parameter. It is a local, a struct field,
//     or the result of another call, and resolving those needs go/types and a
//     per-module load. The resolution rate is printed: on this tree it is a
//     small fraction of the sites, and the sites it misses are NOT reported
//     as clean — they are absent, which is the opposite of a clean bill.
//  4. Only direct struct literals. A translation spread over a builder, or
//     assembled field by field across statements, is invisible.
//  5. A struct literal that sets every field by hand still looks like a
//     translation when the values happen to be selectors, which is why the
//     ratio and the minimum width are thresholds and not definitions.
//
// Usage:
//
//	go run ./scripts/transcheck [dir ...]
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm/schema"
)

const limitations = `renamed or derived columns · values passed as extra parameters · ` +
	`receivers that are not parameters (see the resolution rate) · builder-style ` +
	`assignment · columns set after the literal · source columns read as identity or ` +
	`envelope by sibling code · destination columns with no source counterpart (invisible) · ` +
	`thresholds rather than definitions · columns filled by an untyped update map · ` +
	`columns post-filled by a middleware in another file · writes through a pointer of a ` +
	`type this command does not resolve`

// knownFalsePositives is the pair of misses this command's own first reading
// produced on this tree. They are printed on every run for the reason given in
// the package comment: a reader has to meet them before the numbers, or the
// numbers teach the wrong lesson.
var knownFalsePositives = []struct {
	kind    string
	where   string
	because string
}{
	{
		"extra argument",
		"iam/biz/user/usecase.go: IssueAgentTeamsToken",
		"TTLSeconds travels as SignAgentTeamsService's second parameter, not as a claim field",
	},
	{
		"rename",
		"hitl/agentteams.go: CreateAgentTeams",
		"Payload arrives as PayloadJSON, a renamed and serialised column",
	},
	{
		"rename",
		"service/plugin/fleet.go: PluginInstallRequest ← PluginSpec, and its mirror in core/edge/biz/plugin.go",
		"the same rename at both ends of one hop: Plugin: spec.Name here, Name: req.Plugin there",
	},
	{
		"rename",
		"pigcoding/session.go: SessionStartOptions ← Start",
		"Tools and SessionLog arrive as PiG's ExtraTools and SessionManager; both renames carry their reasons inline",
	},
	{
		"assignment after the literal",
		"pigcoding/session.go: SessionStartOptions.CWDOverride",
		"set on the next statement, and only when the caller asked for one — nil means inherit",
	},
	{
		"narrowing",
		"service/aiops/service.go (3 sites)",
		"a literal inside a type switch copies the branch's own case out of a wider struct",
	},
	{
		"narrowing",
		"service/aiops/service.go: ToolEvent",
		"the fourth site in the same file, and the only one that copies more than the case's own field",
	},
	{
		"flattening",
		"cmd/opskeeper-edge/autonomy.go: autonomyAuditRow",
		"Row.Trigger is spread across the wire's Kind/Metric/Threshold triple, on purpose",
	},
	{
		"derived identity",
		"biz/edge/usecase.go: HostFacts ← HostInfo (Fingerprint, HardwareFingerprint)",
		"both are hashed into fp, which keys the row through the seed literal and the legacy rebind; neither is a fact to copy",
	},
	{
		"envelope projection",
		"biz/nodefleet/tunnelprocess.go: ProjectEvent",
		"EdgeID, Frame and At are the transport envelope, and the port's shape is the raw record underneath it",
	},
	{
		"closure capture",
		"edge/auditlog/pump.go and edge/autonomy/pump.go: PumpOptions",
		"Sink, Sender and Link are dereferenced once and captured into the destination's Send func",
	},
	{
		"fan-out",
		"cmd/opskeeper/aiopskernel.go: PersistDeps ← agentKernelInput",
		"one input struct feeds the persister, the host, the provider and the driver; each literal sees a slice of it",
	},
	{
		"assignment after the literal",
		"(dest) edge/pigsupervisor/supervisor.go: ProcessHealth.Version",
		"the same class as the source-side one: the value is only knowable from a live process, so it is set on the next statement",
	},
	{
		"source has no counterpart",
		"(dest) edge/plugins/databasemetrics/spec.go: metricscommon.Target",
		"the TLS and credential columns belong to targets this source kind cannot be; there is nothing to copy",
	},
	{
		"two-step write",
		"(dest) 19 sites over model.* — a create statement writes what this call knows",
		"ID/CreatedAt/UpdatedAt carry gorm autoIncrement/autoCreateTime/autoUpdateTime; ApprovedBy, DecidedAt, Status, Seq, PrevHash and Hash are written by the later Decide/SetResult/ChainStamper calls. This is the destination-side twin of the fan-out class",
	},
	{
		"two-step write",
		"(dest) biz/edge/usecase.go: devicemodel.Device seed",
		"OSVersion and DiskTotalBytes are set by the UpdateHostFacts call two lines below; the seed is the identity and the facts call is the facts",
	},
}

// minFields is the width below which a literal is not treated as a
// translation. A three-field literal is as likely to be a constructor call
// site or a test fixture as a copy, and the report is only useful while the
// sites in it are worth reading.
const minFields = 5

// projectionRatio is the share of a literal's fields that must be plain
// selector expressions before the literal counts as a copy rather than a
// declaration. A DTO definition assigns literals, calls and conversions; a
// translation assigns `other.Field`.
const projectionRatio = 0.7

// goFile is one parsed production file with the import aliases it declares.
type goFile struct {
	dir     string
	path    string
	aliases map[string]string // alias -> import directory
	// selfDir is what a bare type name resolves to.
	selfDir string
	// fset is the one the file was parsed with, so a node found in a later
	// pass can still be turned into a line number. Positions from a
	// different fileset are not comparable with it, which is why the
	// passes that walk the same file share this one.
	fset *token.FileSet
}

// typeIndex maps a directory and a type name to that struct's field names.
type typeIndex map[string]map[string][]string

// tagIndex maps a directory and a type name to that struct's fields' gorm
// tags.
//
// It exists for one question the first two directions could not answer: a
// column nothing in Go writes may still be written by the ORM, and the tag
// is the evidence. The report used to assert that for nineteen sites at
// once, in prose, with nothing behind it — "ID/CreatedAt/UpdatedAt carry
// gorm autoIncrement/autoCreateTime/autoUpdateTime" was a claim about code
// nobody had read. Reading the tag turns the claim into a measurement, and
// a column with no tag and no writer is a different sentence from a column
// with no tag and a writer.
type tagIndex map[string]map[string]map[string]string

// columnIndex maps a type to the names gorm would accept as an update key.
//
// gorm's map path accepts two spellings: a field name, which it looks up and
// maps, and a column name, which it emits verbatim when the lookup misses
// (callbacks/update.go, clause.Assignment{Column: clause.Column{Name: k}}).
// A key that is neither updates nothing and reports nothing, which is why
// this set exists — a call site writing "approvedBy" is not caught by a
// reviewer as easily as one writing "Approved_By", because the first looks
// like a column from a different naming style and the second looks like a
// typo on sight.
type columnIndex map[string]map[string]bool

// dbColumn is gorm's own name for a field.
//
// It is a call rather than a re-implementation, and the first version of this
// file did re-implement it: an underscore before every capital, which turns
// ResultJSON into result_j_s_o_n and misses every column in the repository
// whose name ends in an initialism. That produced nine confident mismatches
// on a tree where nothing is wrong, and a re-implementation that is wrong in
// the direction of "flagging" is worse than no check at all — it is nine
// false positives on the first run, and a reader who learns to skip the
// section has lost the one thing the section was for.
//
// gorm is already a dependency of this module, and the namer is the exact
// function the runtime will use, so the rule cannot drift from the thing it
// is checking.
func dbColumn(name string) string {
	return schema.NamingStrategy{}.ColumnName("", name)
}

// sites is the report, kept as a named type so it can carry the printer.
type sites []*site

// site is one candidate translation.
type site struct {
	file string
	fn   string
	// dest is the type being constructed, as written.
	dest string
	// source is the struct the columns come from, when it resolved.
	source string
	// carried is every field the literal sets.
	carried int
	// sourceFields is how many fields the source struct has.
	sourceFields int
	// dropped is what the literal does not set, by name.
	dropped []string
	// resolved says whether source was established at all.
	resolved bool

	// The destination direction. The two directions answer different
	// questions and both were needed: the source direction finds a value
	// that exists and is not carried, and the destination direction finds
	// a column with nothing to carry it. Both real defects found on this
	// tree were in this direction — os_version and disk_total_bytes were
	// permanently zero because tunnel.HostInfo had no column for them, and
	// cache_write_tokens was missing because UsageFrame had none — and
	// neither is visible from the source side at any resolution rate.
	destResolved bool
	destFields   int
	destUnset    []string

	// The third direction. destKey is the destination type as the index
	// knows it — "dir.Type" — which is what a write elsewhere in the tree
	// is recorded against, and verdicts is what that lookup found. Without
	// it the destination list says "nothing sets this column" about a
	// column the rest of the repository sets on the next statement, in
	// another constructor, or through the ORM, and a reader has no way to
	// tell those three apart from a hole.
	destKey string
	// destAt is where this site's own literal is. Without it the third
	// direction answers the question with the question: the literal under
	// examination is itself a construction of the type, so every column it
	// sets would be reported as "another literal sets it" — which is how a
	// column the orm fills gets credited to the site that failed to.
	destAt   string
	verdicts []columnVerdict
}

// columnVerdict is what the third direction found for one destination column.
type columnVerdict struct {
	column   string
	reason   verdictReason
	evidence string
}

// verdictReason orders the answers from strongest evidence to weakest, and
// the report prints them in that order because the order is the point: a
// column backed by a literal of the same type somewhere else is a two-step
// write, and a column with nothing at all is a hole, and they look
// identical until something is counted.
type verdictReason int

const (
	// verdictElsewhere: another composite literal of this exact type sets it.
	verdictElsewhere verdictReason = iota
	// verdictSameFunc: a field of this name is assigned inside the same
	// function. The receiver's type is not resolved, so this is evidence
	// rather than proof, and it is reported as such.
	verdictSameFunc
	// verdictSameFile: assigned elsewhere in the same file. Weaker still.
	verdictSameFile
	// verdictORM: gorm fills it, by an explicit option or by convention.
	verdictORM
	// verdictDBDefault: the column carries a default, so the database
	// supplies the value at insert. It is a writer, and it is not the same
	// writer: nothing in the application is responsible for it.
	verdictDBDefault
	// verdictNowhere: no literal, no assignment, and nothing in the schema
	// that would fill it.
	verdictNowhere
)

func (r verdictReason) String() string {
	switch r {
	case verdictElsewhere:
		return "another literal of this type sets it"
	case verdictSameFunc:
		return "a field of this name is assigned in the same function"
	case verdictSameFile:
		return "a field of this name is assigned elsewhere in this file"
	case verdictORM:
		return "the orm writes it"
	case verdictDBDefault:
		return "the database supplies a default"
	default:
		return "no writer this command can see"
	}
}

// writeIndex records every place the tree constructs a type or assigns a
// field name, so a column the site under inspection does not set can be
// looked up rather than declared lost.
type writeIndex struct {
	// constructed is "dir.Type" -> column -> places. It is type-exact: a
	// literal of model.Proposal is evidence about model.Proposal and
	// about nothing else.
	constructed map[string]map[string][]write
	// assignedInFile is file path -> column -> places. It is name-exact
	// and type-blind, which is why its verdicts are ranked below
	// constructed and labelled weaker in the output.
	assignedInFile map[string]map[string][]write
	// assignedInFunc is the same, narrowed to one function of that file.
	assignedInFunc map[string]map[string][]write
}

// write is one place a column was seen being set.
type write struct {
	where string
	fn    string
}

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	files, index, tags, columns, err := scan(dirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "transcheck:", err)
		os.Exit(2)
	}
	all, _, updates := analyse(files, index, tags, columns)
	all.print(os.Stdout, updates)
	// Always 0. See the package comment.
	os.Exit(0)
}

func scan(dirs []string) ([]*goFile, typeIndex, tagIndex, columnIndex, error) {
	var files []*goFile
	index := typeIndex{}
	tags := tagIndex{}
	columns := columnIndex{}
	fset := token.NewFileSet()
	for _, root := range dirs {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if skipDir(info.Name(), path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			// The directory is keyed relative to the scan root, because that
			// is the form an import path can be turned back into: the module
			// prefix is stripped to a repository-relative path, and a
			// fixture's own prefix is matched by its trailing segments. Keying
			// on the absolute path works in the repository and breaks in every
			// fixture, which is the wrong way round.
			rel, relErr := filepath.Rel(root, filepath.Dir(path))
			if relErr != nil {
				rel = filepath.Dir(path)
			}
			rel = filepath.ToSlash(rel)
			f := &goFile{dir: rel, path: path, aliases: map[string]string{}, selfDir: rel, fset: fset}
			for _, imp := range parsed.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				alias := ""
				if imp.Name != nil {
					alias = imp.Name.Name
				} else {
					alias = p[strings.LastIndex(p, "/")+1:]
				}
				f.aliases[alias] = p
			}
			for _, decl := range parsed.Decls {
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
					if !ok {
						continue
					}
					if index[f.dir] == nil {
						index[f.dir] = map[string][]string{}
					}
					index[f.dir][ts.Name.Name] = fieldNames(st)
					if g := gormTags(st); len(g) > 0 {
						if tags[f.dir] == nil {
							tags[f.dir] = map[string]map[string]string{}
						}
						tags[f.dir][ts.Name.Name] = g
					}
					if cols := columnForms(st); len(cols) > 0 {
						// Keyed by the same "dir.Type" the destination
						// direction uses. Keying on the directory alone would
						// let a column of one struct vouch for a key written
						// against another, and two structs in one package
						// both having a status is the normal case, not the
						// exotic one.
						columns[f.dir+"."+ts.Name.Name] = cols
					}
				}
			}
			files = append(files, f)
			return nil
		})
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return files, index, tags, columns, nil
}

func skipDir(name, path string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "__pycache__":
		return true
	}
	// The tools analyse the tree; they are not part of it.
	return strings.HasPrefix(filepath.ToSlash(path), "scripts/")
}

func dirOf(path string) string {
	return filepath.ToSlash(filepath.Dir(path))
}

// columnForms returns every spelling gorm accepts as an update key for this
// struct's fields: the field name itself, the column it maps to when the tag
// says so, and the default name the namer would derive.
func columnForms(st *ast.StructType) map[string]bool {
	out := map[string]bool{}
	for _, f := range st.Fields.List {
		if f.Tag != nil {
			if tag, err := strconv.Unquote(f.Tag.Value); err == nil {
				for _, opt := range strings.Split(reflect.StructTag(tag).Get("gorm"), ";") {
					if v, ok := strings.CutPrefix(opt, "column:"); ok && v != "" {
						out[v] = true
					}
				}
			}
		}
		for _, n := range f.Names {
			out[n.Name] = true
			if c := dbColumn(n.Name); c != "" {
				out[c] = true
			}
		}
	}
	return out
}

// gormTags returns the gorm tag of every field that carries one, keyed by
// field name. The whole tag is kept rather than parsed: the question is
// whether the ORM manages the column, and which option it is comes second.
func gormTags(st *ast.StructType) map[string]string {
	out := map[string]string{}
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			continue
		}
		value, ok := reflect.StructTag(tag).Lookup("gorm")
		if !ok {
			continue
		}
		for _, n := range f.Names {
			out[n.Name] = value
		}
	}
	return out
}

func fieldNames(st *ast.StructType) []string {
	var out []string
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if n.IsExported() {
				out = append(out, n.Name)
			}
		}
	}
	return out
}

func analyse(files []*goFile, index typeIndex, tags tagIndex, columns columnIndex) (sites, *writeIndex, updateReport) {
	var out sites
	writes := &writeIndex{
		constructed:    map[string]map[string][]write{},
		assignedInFile: map[string]map[string][]write{},
		assignedInFunc: map[string]map[string][]write{},
	}
	// The two passes share one parse per file on purpose. The first pass
	// indexes every construction and assignment in the tree, and the second
	// asks the sites about it; a site cannot be judged against an index
	// that was built from a different view of the same source.
	parsedFiles := make([]*ast.File, len(files))
	for i, f := range files {
		parsed, err := parser.ParseFile(f.fset, f.path, nil, 0)
		if err != nil {
			continue
		}
		parsedFiles[i] = parsed
		writes.collect(f, parsed, index)
	}
	for i, f := range files {
		parsed := parsedFiles[i]
		if parsed == nil {
			continue
		}
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			params := paramTypes(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if s, ok := translationAt(lit, f, params, index); ok {
					s.file = f.path
					s.fn = fd.Name.Name
					s.destAt = at(f, lit)
					out = append(out, &s)
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].dropped) != len(out[j].dropped) {
			return len(out[i].dropped) > len(out[j].dropped)
		}
		if out[i].sourceFields != out[j].sourceFields {
			return out[i].sourceFields > out[j].sourceFields
		}
		return out[i].file < out[j].file
	})
	// Last, so that a site is judged against every construction in the tree
	// rather than against the ones the walk happened to reach first.
	for _, s := range out {
		s.verdicts = writes.verdictsFor(s, tags)
	}
	return out, writes, untypedUpdateKeys(files, index, columns)
}

// collect records every construction and every field assignment in one file.
//
// The function walk is not decoration: a column assigned three statements
// after the literal is the same statement's second half, and a column
// assigned in a different function of the same file is a weaker kind of
// evidence. Collapsing the two would make the ranking in verdictsFor a
// claim rather than a measurement.
func (w *writeIndex) collect(f *goFile, parsed *ast.File, index typeIndex) {
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			// A package-level var initialised from a literal is still a
			// construction of that type, and it is a writer.
			w.collectNode(f, decl, "", index)
			continue
		}
		w.collectNode(f, fd.Body, fd.Name.Name, index)
	}
}

func (w *writeIndex) collectNode(f *goFile, root ast.Node, fn string, index typeIndex) {
	note := func(n ast.Node) write { return write{where: at(f, n), fn: fn} }
	ast.Inspect(root, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			key, ok := f.typeKey(n.Type, index)
			if !ok {
				return true
			}
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				id, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if w.constructed[key] == nil {
					w.constructed[key] = map[string][]write{}
				}
				w.constructed[key][id.Name] = append(w.constructed[key][id.Name], note(n))
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id := sel.Sel
				if w.assignedInFile[f.path] == nil {
					w.assignedInFile[f.path] = map[string][]write{}
				}
				w.assignedInFile[f.path][id.Name] = append(w.assignedInFile[f.path][id.Name], note(n))
				if fn == "" {
					continue
				}
				if w.assignedInFunc[f.path] == nil {
					w.assignedInFunc[f.path] = map[string][]write{}
				}
				w.assignedInFunc[f.path][id.Name] = append(w.assignedInFunc[f.path][id.Name], note(n))
			}
		}
		return true
	})
}

// verdictsFor turns the destination's unset columns into one answer each.
//
// The order is the ranking of the evidence, and the last entry is the one
// that matters: a column that no literal of the type sets, no assignment in
// the file touches and no gorm tag manages is a column with no writer in this
// tree, which is the shape every real defect found so far has had.
func (w *writeIndex) verdictsFor(s *site, tags tagIndex) []columnVerdict {
	var out []columnVerdict
	for _, column := range s.destUnset {
		v := columnVerdict{column: column}
		elsewhere := w.elsewhere(s.destKey, s.destAt, column)
		switch {
		case elsewhere != "":
			v.reason = verdictElsewhere
			v.evidence = elsewhere
		case len(w.assignedInFunc[s.file][column]) > 0:
			v.reason = verdictSameFunc
			v.evidence = w.assignedInFunc[s.file][column][0].where
		case len(w.assignedInFile[s.file][column]) > 0:
			v.reason = verdictSameFile
			v.evidence = w.assignedInFile[s.file][column][0].where
		default:
			reason, evidence := ormVerdict(tags, s.destKey, column)
			v.reason, v.evidence = reason, evidence
		}
		out = append(out, v)
	}
	return out
}

// ormManaged is the set of column names gorm fills in by convention rather
// than by tag. It is a convention of the library, not of this repository, so
// the tool has to know it or it will call four of the most ordinary columns
// in the tree orphans — and a report whose ordinary answers are wrong is the
// failure this command was written to avoid.
var ormManaged = map[string]bool{"CreatedAt": true, "UpdatedAt": true, "DeletedAt": true}

// elsewhere returns where a construction of this type other than the one
// under examination sets the column, or "" when there is none.
func (w *writeIndex) elsewhere(key, self, column string) string {
	for _, at := range w.constructed[key][column] {
		if at.where != self {
			return at.where
		}
	}
	return ""
}

// ormVerdict says who fills this column when the database is the writer, and
// returns an empty reason when it is not.
//
// The first version of this function asked the wrong question. It treated
// any gorm tag as evidence, and a mapping tag is not one: `column:paused_at`
// says the column exists, and says nothing at all about whether anything
// ever puts a time in it. That mistake would have cleared four of the
// columns on the hitl proposal row — PausedAt, ResumedAt among them — which
// are exactly the columns whose emptiness is worth knowing about. A tag is
// a mapping; only three of its options, plus the three conventional names,
// make the library write a value, and a `default:` makes the database write
// one at insert rather than the library.
func ormVerdict(tags tagIndex, key, column string) (verdictReason, string) {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return verdictNowhere, ""
	}
	fields := tags[key[:i]][key[i+1:]]
	if fields == nil {
		return verdictNowhere, ""
	}
	tag := fields[column]
	for _, opt := range []string{"autoCreateTime", "autoUpdateTime", "autoIncrement"} {
		if strings.Contains(tag, opt) {
			return verdictORM, opt
		}
	}
	for _, opt := range strings.Split(tag, ";") {
		if v, ok := strings.CutPrefix(opt, "default:"); ok && v != "" {
			return verdictDBDefault, "default:" + v
		}
	}
	// The convention only applies to a type that is a model, which is what
	// having any tag on any field establishes. Without that condition a
	// plain struct with a CreatedAt field would be reported as filled by a
	// database it has never heard of.
	if ormManaged[column] {
		return verdictORM, "by convention"
	}
	return verdictNowhere, ""
}

// typeKey resolves the type a literal or a bare type name refers to, in the
// same form the type index is keyed by.
func (f *goFile) typeKey(e ast.Expr, index typeIndex) (string, bool) {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		d, isImport := f.aliases[pkg.Name]
		if !isImport {
			return "", false
		}
		_, dir, ok := lookupDir(index, dirCandidates(d), t.Sel.Name)
		if !ok {
			return "", false
		}
		return dir + "." + t.Sel.Name, true
	case *ast.Ident:
		if _, ok := index[f.selfDir][t.Name]; !ok {
			return "", false
		}
		return f.selfDir + "." + t.Name, true
	}
	return "", false
}

// at renders a node's position as file:line.
func at(f *goFile, n ast.Node) string {
	pos := f.fset.Position(n.Pos())
	return pos.Filename + ":" + strconv.Itoa(pos.Line)
}

// translationAt decides whether lit is a copy of one struct into another
// package's type, and resolves the source when the receiver is a parameter.
func translationAt(lit *ast.CompositeLit, f *goFile, params map[string]string, index typeIndex) (site, bool) {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return site{}, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return site{}, false
	}
	if _, isImport := f.aliases[pkg.Name]; !isImport {
		return site{}, false
	}
	carried, projections, receivers := readLiteral(lit)
	if len(carried) < minFields {
		return site{}, false
	}
	if float64(projections)/float64(len(carried)) < projectionRatio {
		return site{}, false
	}
	s := site{dest: pkg.Name + "." + sel.Sel.Name, carried: len(carried)}
	set := map[string]bool{}
	for _, c := range carried {
		set[c] = true
	}
	// The destination is written as pkg.Type with pkg an import alias in this
	// file, so it resolves the same way the source does once that one is
	// known — and it resolves even when the source does not, which is the
	// point: a hole in a contract shows up as a destination column nobody
	// fills whatever the receiver turned out to be.
	if d, isImport := f.aliases[pkg.Name]; isImport {
		if fields, dir, ok := lookupDir(index, dirCandidates(d), sel.Sel.Name); ok {
			s.destResolved = true
			s.destKey = dir + "." + sel.Sel.Name
			s.destFields = len(fields)
			for _, name := range fields {
				if !set[name] {
					s.destUnset = append(s.destUnset, name)
				}
			}
		}
	}
	// The receiver is the identifier most of the projections hang off. A
	// literal that mixes several receivers is still a copy, but the source
	// is then not one struct and resolving it would be a guess.
	if len(receivers) != 1 {
		return s, true
	}
	var recv string
	for r := range receivers {
		recv = r
	}
	expr, ok := params[recv]
	if !ok {
		return s, true
	}
	name := expr
	candidates := []string{f.selfDir}
	if i := strings.LastIndex(expr, "."); i >= 0 {
		alias := expr[:i]
		d, isImport := f.aliases[alias]
		if !isImport {
			return s, true
		}
		name = expr[i+1:]
		candidates = dirCandidates(d)
	}
	fields, ok := lookup(index, candidates, name)
	if !ok {
		return s, true
	}
	s.resolved = true
	s.source = name
	s.sourceFields = len(fields)
	for _, name := range fields {
		if !set[name] {
			s.dropped = append(s.dropped, name)
		}
	}
	return s, true
}

// readLiteral returns the field names a composite literal sets, the count of
// those whose value is a plain selector, and the identifier those selectors
// hang off.
func readLiteral(lit *ast.CompositeLit) (carried []string, projections int, receivers map[string]bool) {
	receivers = map[string]bool{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		carried = append(carried, key.Name)
		proj, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		root, ok := proj.X.(*ast.Ident)
		if !ok {
			// in.Rule.Conditions and friends: the root is still the
			// receiver, two hops out.
			if mid, ok := proj.X.(*ast.SelectorExpr); ok {
				if r2, ok := mid.X.(*ast.Ident); ok {
					projections++
					receivers[r2.Name] = true
					continue
				}
			}
			continue
		}
		projections++
		receivers[root.Name] = true
	}
	return carried, projections, receivers
}

// paramTypes maps each parameter name to the type expression as written.
func paramTypes(fd *ast.FuncDecl) map[string]string {
	out := map[string]string{}
	if fd.Type.Params == nil {
		return out
	}
	for _, f := range fd.Type.Params.List {
		expr := typeString(f.Type)
		if len(f.Names) == 0 {
			continue
		}
		for _, n := range f.Names {
			out[n.Name] = expr
		}
	}
	return out
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	}
	return fmt.Sprintf("%T", e)
}

// dirCandidates turns an import path into the directories the scan may have
// indexed it under, longest first.
//
// The repository's own module path is stripped to what follows the module
// prefix, which is what the walk keys on. A fixture or a vendored tree names
// something else, so the trailing segments are tried as well rather than
// failing the whole resolution: the report is a reading, and a reading that
// gives up on an unusual prefix reports less than the tree contains.
func dirCandidates(importPath string) []string {
	const prefix = "github.com/vincent-wuhan/opskeeper/"
	var out []string
	if i := strings.Index(importPath, prefix); i >= 0 {
		out = append(out, importPath[i+len(prefix):])
	}
	parts := strings.Split(importPath, "/")
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[i:], "/"))
	}
	if len(out) == 0 {
		out = append(out, importPath)
	}
	return out
}

func lookup(index typeIndex, dirs []string, name string) ([]string, bool) {
	fields, _, ok := lookupDir(index, dirs, name)
	return fields, ok
}

// lookupDir is lookup that also says which directory answered, because a
// column has to be looked up by the identity of its type and not by its
// name: two packages may both declare a Status, and only one of them is the
// one the site is talking about.
func lookupDir(index typeIndex, dirs []string, name string) ([]string, string, bool) {
	for _, d := range dirs {
		if fields, ok := index[d][name]; ok {
			return fields, d, true
		}
	}
	return nil, "", false
}

// shortReason is the compact form used in the per-site counts.
func shortReason(r verdictReason) string {
	switch r {
	case verdictElsewhere:
		return "set elsewhere"
	case verdictSameFunc:
		return "assigned in this function"
	case verdictSameFile:
		return "assigned in this file"
	case verdictORM:
		return "the orm writes it"
	case verdictDBDefault:
		return "a database default fills it"
	default:
		return "unaccounted"
	}
}

// updateFinding is one column name handed to gorm that names no column of
// the model the call updates.
type updateFinding struct {
	where  string
	model  string
	column string
}

// untypedUpdateKeys finds every string key this tree hands to gorm through
// a map, and checks it against the model the call says it is updating.
//
// This is the blind spot the third direction documents, measured instead of
// described. The reason it is here rather than in a gate is the same reason
// the whole command is a report: the check is only as good as the model
// resolution, and the resolution is a syntactic walk up a call chain. A
// mismatch is certain to be worth reading; a match is not a proof, and the
// report says which.
// updateReport is what the untyped-key check found, and — the part that makes
// a zero meaningful — how much it managed to look at.
type updateReport struct {
	findings []updateFinding
	// checked is the number of Updates calls whose model this command
	// resolved. skipped is the number whose model it could not. A report
	// that printed only the findings would read "clean" in both cases, and
	// "clean" is the one word this command is not allowed to say about work
	// it did not do.
	checked int
	skipped int
}

func untypedUpdateKeys(files []*goFile, index typeIndex, columns columnIndex) updateReport {
	var out []updateFinding
	var total updateReport
	for _, f := range files {
		parsed, err := parser.ParseFile(f.fset, f.path, nil, 0)
		if err != nil {
			continue
		}
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			rep := updateKeysIn(f, fd, index, columns)
			rep.findings = append(rep.findings, out...)
			out = rep.findings
			total.checked += rep.checked
			total.skipped += rep.skipped
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].where != out[j].where {
			return out[i].where < out[j].where
		}
		return out[i].column < out[j].column
	})
	total.findings = out
	return total
}

// updateKeysIn is the check for one function, and the function boundary is
// load-bearing rather than a convenience.
//
// The first version collected `updates := map[string]any{...}` across the
// whole file and looked the name up at the call. `updates` is the most
// popular variable name in a repository of gorm repositories, so every
// function's keys were attributed to every other function's call, and the
// report named nine columns on a call that writes seven — none of them the
// nine. A per-file scope is not a coarse version of the right answer; it is
// a different and wrong answer that looks right until you read one line of
// the output.
func updateKeysIn(f *goFile, fd *ast.FuncDecl, index typeIndex, columns columnIndex) updateReport {
	locals := map[string][]string{}
	// Locals first: `updates := map[string]any{...}` then `Updates(updates)`
	// is the shape the hitl store uses, and the keys are one statement away
	// from the call that consumes them.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			lit, ok := rhs.(*ast.CompositeLit)
			if !ok {
				continue
			}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok || !isAnyStringMap(mt) {
				continue
			}
			id, ok := as.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			locals[id.Name] = append(locals[id.Name], literalKeys(lit)...)
		}
		return true
	})
	// Index assignment into such a local: updates["decided_at"] = now
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		id, ok := idx.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, known := locals[id.Name]; !known {
			return true
		}
		if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if name, err := strconv.Unquote(lit.Value); err == nil {
				locals[id.Name] = append(locals[id.Name], name)
			}
		}
		return true
	})

	rep := updateReport{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Updates" {
			return true
		}
		// Only calls that actually hand gorm a string map are in scope;
		// Updates(struct) and Updates(column) are typed and the compiler
		// has already had its say about them.
		if !carriesStringMap(call, locals) {
			return true
		}
		model := modelInChain(call, f, index)
		if model == "" {
			rep.skipped++
			return true
		}
		accepted := columns[model]
		if accepted == nil {
			rep.skipped++
			return true
		}
		rep.checked++
		var keys []string
		switch arg := call.Args[0].(type) {
		case *ast.CompositeLit:
			mt, ok := arg.Type.(*ast.MapType)
			if !ok || !isAnyStringMap(mt) {
				return true
			}
			keys = literalKeys(arg)
		case *ast.Ident:
			keys = locals[arg.Name]
		default:
			return true
		}
		for _, key := range keys {
			if !accepted[key] {
				rep.findings = append(rep.findings, updateFinding{at(f, call), model, key})
			}
		}
		return true
	})
	return rep
}

// carriesStringMap says whether this Updates call is the untyped shape, so
// that the counts above are about that shape and not about every update in
// the tree.
func carriesStringMap(call *ast.CallExpr, locals map[string][]string) bool {
	switch arg := call.Args[0].(type) {
	case *ast.CompositeLit:
		mt, ok := arg.Type.(*ast.MapType)
		return ok && isAnyStringMap(mt)
	case *ast.Ident:
		_, ok := locals[arg.Name]
		return ok
	}
	return false
}

func literalKeys(lit *ast.CompositeLit) []string {
	var keys []string
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			continue
		}
		if name, err := strconv.Unquote(k.Value); err == nil {
			keys = append(keys, name)
		}
	}
	return keys
}

func isAnyStringMap(mt *ast.MapType) bool {
	key, ok := mt.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	switch v := mt.Value.(type) {
	case *ast.Ident:
		return v.Name == "any" || v.Name == "interface{}"
	case *ast.InterfaceType:
		return true
	}
	return false
}

// modelInChain walks up a chained call — db.Model(&m.T{}).Where(..).Updates(..)
// — to the model the statement updates, and returns it as the type index
// knows it. It gives up rather than guessing: a chain it cannot read is a
// site this check does not cover, and pretending otherwise would be the one
// failure mode a silent check has.
func modelInChain(call *ast.CallExpr, f *goFile, index typeIndex) string {
	for cur := call; cur != nil; {
		sel, ok := cur.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if sel.Sel.Name == "Model" && len(cur.Args) > 0 {
			return modelTypeOf(cur.Args[0], f, index)
		}
		next, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

func modelTypeOf(arg ast.Expr, f *goFile, index typeIndex) string {
	for {
		switch e := arg.(type) {
		case *ast.UnaryExpr: // &model.T{}
			arg = e.X
			continue
		case *ast.ParenExpr:
			arg = e.X
			continue
		case *ast.CompositeLit: // model.T{}
			key, ok := f.typeKey(e.Type, index)
			if !ok {
				return ""
			}
			return key
		case *ast.CallExpr: // new(model.T)
			if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" && len(e.Args) == 1 {
				arg = e.Args[0]
				continue
			}
			return ""
		default:
			return ""
		}
	}
}

// columnList prints a group of columns with the place the evidence is, and
// stops at six. A group of twenty is a list nobody reads, and a list nobody
// reads is the failure mode this whole command was written against.
func columnList(group []columnVerdict) string {
	const shown = 6
	var b strings.Builder
	for i, v := range group {
		if i == shown {
			fmt.Fprintf(&b, "…(+%d more)", len(group)-shown)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(v.column)
		if v.evidence != "" {
			b.WriteString(" (")
			b.WriteString(v.evidence)
			b.WriteString(")")
		}
	}
	return b.String()
}

func (all sites) print(w io.Writer, updates updateReport) {
	fmt.Fprintln(w, "hand-written struct translations, read from both ends")
	fmt.Fprintln(w, "  a translation is a composite literal of another package's type whose fields are")
	fmt.Fprintln(w, "  copied one by one from a struct here. it fails at either end:")
	fmt.Fprintln(w, "    - from the source: a column the source has and the literal drops, so the value")
	fmt.Fprintln(w, "      is computed upstream and never arrives (add a column to the source and the")
	fmt.Fprintln(w, "      literal keeps compiling, shipping the new column as a zero value);")
	fmt.Fprintln(w, "    - from the destination: a column the literal's type HAS and nothing can fill,")
	fmt.Fprintln(w, "      which ships as a zero value no matter how the source grows. on this tree both")
	fmt.Fprintln(w, "      real defects were of this second kind, and neither is visible from the first.")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  %d site(s) with at least %d fields and at least %.0f%% plain projections\n",
		len(all), minFields, projectionRatio*100)

	resolved, flagged, destResolved, destFlagged := 0, 0, 0, 0
	for _, s := range all {
		if s.resolved {
			resolved++
		}
		if len(s.dropped) > 0 {
			flagged++
		}
		if s.destResolved {
			destResolved++
		}
		if len(s.destUnset) > 0 {
			destFlagged++
		}
	}
	fmt.Fprintf(w, "  reading from the source: %d resolved the source struct, %d of them leave at\n", resolved, flagged)
	fmt.Fprintln(w, "  least one column unset")
	if resolved < len(all) {
		fmt.Fprintf(w, "  %d did NOT resolve. they are ABSENT from that list, which is not the\n", len(all)-resolved)
		fmt.Fprintln(w, "  same as being clean: a site this command cannot read is a site nobody is checking.")
	}
	fmt.Fprintf(w, "  reading from the destination: %d of %d resolved it, %d leave at least one\n",
		destResolved, len(all), destFlagged)
	fmt.Fprintln(w, "  column unset. this direction needs no receiver, so it covers the sites the")
	fmt.Fprintln(w, "  source direction cannot read at all.")
	fmt.Fprintln(w, "")

	// The four known misses are printed here, above the sites, because the
	// first version printed them at the end while the comment claimed the
	// reader met them first. A tool whose prose about itself is wrong is the
	// same failure as a gate whose verdict is wrong, and this one was easier
	// to catch only because the claim was written down.
	fmt.Fprintln(w, "")
	// The count is derived rather than written down. The first version of
	// this section said "four kinds" and then printed twelve, which is the
	// same failure as a gate whose verdict disagrees with its own comment:
	// nobody notices a stale number in prose, and everybody trusts it.
	fmt.Fprintf(w, "  %d kinds of flag below that are not defects, each one established by reading\n", len(knownFalsePositives))
	fmt.Fprintln(w, "  the site rather than by the analysis. most of them were found in the source")
	fmt.Fprintln(w, "  direction and are known to recur in the destination one; the two entries marked")
	fmt.Fprintln(w, "  (dest) were found there.")
	for _, k := range knownFalsePositives {
		fmt.Fprintf(w, "    - %-16s %s: %s\n", k.kind, k.where, k.because)
	}
	fmt.Fprintln(w, "  a report whose first answers were all wrong is not a gate. it is a list of")
	fmt.Fprintf(w, "  places to look, and the %d lines above are the reason to look sceptically.\n", len(knownFalsePositives))

	for _, s := range all {
		if !s.resolved {
			continue
		}
		head := fmt.Sprintf("  %2d/%2d columns  %-34s <- %-22s %s", s.sourceFields-len(s.dropped), s.sourceFields, s.dest, s.source, s.file)
		if len(s.dropped) == 0 {
			fmt.Fprintln(w, head+"  (all set)")
			continue
		}
		fmt.Fprintln(w, head)
		fmt.Fprintf(w, "          unset: %s\n", strings.Join(s.dropped, ", "))
	}
	if resolved == 0 {
		fmt.Fprintln(w, "  (no site resolved its source struct — the run measured nothing)")
	}

	// The destination list, printed as its own section rather than as extra
	// columns on the source one: the two questions have different denominators
	// and merging them would let a site that resolved one way and not the
	// other read as a single verdict.
	dest := sites{}
	for _, s := range all {
		if s.destResolved {
			dest = append(dest, s)
		}
	}
	sort.Slice(dest, func(i, j int) bool {
		if len(dest[i].destUnset) != len(dest[j].destUnset) {
			return len(dest[i].destUnset) > len(dest[j].destUnset)
		}
		if dest[i].destFields != dest[j].destFields {
			return dest[i].destFields > dest[j].destFields
		}
		return dest[i].file < dest[j].file
	})
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  destination columns that nothing sets — every one of these ships as a zero value:")
	// The third direction. The line above used to end at the column names,
	// which is the point at which a reader has to stop and go looking, and
	// looking is what nobody does twice. Each name now carries the answer
	// the tree gives about it, and the answers are ranked so that the ones
	// with evidence behind them are not read as loudly as the one without.
	totalUnset, accounted, orphans := 0, 0, 0
	for _, s := range dest {
		head := fmt.Sprintf("  %2d/%2d columns  %-34s <- %-22s %s",
			s.destFields-len(s.destUnset), s.destFields, s.dest, s.source, s.file)
		if len(s.destUnset) == 0 {
			fmt.Fprintln(w, head+"  (all set)")
			continue
		}
		fmt.Fprintln(w, head)
		byReason := map[verdictReason][]columnVerdict{}
		for _, v := range s.verdicts {
			byReason[v.reason] = append(byReason[v.reason], v)
			totalUnset++
			if v.reason == verdictNowhere {
				orphans++
			} else {
				accounted++
			}
		}
		fmt.Fprintf(w, "          %d column(s) this call does not set:", len(s.destUnset))
		for _, r := range []verdictReason{verdictElsewhere, verdictSameFunc, verdictSameFile, verdictORM, verdictDBDefault, verdictNowhere} {
			if n := len(byReason[r]); n > 0 {
				fmt.Fprintf(w, "  %d %s", n, shortReason(r))
			}
		}
		fmt.Fprintln(w)
		for _, r := range []verdictReason{verdictElsewhere, verdictSameFunc, verdictSameFile, verdictORM, verdictDBDefault, verdictNowhere} {
			group := byReason[r]
			if len(group) == 0 {
				continue
			}
			fmt.Fprintf(w, "            %-26s %s\n", shortReason(r)+":", columnList(group))
		}
	}
	if len(dest) == 0 {
		fmt.Fprintln(w, "  (no destination struct resolved — the second direction measured nothing)")
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  %d destination column(s) are not set at the site that was flagged. %d have\n", totalUnset, accounted)
	fmt.Fprintf(w, "  a writer this command found and %d are unaccounted for.\n", orphans)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  read the second number as a list of places to look, not as a count of holes.")
	fmt.Fprintln(w, "  three shapes hide a writer from this analysis, and all three are real patterns in")
	fmt.Fprintln(w, "  this repository rather than hypotheticals:")
	fmt.Fprintln(w, "    - a middleware that post-fills columns on every event. core/domains/server/")
	fmt.Fprintln(w, "      middleware/audit.go sets Event.IP and Event.UserAgent on every row, on a")
	fmt.Fprintln(w, "      local, in another file — which is why those two read as unaccounted here.")
	fmt.Fprintln(w, "    - an untyped update map. core/manager/data/hitl/store's Transition writes")
	fmt.Fprintln(w, "      paused_by, decided_at and a dozen more through map[string]any, so the write")
	fmt.Fprintln(w, "      is a string literal rather than a field and no analysis of assignments can")
	fmt.Fprintln(w, "      see it. that path is checked instead by a guard on the keys themselves.")
	fmt.Fprintln(w, "    - a gorm tag that only maps. `column:paused_at` says the column exists, not that")
	fmt.Fprintln(w, "      anything ever puts a time in it. only autoCreateTime / autoUpdateTime /")
	fmt.Fprintln(w, "      autoIncrement and the three conventional names make the library write one,")
	fmt.Fprintln(w, "      and only a `default:` makes the database write one at insert.")
	fmt.Fprintln(w, "  none of that makes the first bucket a clearance either: a writer in another")
	fmt.Fprintln(w, "  package reached through a pointer of unknown type is invisible here, so a")
	fmt.Fprintln(w, "  column counted as written is examined, not explained.")

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  column names handed to the database through an untyped map:")
	fmt.Fprintf(w, "  %d call(s) checked, %d skipped because the model could not be read.\n",
		updates.checked, updates.skipped)
	if len(updates.findings) == 0 {
		fmt.Fprintln(w, "  every key checked names a column of the model it is written against.")
	} else {
		for _, u := range updates.findings {
			fmt.Fprintf(w, "    %s updates %s with %q, which is not a column of that model\n",
				u.where, u.model, u.column)
		}
		fmt.Fprintln(w, "  a key that names no column is not an error: gorm emits it into the SQL")
		fmt.Fprintln(w, "  as a column name and updates zero rows, so the row still moves state and")
		fmt.Fprintln(w, "  the field nobody meant to write keeps its old value. the count above is")
		fmt.Fprintln(w, "  the number of mismatches found, not the number of such calls: a chain")
		fmt.Fprintln(w, "  this walk cannot read is counted as clean, and that is the same shape of")
		fmt.Fprintln(w, "  hole this report is about, one level up.")
	}

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  what this command cannot see, and what it gets wrong:")
	for i, l := range strings.Split(limitations, " · ") {
		fmt.Fprintf(w, "  %d. %s\n", i+1, l)
	}
}
