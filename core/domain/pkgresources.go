package domain

// A Pi package's resource classes, and the legacy container names that map
// onto them.
//
// This table lives in `core` rather than beside the one converter that reads
// it, for a reason that is only visible once you try to keep it honest.
// PiG decides the list: `coding/packagecontent` enumerates the kinds a
// package may carry, and that list is the thing that moves. A converter
// carrying its own hand-written subset can only be checked against PiG by
// code that is allowed to import PiG, and `core/pig` is the only module that
// is. So the names have to be readable from both sides, which means they
// cannot live in either of them.
//
// Two legacy spellings are remapped rather than copied:
//
//   - commands → prompts. PiG names that resource class `prompts`, and a
//     converter that left the old name in place would produce a package
//     whose commands are silently undiscoverable — which is
//     indistinguishable from a plugin that shipped nothing.
//   - skills → skills, and the rest are spelled the same. The remap table
//     exists anyway, because the two spellings that differ are the whole
//     reason a reader needs to look at this list instead of assuming
//     identity.

// PackageResource is one class of resource a Pi package may carry.
type PackageResource struct {
	// Kind is PiG's own name for the class, and the directory name a
	// package keeps it in.
	Kind string
	// LegacyNames are the directory names a pre-Pi container may spell it
	// with. The first entry is the identity case and is the one a package
	// is written with; the rest are the ones a converter has to rewrite.
	LegacyNames []string
}

// PackageResources is every class a Pi package may carry.
//
// The order is PiG's declaration order, not alphabetical, so a diff against
// upstream shows a moved entry rather than a reshuffled one.
var PackageResources = []PackageResource{
	{Kind: "extensions", LegacyNames: []string{"extensions"}},
	{Kind: "skills", LegacyNames: []string{"skills"}},
	{Kind: "prompts", LegacyNames: []string{"prompts", "commands"}},
	{Kind: "themes", LegacyNames: []string{"themes"}},
	{Kind: "agents", LegacyNames: []string{"agents"}},
	{Kind: "mcp", LegacyNames: []string{"mcp"}},
	{Kind: "hooks", LegacyNames: []string{"hooks"}},
	{Kind: "agent-environments", LegacyNames: []string{"agent-environments"}},
}

// PackageResourceKinds returns just the PiG class names, in declaration
// order. It is the list the PiG-side gate compares against, and returning a
// copy keeps a caller from sorting the package-level slice in place — which
// is exactly the mutation that would make the comparison silently pass or
// silently fail depending on test order.
func PackageResourceKinds() []string {
	out := make([]string, 0, len(PackageResources))
	for _, r := range PackageResources {
		out = append(out, r.Kind)
	}
	return out
}

// PackageDirectoryFor returns the package-relative directory a legacy
// container's spelling of this class is written to, and whether the name is
// a recognised spelling at all.
//
// The two answers are deliberately not merged. "Not a resource directory" and
// "a resource directory that happens to map somewhere else" are different
// facts, and a converter that collapsed them would either drop a class or
// copy one into the wrong place.
func PackageDirectoryFor(legacyName string) (string, bool) {
	for _, r := range PackageResources {
		for _, name := range r.LegacyNames {
			if name == legacyName {
				return r.Kind, true
			}
		}
	}
	return "", false
}
