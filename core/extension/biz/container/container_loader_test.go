package container

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The adapter exists so the importer can ask one question without importing
// this package. What is worth testing is therefore not "does it load" — the
// loader has its own tests for that — but the two things the adapter decides
// on its own account: which kind it reports, and whether it fills anything in.

// TestLoadContainerReportsTheKindItActuallyLoaded is the property the old two
// -call shape could not hold.
//
// The importer used to call LoadPluginContainer and then DetectContainer, so
// the `kind` in the conversion report came from a second reading of the same
// directory. Nothing stopped the two from disagreeing, and nothing noticed
// when they did. One call cannot produce that disagreement.
func TestLoadContainerReportsTheKindItActuallyLoaded(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		want ContainerKind
	}{
		{"claude", fixturePack("claude_pack"), ContainerClaude},
		{"openclaw", fixturePack("openclaw_pack"), ContainerOpenclaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ContainerLoader{}.LoadContainer(tc.dir)
			if err != nil {
				t.Fatalf("LoadContainer: %v", err)
			}
			if got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q; the report would name a container the "+
					"importer had not loaded", got.Kind, tc.want)
			}
		})
	}
}

// TestLoadContainerDeclinesADirectoryThatIsNotAContainer pins the error
// contract.
//
// A directory with no recognised layout is not a container with an empty
// identity. The importer's answer to "I cannot read this" has to be to refuse
// the import, and a zero ContainerSource would let a caller mistake a refusal
// for a successful read of an empty pack.
func TestLoadContainerDeclinesADirectoryThatIsNotAContainer(t *testing.T) {
	got, err := ContainerLoader{}.LoadContainer(t.TempDir())
	if err == nil {
		t.Fatalf("LoadContainer accepted a directory with no manifest and returned %+v", got)
	}
	// Compared against the type's own zero value rather than field by
	// field, so a field added to ContainerSource later is covered by this
	// assertion instead of quietly escaping it.
	if !reflect.DeepEqual(got, domain.ContainerSource{}) {
		t.Errorf("LoadContainer returned %+v alongside an error; the zero value has to "+
			"be the zero value so a caller that ignores the error cannot read a pack that "+
			"was never there", got)
	}
}

// TestLoadContainerForwardsWhatTheSourceSaidAndInventsNothing is the half of
// the port contract that is easy to get wrong in the direction that looks
// helpful.
//
// The importer falls back to the directory name when a source states no id,
// and to "0.0.0" when it states no version. A loader that applied those
// defaults would be making the converter's decisions on the converter's
// behalf, and the fallback would then be invisible at the one place a reader
// would look for it — the report, which cannot show a value it did not
// receive.
func TestLoadContainerForwardsWhatTheSourceSaidAndInventsNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A manifest that declares a name and nothing else. No version, no
	// description — both of which the importer defaults.
	manifest := `{"name": "no-version-pack"}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	got, err := ContainerLoader{}.LoadContainer(root)
	if err != nil {
		t.Fatalf("LoadContainer: %v", err)
	}
	if got.Version != "" {
		t.Errorf("Version = %q, want empty; the importer defaults a missing version to "+
			"0.0.0, and a loader that did it too would make that fallback unchangeable "+
			"without changing this file", got.Version)
	}
	if got.Description != "" {
		t.Errorf("Description = %q, want empty; nothing about a missing description is "+
			"the loader's to fill in", got.Description)
	}
	if got.Kind != ContainerClaude {
		t.Errorf("Kind = %q, want claude", got.Kind)
	}
}

// TestLoadPluginContainerStillLoads is the regression guard for the split.
//
// LoadPluginContainer became a three-line wrapper over detectContainerForLoad
// plus loadPluginContainer, so that the adapter could reuse one detection
// rather than making a second. A refactor that leaves the public entry point
// unable to load anything is exactly what that split could have done.
func TestLoadPluginContainerStillLoads(t *testing.T) {
	res, err := LoadPluginContainer(fixturePack("claude_pack"))
	if err != nil {
		t.Fatalf("LoadPluginContainer: %v", err)
	}
	if res.Pack == nil {
		t.Fatal("Pack is nil after the loader was split")
	}
}
