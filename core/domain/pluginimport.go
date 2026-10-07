package domain

// This file is the answer to a measurement. `domaincheck -edges` priced the
// `marketplace -> pluginimport` edge at two types and no methods, and the
// measurement was honest about what it could not see: that both types drag
// four more behind them, and two of those four are not the producer's to give —
// `ContainerKind` and `LoadWarning` are declared in `aiops`'s chatruntime and
// shared by three domains. The price column said "two types"; the real closure
// was six, and cutting it needed a decision about who owns the vocabulary
// underneath, not about the two structs at the top.
//
// The consumer is `server/marketplace/import.go`, the route behind
// `POST /v1/marketplace/import`. It does not import the converter. It is handed
// one by the composition root through `SetImporter`, and the only reason it
// named `pluginimport` at all is that the function type it takes spells
// `pluginimport.Options` and `*pluginimport.Report` in its signature. So the
// edge existed to let an HTTP layer say two type names it did not own.
//
// Note what is NOT here: no port interface. The route's seam is already a
// function type it declares itself (`server/marketplace.ImportFunc`), and the
// composition root hands it a package-level function. That is a port, and it
// was already the right shape — the only thing wrong with it was the two names
// in its signature. Adding an interface here would be a second seam over the
// first, and nothing would call it.
//
// The four names below come in two groups, and the split is the point:
//
//   - `ContainerKind` and `LoadWarning` are the *loader's* vocabulary. Three
//     domains need them and none of the three is the loader. They were
//     stranded inside `chatruntime`, which is a business package, so every
//     other domain had to either import it or copy it. One of them copied it —
//     `biz/marketplace` declared a second `LoadWarning` with the comment
//     "mirrors chatruntime.LoadWarning so we don't leak that import out of
//     biz/marketplace", in a file whose own package imported chatruntime eight
//     symbols over. The stated reason was false, and the copy was the evidence.
//
//   - `PluginImport*` is the *conversion's* vocabulary, which one domain owns
//     and the route merely forwards. They are prefixed because, counting type
//     declarations across the repository, `Options` is declared 13 times,
//     `Decision` 10 and `Report` 8; putting any of those three bare into a
//     namespace every domain shares would be the fourteenth, the eleventh and
//     the ninth declaration of words that already mean several things. (Those
//     are type declarations only. An earlier count of mine that also matched
//     methods and struct fields put `Report` at 11, which is the number a
//     grep for the word gives; the honest figure for "how many packages
//     declare a type by this name" is 8.)

// ContainerKind labels which marker file was found in a candidate directory.
//
// The order the detector probes in is openclaw > claude-plugin > bare-skills >
// none, and the three recognised forms are the legacy plugin containers that
// `POST /v1/marketplace/import` exists to convert.
type ContainerKind string

const (
	// ContainerClaude is the `.claude-plugin/plugin.json` form.
	ContainerClaude ContainerKind = "claude"
	// ContainerOpenclaw is the `openclaw.plugin.json` form.
	ContainerOpenclaw ContainerKind = "openclaw"
	// ContainerBareSkills is the skills.sh / vercel-labs/skills form:
	// a directory containing one or more `skills/<name>/SKILL.md`
	// (or a root-level SKILL.md) and NO manifest file. The pack ID
	// and display name are synthesized from the directory name. This
	// is what `npx skills add owner/repo` produces.
	ContainerBareSkills ContainerKind = "bare_skills"
	// ContainerNone means no recognized layout was found.
	ContainerNone ContainerKind = ""
)

// LoadWarning is a non-fatal load issue. Code is a stable identifier
// (e.g. "name_normalized", "missing_when_to_use") so a client can filter
// without scraping Reason text.
type LoadWarning struct {
	// Path is the file the warning applies to (absolute or relative
	// to the loader's root — loader convention).
	Path string `json:"path"`

	// Reason is human-readable English text.
	Reason string `json:"reason"`

	// Code is the stable identifier.
	Code string `json:"code"`
}

// PluginImportRequest configures an import.
type PluginImportRequest struct {
	// Source is the legacy container directory. Required.
	Source string
	// Dest is the package directory to write. Required. It must not
	// already exist: an import that merged into an existing package would
	// leave behind whatever the previous version had and nobody reviewed.
	Dest string
	// Vendor is recorded in the generated manifest. Optional.
	Vendor string
	// Targets is where the package may run. Defaults to the edge, which is
	// the only target a converted package is safe on: nothing here has
	// been reviewed for the control plane, and the control plane holds
	// identity, approval, and the audit ledger.
	Targets Targets
}

// PluginImportDecision is one thing a human still has to decide before the
// generated package can do anything.
//
// These are reported rather than guessed. A converter that filled them in
// would be claiming to have reviewed code it only read the names of.
// The JSON names are declared rather than left to Go's field names because
// this struct is embedded in an HTTP response. Without tags the route would
// answer `{"Field": ..., "Question": ...}` while the LoadWarning slice
// right beside it answers `{"path": ..., "reason": ...}` — two naming
// conventions inside one JSON object, which is a contract the console has
// to be taught the shape of rather than read. Snake_case matches every other
// DTO the console parses. Nothing consumed this response before the import
// page, so there is no wire compatibility to preserve.
type PluginImportDecision struct {
	// Field is the manifest path, e.g. "spec.tools".
	Field string `json:"field"`
	// Question is what has to be answered.
	Question string `json:"question"`
	// Why is the answer not derivable — which is what makes this a
	// decision rather than a missing value.
	Why string `json:"why"`
}

