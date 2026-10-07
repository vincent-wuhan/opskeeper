#!/usr/bin/env bash
# Copy the canonical PiG extensions into the packages a node installs.
#
# Every package under plugins/pig-ops/ ships its extensions as source,
# because the agent runtime builds them on the node it runs on and a node
# has no access to an unpublished OpsKeeper module. That necessity is the
# whole reason this script exists: the copies are unavoidable, and a hand
# copy is how they drift.
#
# So the copies are generated, and the drift tests in
# core/floor/pluginmanifest are the check that this script was run. Run
# this after touching anything under core/pig/extensions/ or core/wire,
# then run the tests. Editing a packaged file directly is always wrong —
# the next run of this script overwrites it, which is the intended
# outcome.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

canonical_root="core/pig/extensions"
wire_root="core/wire"
packages_root="plugins/pig-ops"

if [[ ! -d "$canonical_root" || ! -d "$packages_root" ]]; then
	echo "sync-pig-ops: expected $canonical_root and $packages_root under $repo_root" >&2
	exit 1
fi
if [[ ! -d "$wire_root" ]]; then
	echo "sync-pig-ops: expected $wire_root under $repo_root" >&2
	exit 1
fi

copied=0
for pkg_dir in "$packages_root"/*/; do
	[[ -d "$pkg_dir" ]] || continue
	pkg="$(basename "$pkg_dir")"
	exts_dir="$pkg_dir/extensions"
	[[ -d "$exts_dir" ]] || continue

	for ext_dir in "$exts_dir"/*/; do
		[[ -d "$ext_dir" ]] || continue
		ext="$(basename "$ext_dir")"
		src="$canonical_root/$ext"

		if [[ ! -d "$src" ]]; then
			echo "sync-pig-ops: $pkg ships extensions/$ext but core/pig/extensions/$ext does not exist" >&2
			echo "               a package may only ship extensions that have a canonical source" >&2
			exit 1
		fi

		# Three things change and only three: the module path moves under
		# the package that ships it, the replace block is dropped, and the
		# wire vocabulary is inlined. A node has no local checkout of
		# OpsKeeper and no local checkout of a pre-stable PiG, so a replace
		# directive would send the node's build to a path that does not
		# exist on it — and a require on github.com/vincent-wuhan/opskeeper
		# /core v0.0.0 would not resolve at all, because that version is
		# never published. The extension then installs cleanly and fails
		# its first build on the node, which is the worst possible time to
		# find out: the agent is up, the credentials are good, the
		# conversation works, and the model has no tools.
		#
		# So core/wire — three files, stdlib only, the DTOs the broker
		# protocol is made of — is copied in as a local package and the
		# one import that names it is rewritten to the packaged module.
		# What remains is a single dependency on the PiG extension SDK,
		# which PiG stages from its own tree at build time, so a node
		# needs nothing from this repository and nothing from the network.
		#
		# The rewrite is deliberately exactly one substitution. The drift
		# test in core/floor/pluginmanifest applies the same substitution
		# to the canonical bytes before comparing, so every other edit
		# still fails the check.
		SYNC_SRC="$src" SYNC_WIRE="$wire_root" SYNC_DST="$ext_dir" \
		SYNC_PKG="$pkg" SYNC_EXT="$ext" python3 - <<'PYTHON'
import io
import os
import re
import shutil

src = os.environ["SYNC_SRC"]
wire_root = os.environ["SYNC_WIRE"]
dst = os.environ["SYNC_DST"]
pkg = os.environ["SYNC_PKG"]
ext = os.environ["SYNC_EXT"]

# The canonical import of the broker vocabulary, and where it lives once
# the package is on a node. Everything else about the file is untouched.
CORE_WIRE = "github.com/vincent-wuhan/opskeeper/core/wire"
PACKAGED_MODULE = (
    "github.com/vincent-wuhan/opskeeper/plugins/pig-ops/%s/extensions/%s" % (pkg, ext)
)
PACKAGED_WIRE = PACKAGED_MODULE + "/wire"


def rewrite(text):
    return text.replace('"%s"' % CORE_WIRE, '"%s"' % PACKAGED_WIRE)


