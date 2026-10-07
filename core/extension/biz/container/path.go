package container

import (
	"path/filepath"
	"strings"
)

// PathHasPrefix returns true when child sits inside parent (or is the
// parent itself). Cleans both paths first so trailing slashes don't
// cause false negatives. Mirrors core/floor/skill/loader.go for consistency.
//
// It is exported for one caller outside this file and one reason: the loader
// in plugin_container.go filters resource paths with the same predicate, and
// while both lived in one package the two call sites shared this one
// implementation. Splitting the package without splitting the predicate would
// have left a copy behind, and a copy of a path-containment check is a copy
// of a security boundary. The third copy, in core/floor/skill, is a different
// module and out of scope here.
func PathHasPrefix(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}
