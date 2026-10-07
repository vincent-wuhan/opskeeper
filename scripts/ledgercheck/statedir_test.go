package ledgercheck

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryStateDirTheCodeWritesHasAHostMount is the seventeenth check, and it
// answers a question the other sixteen do not: the deployment manifests.
//
// Everything else in this package reconciles the ledger against the tree.
// This one reconciles the INSTALL MANIFEST against the code, and it exists
// because a directory can be writable and still be wrong.
//
// The manager image chowns /var/lib/opskeeper to the same uid it runs as, so
// a path under it is writable in the container — nothing fails, nothing logs,
// the ledger's file is written. It is written into the container's writable
// layer, and `compose up` / an image upgrade replaces that layer. The ledger
// the file describes — a federation membership, a crystallisation streak —
// is a record whose entire reason for existing is surviving a restart, and
// the container's writable layer does not survive one. For those two paths a
// missing mount is not one fewer layer of insurance; it is the feature being
// off, while every surface that reports on it looks healthy.
//
// The image already says so for one of them:
//   Dockerfile.opskeeper:248  "Bind-mount this in production to keep them
//   across container restarts."
// and the manifest did not. A comment in one file asserting an invariant
// that another file violates is the exact shape that survives review, so the
// invariant is checked here instead of trusted.
//
// This does not judge whether a path ought to be mounted. It judges one thing:
// every /var/lib/opskeeper/<dir> the code names, and every one the manifest
// mounts, are the same set — modulo directories that are legitimately local
// to a build, each listed below with the reason it is one.

// composeMountRE captures the container side of a bind mount under the
// manager's state root. It keys on the state root and reads to the end of the
// line, because a mount is written as `host:container[:mode]` and the mode is
// not part of the directory name.
var composeMountRE = regexp.MustCompile(`:/var/lib/opskeeper/([A-Za-z0-9_.-]+)`)

// codeStateDirRE captures a manager state directory out of a Go string
// literal. It stops at the first path separator, so /var/lib/opskeeper/skills
// reads as "skills" and a nested /var/lib/opskeeper/system/skills reads as
// "system" — which is the right granularity, because the manifest mounts
// parents, not leaves.
var codeStateDirRE = regexp.MustCompile(`"/var/lib/opskeeper/([A-Za-z0-9_.-]+)`)

// stateDirAllowlisted is where a state directory that the code names but the
// manifest does not mount is allowed to sit, and why.
//
// It is a list and not a boolean because the reason is the part worth
// re-reading in a year. An entry without one is not allowed in: the reason is
// what distinguishes "this deployment shape does not use it" from "somebody
// forgot", and a bare name reads identically to both.
// Keys are bare directory names with no leading slash, which is what all
// three sources produce once their capture groups are written the same way.
// That uniformity is the whole point and it was arrived at the hard way: an
// earlier revision had the manifest's names slashed and the scripts' names
// bare, and every check that compared the two reported all nine directories
// as unaccounted for — including five that had carried a chown line since
// before this file existed. A comparison whose two sides disagree about
// spelling fails in the direction that looks like a large finding, which is
// the one direction a reader is most likely to believe.
var stateDirAllowlisted = map[string]string{
	"db": "the install manifest runs OPSKEEPER_DB_DIALECT=mysql, so " +
		"openSQLite is never reached on this path; the same manifest tells " +
		"an operator switching to sqlite to add the mount themselves, " +
		"next to the line it already carries about the data volume",
}

func stateRoots(t *testing.T) []string {
	t.Helper()
	var roots []string
	for _, mod := range []string{"cmd", "core"} {
		err := filepath.WalkDir(filepath.FromSlash("../../"+mod), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range codeStateDirRE.FindAllStringSubmatch(string(raw), -1) {
				roots = append(roots, filepath.ToSlash(path)+" "+m[1])
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", mod, err)
		}
	}
	sort.Strings(roots)
	return roots
}

// hostSideMountRE captures a data directory as it appears on the HOST side of
// any mount, whichever service declares it.
//
// This is the second of the two sides, and it exists because the first
// attempt at the reverse assertion was wrong in an instructive way. Asking
// "is every directory in the install list also a /var/lib/opskeeper mount?"
// answers no for grafana, loki, mysql, prometheus, qdrant and tempo — all
// six of which the list quite correctly prepares, and all six of which are
// mounted by a DIFFERENT service onto its own container path (/prometheus,
// /var/tempo, …). The check reported correct behaviour as a defect, which is
// the failure direction a reader trusts most.
var hostSideMountRE = regexp.MustCompile(`\$\{OPSKEEPER_DATA_DIR[^}]*\}/([A-Za-z0-9_.-]+):`)

func hostSideStateDirs(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(installManifest))
	if err != nil {
		t.Fatalf("read %s: %v", installManifest, err)
	}
	out := map[string]bool{}
	for _, m := range hostSideMountRE.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	return out
}

