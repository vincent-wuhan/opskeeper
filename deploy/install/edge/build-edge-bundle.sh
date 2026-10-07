#!/usr/bin/env bash
# build-edge-bundle.sh — rebuild the ADR-024 one-button upgrade bundle on the
# manager host from the loose per-arch edge binaries already staged under the
# install's edge/ dir.
#
# Why this exists: the release tarball used to carry a pre-built
# edge-bundle-<arch>-<version>.tar.gz *in addition to* the loose binaries it
# is a copy of (one for install-edge.sh, one for the upgrade path). That
# double-pack added ~120 MB of incompressible payload to every release. We now
# ship only the loose binaries and reassemble the bundle here at install /
# upgrade time. nginx serves the result statically from /edge/ exactly as
# before, so the edge-side upgrade flow is unchanged.
#
# The bundle layout + MANIFEST format MUST stay byte-compatible with what
# dist/build-edge-bundle.sh produces and what apply-pending-upgrade.sh
# consumes (fields: sha256  mode  src_in_bundle  dest_path).
#
# Usage: build-edge-bundle.sh <edge_dir> <version> [arch]
#   edge_dir   the installed edge dir holding the loose binaries
#              (e.g. /opt/opskeeper/edge); also where the bundle is written.
#   version    e.g. v0.7.159
#   arch       bundle target arch; default linux-amd64 for legacy callers.

set -euo pipefail

EDGE_DIR=${1:?usage: build-edge-bundle.sh <edge_dir> <version> [arch]}
VERSION=${2:?version}
ARCH=${3:-linux-amd64}

# (src_in_bundle  mode  dest_path  loose_file_in_edge_dir  required?)
#
# The last column has to agree with dist/build-edge-bundle.sh: a bundle
# missing an optional entry is degraded, a bundle missing a required one is a
# node that installs and comes up healthy with an empty toolset. `pig` is
# required because the edge spawns it — see the note in dist/build-edge-bundle.sh.
ENTRIES=(
  "opskeeper-edge              0755 /usr/local/bin/opskeeper-edge                          opskeeper-edge-${ARCH}                 required"
  "node_exporter            0755 /usr/local/lib/opskeeper-edge/node_exporter            node_exporter-${ARCH}                 optional"
  "process_exporter         0755 /usr/local/lib/opskeeper-edge/process_exporter         process_exporter-${ARCH}              optional"
  "mysqld_exporter          0755 /usr/local/lib/opskeeper-edge/mysqld_exporter          mysqld_exporter-${ARCH}               optional"
  "postgres_exporter        0755 /usr/local/lib/opskeeper-edge/postgres_exporter        postgres_exporter-${ARCH}             optional"
  "redis_exporter           0755 /usr/local/lib/opskeeper-edge/redis_exporter           redis_exporter-${ARCH}                optional"
  "mongodb_exporter         0755 /usr/local/lib/opskeeper-edge/mongodb_exporter         mongodb_exporter-${ARCH}              optional"
  "promtail                 0755 /usr/local/lib/opskeeper-edge/promtail                 promtail-${ARCH}                      optional"
  "otelcol-contrib          0755 /usr/local/lib/opskeeper-edge/otelcol-contrib          otelcol-contrib-${ARCH}               optional"
  "pig                      0755 /usr/local/lib/opskeeper-edge/pig                     pig-${ARCH}                           required"
  "apply-pending-upgrade.sh 0755 /usr/local/lib/opskeeper-edge/apply-pending-upgrade.sh apply-pending-upgrade.sh            optional"
)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

manifest=$work/MANIFEST.txt
{
  echo "# ADR-024 bundle manifest"
  echo "# fields: sha256  mode  src_in_bundle  dest_path"
} > "$manifest"

echo "$VERSION" > "$work/VERSION"

staged=0
for entry in "${ENTRIES[@]}"; do
  # shellcheck disable=SC2086
  set -- $entry
  src_in_bundle=$1
  mode=$2
  dest=$3
  loose=$4
  required=${5:-optional}
  src_file="$EDGE_DIR/$loose"

  if [[ ! -f "$src_file" ]]; then
    if [[ "$required" == "required" ]]; then
      echo "build-edge-bundle(host): REQUIRED $src_in_bundle is missing at $src_file" >&2
      echo "  The node would install cleanly and offer the model no tools at all." >&2
      echo "  Stage $loose into $EDGE_DIR and re-run." >&2
      exit 1
    fi
    echo "build-edge-bundle(host): missing $src_file — skipping (bundle will be incomplete)" >&2
    continue
  fi
  install -m 0755 "$src_file" "$work/$src_in_bundle"
  sha=$(sha256sum "$work/$src_in_bundle" | awk '{print $1}')
  echo "$sha  $mode  $src_in_bundle  $dest" >> "$manifest"
  staged=$((staged + 1))
done

if [[ "$staged" -eq 0 ]]; then
  echo "build-edge-bundle(host): no loose binaries found under $EDGE_DIR for $ARCH — bundle NOT built" >&2
  exit 1
fi

tarball="$EDGE_DIR/edge-bundle-$ARCH-$VERSION.tar.gz"
tar -C "$work" -czf "$tarball" .
sha256sum "$tarball" | awk '{print $1}' > "$tarball.sha256"

echo "build-edge-bundle(host): wrote $tarball ($staged file(s))"