// PluginImportSourceManifest is a read of a container's own package.json.
//
// PiG discovers a package's resources from its manifest when the manifest
// declares them, and from the conventional directories when it does not.
// Those two are mutually exclusive: a package.json carrying a "pi" block
// suppresses convention discovery for every Pi class, so a package whose
// manifest declares one skill and has a skills/ directory with nine loads
// exactly one.
//
// That is why this is reported rather than carried across. A converted
// package has no manifest, so it discovers by convention — which is a
// superset of what a declaring manifest selects, and therefore the safe
// direction. Copying the manifest instead would import its *suppression*:
// every class it left undeclared would stop loading on the node, silently,
// and the operator would be looking at a package that shipped nine skills
// and serves one.
type PluginImportSourceManifest struct {
	// Present is whether a package.json was there at all.
	Present bool `json:"present"`
	// Declares is whether it carried a resource-declaring block ("pi" or
	// "pig"). A package.json with neither is npm metadata and nothing more.
	Declares bool `json:"declares_resources"`
	// Classes is every resource class either block named, sorted.
	Classes []string `json:"classes,omitempty"`
	// Entries maps a class to the paths it declared. It is the list a
	// reviewer needs, because it is the difference between the resources
	// the container served and the ones the converted package will find.
	Entries map[string][]string `json:"entries,omitempty"`
}

// PluginImportReport is what an import produced.
//
// The json tags are a live contract: this struct is the body of
// `POST /v1/marketplace/import` and the console reads every one of them. The
// move changes where the shape is declared, never what goes on the wire.
type PluginImportReport struct {
	// Kind is the container form that was recognised.
	Kind ContainerKind `json:"kind"`
	// Name, Version and Description are carried across from the source.
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	// Skills and Agents are the package-relative paths written.
	Skills []string `json:"skills"`
	Agents []string `json:"agents"`
	// The other resource classes are counts rather than paths: nobody
	// reviews a count, and a report that listed forty identical extension
	// paths would be skimmed past.
	//
	// Themes and AgentEnvironments are here for the same reason the classes
	// they count are copied at all. They are the two an earlier version of
	// this converter did not know about, and a class it did not know about
	// was a class it dropped without saying so — the report read exactly
	// the same whether the container had shipped a theme or not.
	Prompts           int `json:"prompts"`
	MCP               int `json:"mcp"`
	Extension         int `json:"extensions"`
	Themes            int `json:"themes"`
	AgentEnvironments int `json:"agent_environments"`
	// SourceManifest is what the container's own package.json said about
	// where its resources live. It is reported and deliberately not copied;
	// see PluginImportSourceManifest for why copying it would be worse
	// than dropping it.
	SourceManifest PluginImportSourceManifest `json:"source_manifest"`
	// Decisions is what remains undecided. It is never empty for a
	// container that carried no governance, which is all of them.
	Decisions []PluginImportDecision `json:"decisions"`
	// Warnings are the loader's own non-fatal findings, carried through
	// so an import does not quietly drop a parse failure.
	Warnings []LoadWarning `json:"warnings"`
}

// ContainerSource is what one legacy container directory yielded.
//
// It is six fields, and they are the six the importer writes into the
// generated manifest: which form was recognised, the four identity strings
// the manifest carries, and the loader's own non-fatal findings.
//
// The shape is the decision, so it is worth saying what is NOT here. The
// loader's real result type also carries the parsed `Skills` and `Agents`
// trees, and the importer reads **neither** — it copies files by walking the
// source directory itself, because the thing it produces is a directory, not
// an in-memory package. So the port hands back the pack's *identity* and the
// warnings, and the two big trees stay inside the loader where nothing else
// can reach them.
//
// The four identity fields are returned raw and un-defaulted on purpose. The
// importer falls back to the directory name when there is no id, and to
// "0.0.0" when there is no version; those are *conversion policy* — what this
// repository decides a package with no stated version should be — and a port
// that applied them would be making that decision on the loader's behalf.
type ContainerSource struct {
	// Kind is the recognised container form.
	Kind ContainerKind
	// ID is the pack key, empty when the source declared none.
	ID string
	// DisplayName is the human-facing title, empty when the source
	// declared none. It is separate from ID because a source may state
	// one and not the other, and the importer prefers ID when both are
	// present.
	DisplayName string
	// Version follows semver, empty when the source declared none.
	Version string
	// Description is a one-liner, empty when the source declared none.
	Description string
	// Warnings are the loader's non-fatal findings, carried through so an
	// import does not quietly drop a parse failure.
	Warnings []LoadWarning
}

// ContainerLoader is the port the importer holds to read a container.
//
// It replaces a direct call into the agent runtime's package loader, and the
// reason that call was a cross-domain edge is the same one this file's header
// describes for the previous two cuts: the importer needed six strings and
// imported a 692-line container detector, a package model it had no use for,
// and two skill trees it never read.
//
// The second return is an error rather than an empty ContainerSource. A
// directory with no recognised layout is not a container with no identity; it
// is a directory this loader declined to read, and the importer's answer to
// that is to refuse the import rather than synthesise a package out of it.
type ContainerLoader interface {
	// LoadContainer reads dir and reports what it found. It returns an
	// error when dir holds no recognised container layout or cannot be
	// read at all.
	LoadContainer(dir string) (ContainerSource, error)
}
