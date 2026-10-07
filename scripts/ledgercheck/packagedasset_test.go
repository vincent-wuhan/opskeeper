package ledgercheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestAnUnguardedInstallAssetIsStagedAsRequired is the nineteenth check, and it
// is the first one about what leaves the repository rather than what is in it.
//
// The release tarball is assembled by dist/package.sh, one asset at a time,
// and it has two stances: `copy_opt` warns and continues when the source is
// missing, and `require_asset` stops. Both are right, for different files, and
// nothing in either file says which is which. A file moves from one to the
// other by being renamed, by a merge, or by an install script gaining a guard
// nobody told the packager about — and the failure mode is a tarball that
// assembles cleanly, ships, and cannot install.
//
// The rule this encodes is the one the install scripts already imply:
//
//	read behind `if [[ -f … ]]`  → optional, copy_opt is correct
//	read with no guard at all     → required, and copy_opt will ship a broken
//	                                tarball while printing a warning nobody
//	                                is watching for at 02:00
//
// Two assets were on the wrong side of it when this was written, both read
// unguarded by install.sh AND upgrade.sh: docker-compose.yml (a `docker
// compose up` with no compose file cannot start) and .env.example (a
// generated .env with nothing to copy from has silently unset variables).
// state-dirs.sh was a third, found a decision earlier.

// installScripts are the entry points that run from inside the tarball, so
// their reads are the ones the tarball has to satisfy.
var installScripts = []string{
	"install.sh", "upgrade.sh", "uninstall.sh", "apply-pending-upgrade.sh",
}

// scriptAssetRE captures a read of an asset out of the directory the script
// was unpacked into.
var scriptAssetRE = regexp.MustCompile(`\$\{?SCRIPT_DIR\}?/([A-Za-z0-9_.-]+)`)

// guardedAssetRE captures the same read behind an existence test. It is
// matched first and subtracted, because a read the script already tolerates
// missing is exactly the optional case.
var guardedAssetRE = regexp.MustCompile(`if \[\[ -f "?\$\{?SCRIPT_DIR\}?/([A-Za-z0-9_.-]+)"?`)

// copyOptRE captures an asset staged with the lenient stance.
var copyOptRE = regexp.MustCompile(`copy_opt "\$\{REPO_ROOT\}/deploy/install/([A-Za-z0-9_.-]+)"`)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func TestAnUnguardedInstallAssetIsStagedAsRequired(t *testing.T) {
	guarded, unguarded := map[string]bool{}, map[string]string{}
	for _, name := range installScripts {
		rel := "../../deploy/install/" + name
		if _, err := os.Stat(filepath.FromSlash(rel)); err != nil {
			continue
		}
		body := repoFile(t, rel)
		for _, m := range guardedAssetRE.FindAllStringSubmatch(body, -1) {
			guarded[m[1]] = true
		}
		for _, m := range scriptAssetRE.FindAllStringSubmatch(body, -1) {
			// Scoped to this script on purpose. The classification is per
			// reader, not per asset: an asset that install.sh reads behind a
			// guard but upgrade.sh does not is optional for one entry point
			// and required for the other, and a tarball is a single file
			// handed to both. A global "guarded anywhere wins" rule would let
			// upgrade.sh's guard launder a read install.sh cannot survive —
			// which is the exact shape of M2 in the reverse-verification run
			// (dropping install.sh's frontier.yaml guard has to go red on its
			// own, with upgrade.sh's guard still in place).
			if guarded[m[1]] {
				continue
			}
			if prev, dup := unguarded[m[1]]; !dup {
				unguarded[m[1]] = name
			} else {
				unguarded[m[1]] = prev + "+" + name
			}
		}
	}
	if len(unguarded) == 0 {
		t.Fatal("no install script reads an unguarded asset, so this check is reading nothing")
	}

	packaged := repoFile(t, "../../dist/package.sh")
	lenient := map[string]bool{}
	for _, m := range copyOptRE.FindAllStringSubmatch(packaged, -1) {
		lenient[m[1]] = true
	}

	var shippedBroken []string
	for asset, readers := range unguarded {
		if lenient[asset] {
			shippedBroken = append(shippedBroken, fmt.Sprintf("%s (read unguarded by %s)", asset, readers))
		}
	}
	sort.Strings(shippedBroken)
	if len(shippedBroken) > 0 {
		t.Errorf("%d asset(s) an install script reads with no existence check are staged with copy_opt, "+
			"which warns and continues when the source is missing:\n  %s\n"+
			"a tarball without one of these assembles cleanly, ships, and cannot install — and the "+
			"warning is printed by a packaging step nobody reads. Stage them with require_asset.",
			len(shippedBroken), strings.Join(shippedBroken, "\n  "))
	}
}
