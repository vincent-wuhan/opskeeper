#!/usr/bin/env bash
# build-edge-bundle.sh — assemble the ADR-024 edge upgrade bundle.
#
# A bundle is `edge-bundle-<arch>-<version>.tar.gz` whose flat root
# carries every binary install-edge.sh would have placed + a
# MANIFEST.txt that the edge-side apply-pending-upgrade.sh consumes
# (per-file sha256 + dest path).
#
# Inputs (must already exist; this script does NOT cross-compile):
#   bin/<arch>/opskeeper-edge
#   bin/<arch>/node_exporter
#   bin/<arch>/process_exporter
#   bin/<arch>/mysqld_exporter
#   bin/<arch>/postgres_exporter
#   bin/<arch>/redis_exporter
#   bin/<arch>/mongodb_exporter
#   bin/<arch>/promtail
#   bin/<arch>/otelcol-contrib
#   bin/<arch>/pig                  (the node's AI agent — REQUIRED, see below)
#   deploy/install/apply-pending-upgrade.sh
#
# Outputs:
#   $OUT/edge-bundle-<arch>-<version>.tar.gz
#   $OUT/edge-bundle-<arch>-<version>.tar.gz.sha256
#
# Usage: build-edge-bundle.sh <version> <arch> <out_dir>

set -euo pipefail

VERSION=${1:?usage: build-edge-bundle.sh <version> <arch> <out_dir>}
ARCH=${2:?arch}
OUT=${3:?out_dir}

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN_DIR=$REPO_ROOT/bin/$ARCH
APPLY_SCRIPT=$REPO_ROOT/deploy/install/apply-pending-upgrade.sh

# (src_in_bundle, mode, dest, source_file_on_disk, required?)
#
# The last column is the reason this array has one. A bundle that is missing
# an optional entry is degraded; a bundle that is missing a required one is a
# node that comes up healthy and cannot do its job. The exporters are
# optional for that reason — a node without promtail loses log shipping and
# everything else works. `pig` is not: the edge spawns it as a child process,
# and a node without it starts, authenticates, answers questions, and offers
# the model an empty toolset. Nothing about that failure looks like a
# packaging bug, which is exactly why it has to be a build failure here
# rather than a discovery on a customer host.
ENTRIES=(
  "opskeeper-edge            0755 /usr/local/bin/opskeeper-edge                            $BIN_DIR/opskeeper-edge              required"
  "node_exporter          0755 /usr/local/lib/opskeeper-edge/node_exporter              $BIN_DIR/node_exporter              optional"
  "process_exporter       0755 /usr/local/lib/opskeeper-edge/process_exporter           $BIN_DIR/process_exporter           optional"
  "mysqld_exporter        0755 /usr/local/lib/opskeeper-edge/mysqld_exporter            $BIN_DIR/mysqld_exporter            optional"
  "postgres_exporter      0755 /usr/local/lib/opskeeper-edge/postgres_exporter          $BIN_DIR/postgres_exporter          optional"
  "redis_exporter         0755 /usr/local/lib/opskeeper-edge/redis_exporter             $BIN_DIR/redis_exporter             optional"
  "mongodb_exporter       0755 /usr/local/lib/opskeeper-edge/mongodb_exporter           $BIN_DIR/mongodb_exporter           optional"
  "promtail               0755 /usr/local/lib/opskeeper-edge/promtail                   $BIN_DIR/promtail                   optional"
  "otelcol-contrib        0755 /usr/local/lib/opskeeper-edge/otelcol-contrib            $BIN_DIR/otelcol-contrib            optional"
  "pig                    0755 /usr/local/lib/opskeeper-edge/pig                       $BIN_DIR/pig                        required"
  "apply-pending-upgrade.sh 0755 /usr/local/lib/opskeeper-edge/apply-pending-upgrade.sh $APPLY_SCRIPT                       optional"
)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$OUT"
manifest=$work/MANIFEST.txt
{
  echo "# ADR-024 bundle manifest"
  echo "# fields: sha256  mode  src_in_bundle  dest_path"
} > "$manifest"

echo "$VERSION" > "$work/VERSION"

for entry in "${ENTRIES[@]}"; do
  # shellcheck disable=SC2086
  set -- $entry
  src_in_bundle=$1
  mode=$2
  dest=$3
  src_file=$4
  required=${5:-optional}

  if [[ ! -f "$src_file" ]]; then
    if [[ "$required" == "required" ]]; then
      echo "build-edge-bundle: REQUIRED $src_in_bundle is missing at $src_file" >&2
      echo "  A bundle without it installs cleanly and produces a node that" >&2
      echo "  starts, authenticates, and offers the model no tools at all." >&2
      echo "  Build it first: make build-pig-all  (or the matching build-edge-<arch>)" >&2
      exit 1
    fi
    echo "build-edge-bundle: missing $src_file — skipping (bundle will be incomplete)" >&2
    continue
  fi
  install -m 0755 "$src_file" "$work/$src_in_bundle"
  sha=$(sha256sum "$work/$src_in_bundle" | awk '{print $1}')
  echo "$sha  $mode  $src_in_bundle  $dest" >> "$manifest"
done

tarball=$OUT/edge-bundle-$ARCH-$VERSION.tar.gz
tar -C "$work" -czf "$tarball" .
sha256sum "$tarball" | awk '{print $1}' > "$tarball.sha256"

echo "edge-bundle:"
ls -lh "$tarball"
echo "sha256: $(cat "$tarball.sha256")"