def copy_go_files(from_dir, to_dir, subdir=""):
    """Copy the non-test Go sources, rewriting the one import above.

    The destination is pruned first. A generated tree that is only ever
    added to accumulates files that outlive their source, and a stale copy
    of a deleted wire type would compile and then disagree with the host
    about the protocol.
    """
    if subdir:
        to_dir = os.path.join(to_dir, subdir)
    if os.path.isdir(to_dir):
        shutil.rmtree(to_dir)
    os.makedirs(to_dir)
    count = 0
    for name in sorted(os.listdir(from_dir)):
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        source = os.path.join(from_dir, name)
        with io.open(source, encoding="utf-8") as handle:
            text = handle.read()
        with io.open(os.path.join(to_dir, name), "w", encoding="utf-8") as handle:
            handle.write(rewrite(text))
        count += 1
    return count


extension_files = copy_go_files(src, dst)
wire_files = copy_go_files(wire_root, dst, subdir="wire")

# The go.mod, rewritten. The OpsKeeper module is gone rather than
# repointed: v0.0.0 was a placeholder for a replace directive that a node
# cannot honour, and the honest way to drop a dependency is to delete it.
# What is left is the one module PiG itself stages, so the go.sum carries
# its hashes and nothing else.
# The SDK version is read from the canonical go.mod rather than written here.
# It used to be a literal, and that literal was v0.3.0 while the host had
# already moved to v0.4.0: ten packaged extensions kept building against a
# different SDK than the pig they run beside, and nothing said so, because
# re-running this script re-wrote the same stale version with the same
# confidence. A generator that repeats a fact nobody reads is a second place
# where that fact can go out of date.
#
# Reading it makes the two sides share one source, and failing loudly when the
# source no longer requires the SDK at all is better than writing a plausible
# version into a file no test can check.
sdk_version = None
with io.open(os.path.join(src, "go.mod"), encoding="utf-8") as handle:
    for line in handle:
        found = re.match(
            r"\s*github\.com/MichaelKinsy/PiG/extensions/sdk\s+(v\S+)", line
        )
        if found:
            sdk_version = found.group(1)
            break
if sdk_version is None:
    raise SystemExit(
        "sync-pig-ops: %s no longer requires the PiG extension SDK, so there is "
        "no version to write into %s -- update this script rather than guess one"
        % (os.path.join(src, "go.mod"), os.path.join(dst, "go.mod"))
    )

header = [
    "// %s, as the agent runtime builds it on a node." % ext.replace("_", "-"),
    "//",
    "// GENERATED by scripts/sync-pig-ops.sh from %s." % src,
    "// Do not edit: the next run overwrites this file, and",
    "// TestEveryPackagedExtensionMatchesItsCanonicalSource fails if it drifts.",
    "//",
    "// The copy is deliberate. The agent builds extensions from source on",
    "// the node it runs on, and a node has no access to an unpublished",
    "// OpsKeeper module. A package that referenced the canonical path",
    "// would install and then fail to build, which is the worst time to",
    "// find out. So the wire vocabulary travels inside the extension as a",
    "// local ./wire package, and the only remaining requirement is the PiG",
    "// extension SDK, which the agent stages from its own tree.",
    "module %s" % PACKAGED_MODULE,
    "",
    "go 1.26.0",
    "",
    "require github.com/MichaelKinsy/PiG/extensions/sdk %s" % sdk_version,
    "",
]
with io.open(os.path.join(dst, "go.mod"), "w", encoding="utf-8") as handle:
    handle.write("\n".join(header))

# Only the PiG SDK's hashes. The canonical go.sum resolves the OpsKeeper
# module through a local replace, so copying it verbatim would ship
# checksums for a module this package deliberately no longer names.
sum_path = os.path.join(dst, "go.sum")
with io.open(os.path.join(src, "go.sum"), encoding="utf-8") as handle:
    lines = [
        line
        for line in handle.read().splitlines()
        if line.startswith("github.com/MichaelKinsy/PiG/")
    ]
with io.open(sum_path, "w", encoding="utf-8") as handle:
    handle.write("\n".join(lines) + "\n")

print("%s/%s: %d sources, %d wire files" % (pkg, ext, extension_files, wire_files))
PYTHON

		copied=$((copied + 1))
		echo "  $pkg <- $ext"
	done
done

echo "sync-pig-ops: wrote $copied extensions"