// Two limits of this reconciliation, stated here rather than discovered later.
//
// It reads the manifest as text and does not parse which service a volume
// belongs to, so a /var/lib/opskeeper mount under some OTHER service would be
// counted as the manager's and asked to be chowned to 65532. That is the
// conservative direction — a false demand, not a missed one — and no such
// mount exists today.
//
// It also reads the INSTALL manifest only. deploy/docker-compose.yml, the
// development shape, deliberately keeps nothing under /var/lib/opskeeper on
// the host: losing a container's state between `compose up`s is the normal
// expectation of a development stack, and making it durable would hide
// exactly the kind of state a developer wants thrown away. The helm chart is
// not read either, because it mounts the whole state root as one PVC and so
// has no per-directory list to disagree about.
// installManifest is the production shape. The development compose next to
// it deliberately keeps nothing under /var/lib/opskeeper, and the helm chart
// mounts the whole state root as one PVC, so neither has a per-directory list
// to disagree about.
const installManifest = "../../deploy/install/docker-compose.yml"

func mountedStateDirs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(installManifest))
	if err != nil {
		t.Fatalf("read %s: %v", installManifest, err)
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		for _, m := range composeMountRE.FindAllStringSubmatch(line, -1) {
			out[m[1]] = fmt.Sprintf("%s:%d", installManifest, i+1)
		}
	}
	return out
}

