package pigcontract

import (
	"sort"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/packagecontent"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// This is the whole of the PiG contract in this file, and it is one line
// long: a package may carry eight classes of resource, and OpsKeeper's
// converter has to carry all eight.
//
// It is a line and it is load-bearing, because the failure it prevents is
// silent and total. The converter reads a legacy container and copies the
// resource directories it recognises. A class it does not recognise is not
// refused and not reported — it is simply not copied, so the package it
// writes loads cleanly, passes review, and arrives on the node missing
// everything of that kind. The console shows a plugin that shipped nothing,
// and the operator has no way to tell that from a plugin that genuinely
// shipped nothing.
//
// The list therefore cannot live beside the converter, because the only
// authority for it is PiG and the only OpsKeeper module allowed to import
// PiG is this one. domain.PackageResources is the shared copy, and this is
// the test that says it still matches.

// TestTheConverterCoversEveryResourceClassPiGDeclares is the gate.
//
// A missing class fails here. So does a class OpsKeeper invented: the
// comparison is on the exact set, so a converter cannot quietly grow a
// directory that PiG would never read, which would be the same silence in
// the other direction — a copied directory that nothing discovers.
func TestTheConverterCoversEveryResourceClassPiGDeclares(t *testing.T) {
	upstream := []string{
		string(packagecontent.Extensions),
		string(packagecontent.Skills),
		string(packagecontent.Prompts),
		string(packagecontent.Themes),
		string(packagecontent.Agents),
		string(packagecontent.MCP),
		string(packagecontent.Hooks),
		string(packagecontent.AgentEnvironments),
	}

	// Sorted, because the comparison below is on the SET and a test that
	// only passes in one declaration order is a test that will fail the
	// next time upstream reorders its constants for reasons of its own.
	want := append([]string(nil), upstream...)
	sort.Strings(want)

	got := domain.PackageResourceKinds()
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("OpsKeeper's converter knows %d resource classes and PiG declares %d.\n"+
			"  ours: %v\n  PiG's: %v\n"+
			"A class PiG declares and we do not is copied by no converter; a class we "+
			"declare and PiG does not is copied by everybody and discovered by nobody.",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resource class %d: OpsKeeper has %q, PiG has %q", i, got[i], want[i])
		}
	}
}

// TestEveryLegacySpellingResolvesToItsOwnClass is the second half.
//
// The remap is where a converter goes quietly wrong. Two classes writing to
// one directory means one of them is lost, and the loss is invisible because
// the package still loads. So every legacy spelling has to resolve, and no
// two may resolve to the same directory unless the class they name is the
// same one.
func TestEveryLegacySpellingResolvesToItsOwnClass(t *testing.T) {
	owners := map[string]string{}
	for _, r := range domain.PackageResources {
		if r.Kind == "" {
			t.Errorf("a resource class has no Kind: %+v", r)
			continue
		}
		if len(r.LegacyNames) == 0 {
			t.Errorf("resource class %q declares no legacy spelling, so no converter can "+
				"recognise it", r.Kind)
			continue
		}
		// The first spelling is the identity case: a package written
		// directly against PiG keeps its resources where PiG expects them.
		if r.LegacyNames[0] != r.Kind {
			t.Errorf("resource class %q lists %q first; the first entry is the name a "+
				"package is written with and has to be PiG's own", r.Kind, r.LegacyNames[0])
		}
		for _, legacy := range r.LegacyNames {
			dir, ok := domain.PackageDirectoryFor(legacy)
			if !ok {
				t.Errorf("legacy spelling %q resolves to nothing", legacy)
				continue
			}
			if dir != r.Kind {
				t.Errorf("legacy spelling %q resolves to %q rather than to its own class %q",
					legacy, dir, r.Kind)
			}
			if other, taken := owners[legacy]; taken {
				t.Errorf("legacy spelling %q is claimed by both %q and %q; whichever converter "+
					"runs second silently overwrites the first", legacy, other, r.Kind)
			}
			owners[legacy] = r.Kind
		}
	}

	// A name nobody claims must come back as unknown rather than as an
	// empty directory, because an empty destination is how a converter ends
	// up writing into the package root.
	if dir, ok := domain.PackageDirectoryFor("not-a-resource"); ok {
		t.Errorf("an unrecognised directory resolved to %q; the second answer has to be false", dir)
	}
}

// TestTheCommandsRemapIsStillDeclared pins the one remap that exists for a
// reason nobody would reconstruct.
//
// `commands` is the legacy Claude spelling of PiG's `prompts`. Nothing
// derives it: remove it and every converted plugin silently loses its
// slash commands, with no error anywhere, because the resulting package is
// a valid Pi package that happens to be missing a resource class.
func TestTheCommandsRemapIsStillDeclared(t *testing.T) {
	dir, ok := domain.PackageDirectoryFor("commands")
	if !ok {
		t.Fatal("the legacy `commands` directory no longer resolves to anything")
	}
	if dir != "prompts" {
		t.Fatalf("`commands` resolves to %q; PiG calls that class %q, and a converter that "+
			"wrote it anywhere else would produce a package whose commands nothing discovers", dir, "prompts")
	}
}