func TestEveryStateDirTheCodeWritesHasAHostMount(t *testing.T) {
	written, mounted := map[string]string{}, mountedStateDirs(t)
	for _, hit := range stateRoots(t) {
		i := strings.LastIndex(hit, " ")
		written[hit[i+1:]] = hit[:i]
	}
	if len(written) == 0 {
		t.Fatal("no manager state directory was found in the tree, so this check is reading nothing")
	}
	if len(mounted) == 0 {
		t.Fatal("no manager state directory is mounted in the install manifest, so this check is reading nothing")
	}

	// The manager writes somewhere the install manifest does not keep. For
	// every state directory that exists to outlive a process, that is the
	// feature being off with nothing reporting it.
	var missing []string
	for dir := range written {
		if _, ok := mounted[dir]; ok {
			continue
		}
		if reason, ok := stateDirAllowlisted[dir]; ok {
			if reason == "" {
				t.Errorf("state dir %q is allowlisted with no reason, which is indistinguishable from a forgotten mount", dir)
			}
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (named in %s)", dir, written[dir]))
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the manager writes into %d state director(ies) the install manifest does not keep:\n  %s\n"+
			"either add the mount, or record why this shape does not need it in stateDirAllowlisted — "+
			"a writable container layer is not a restart, and the records these files hold exist to outlive one",
			len(missing), strings.Join(missing, "\n  "))
	}

	// And the other direction: a mount for a directory no code names is a
	// directory an operator will be told exists and never populated.
	var stale []string
	for dir, where := range mounted {
		if _, ok := written[dir]; !ok {
			stale = append(stale, fmt.Sprintf("%s (mounted at %s)", dir, where))
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("the install manifest keeps %d state director(ies) no code names:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// TestEveryMountedStateDirIsChownedByBothInstallScripts is the second half of
// the same reconciliation, and it is the half that turns a mount into a
// working one.
//
// A bind mount the host directory does not own is worse than no mount at
// all. Docker creates a missing host directory as root on first `up`; the
// manager image runs as uid 65532; so the directory exists, the mount is in
// the manifest, `docker compose ps` looks healthy — and the manager cannot
// write to it. The three failure modes are all silent degradation rather than
// a crash, which is why nothing above the file notices:
//
//   federation/   → the root forgets its cluster set and re-enrols everyone
//   crystallize/ → promotion streaks restart from zero on every `compose up`
//   repos/       → every knowledge repo is re-cloned
//
// install.sh already says this out loud ("Without chown, docker creates them
// root-owned on first `up` and the nonroot manager can't write") and already
// enumerates the directories it knows about. The enumeration is the fragile
// part: a mount added to the manifest does not add itself to a shell loop
// written in another file, and the result is a deployment that is configured
// correctly and persists nothing. This is the check that would have caught
// the four mounts this decision added — and it is written after adding them
// rather than before, which is the honest note on its own value.

// stateDirsSH is the single place the host directories and their uids are
// written down, and both install paths read it rather than carrying a copy.
const stateDirsSH = "../../deploy/install/state-dirs.sh"

// stateDir is one row of that list: a directory under the manager's data root
// and the uid its container process runs as. A uid of "-" means "create it,
// do not chown it" — a service whose container runs as root.
type stateDir struct {
	uid string
}

// listedStateDirs runs the list rather than parsing it.
//
// The list is a shell function, so a regex over it is a second description of
// a format that the shell itself already knows how to read: it would drift
// the first time a row gained a field, and it would drift SILENTLY, because a
// regex that stops matching produces an empty set and an empty set reads as
// "nothing to check" to every assertion downstream. Executing the function
// costs one exec and cannot disagree with what install.sh will see.
func listedStateDirs(t *testing.T) map[string]stateDir {
	t.Helper()
	cmd := exec.Command("bash", "-c", "set -euo pipefail; . \"$1\"; opskeeper_state_dirs", "bash", stateDirsSH)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running %s: %v\n%s", stateDirsSH, err, stderr.String())
	}
	outMap := map[string]stateDir{}
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) > 3 {
			t.Fatalf("%s line %d is not <dir> <uid> [mode]: %q", stateDirsSH, i+1, line)
		}
		if _, dup := outMap[fields[0]]; dup {
			t.Fatalf("%s lists %q twice", stateDirsSH, fields[0])
		}
		if fields[1] != "-" && !uidRE.MatchString(fields[1]) {
			t.Fatalf("%s line %d has uid %q, which is neither n:n nor -", stateDirsSH, i+1, fields[1])
		}
		outMap[fields[0]] = stateDir{uid: fields[1]}
	}
	if len(outMap) == 0 {
		t.Fatalf("%s listed no directories, so this check is reading nothing", stateDirsSH)
	}
	return outMap
}

// uidRE is the one shape a uid column may take besides "-".
var uidRE = regexp.MustCompile(`^\d+:\d+$`)

// managerUID is what Dockerfile.opskeeper chowns the state root to and then
// runs as, so it is the uid every mount the manager writes into must carry.
const managerUID = "65532:65532"

func TestEveryMountedStateDirIsPreparedByTheInstallList(t *testing.T) {
	mounted := mountedStateDirs(t)
	if len(mounted) == 0 {
		t.Fatal("no manager state directory is mounted in the install manifest, so this check is reading nothing")
	}
	listed := listedStateDirs(t)

	// A mount the install list does not prepare is created by `compose up`
	// as root, which no log line reports and the mount does not prevent.
	var unprepared []string
	for dir := range mounted {
		if _, ok := listed[dir]; !ok {
			unprepared = append(unprepared, fmt.Sprintf("%s (mounted at %s)", dir, mounted[dir]))
		}
	}
	sort.Strings(unprepared)
	if len(unprepared) > 0 {
		t.Errorf("%d mounted state director(ies) are not in %s:\n  %s\n"+
			"docker creates a missing host directory as root and the image runs as %s, so the mount "+
			"exists, nothing errors, and the nonroot process cannot write",
			len(unprepared), stateDirsSH, strings.Join(unprepared, "\n  "), managerUID)
	}

	// And the uid, which the previous revision of this check read out of two
	// shell scripts with a regex. Getting the directory into the list without
	// giving it the manager's uid produces the same unwritable mount, so the
	// uid is asserted rather than assumed.
	var wrongUID []string
	for dir := range mounted {
		if got, ok := listed[dir]; ok && got.uid != managerUID {
			wrongUID = append(wrongUID, fmt.Sprintf("%s is %s, want %s", dir, got.uid, managerUID))
		}
	}
	sort.Strings(wrongUID)
	if len(wrongUID) > 0 {
		t.Errorf("%d mounted state director(ies) are not owned by the manager uid:\n  %s\n"+
			"these are the directories the manager WRITES into; a different uid is a read-only mount to it",
			len(wrongUID), strings.Join(wrongUID, "\n  "))
	}

	// And the other side, against the HOST side of every service's mount
	// rather than the manager's container side: a directory the install
	// prepares but no service mounts is created, never used, and reads in the
	// list as something the operator should expect to find persisted.
	hostSide := hostSideStateDirs(t)
	var unused []string
	for dir := range listed {
		if !hostSide[dir] {
			unused = append(unused, dir)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf("%s prepares %d director(ies) no service in the manifest mounts:\n  %s\n"+
			"they are created on every install and nothing ever reads them; if one of them was meant to be "+
			"persistent, its mount is missing, and if it was not, it should not be in the list",
			stateDirsSH, len(unused), strings.Join(unused, "\n  "))
	}
}
