#!/usr/bin/env bash
# Re-vendor the built-in knowledge vault into the opskeeper binary.
#
# The vault is embedded (go:embed) into the manager binary so a fresh
# install populates its knowledge base with no network access — see
# core/manager/biz/knowledge/builtin_vault.go. There is NO public
# upstream URL: the runtime no longer clones anything (the old
# "cloud sync" pointed at a placeholder that never existed). Vendoring
# is a maintainer step driven from a vault checkout you provide.
#
# Run this after the vault content changes to refresh the vendored copy,
# then commit the diff under core/manager/biz/knowledge/builtin_vault/.
#
# Usage:
#   scripts/sync-builtin-vault.sh <path-to-vault-checkout>
#
# The source dir must be a local checkout — offline and pinned by design.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DST="$REPO_ROOT/core/manager/biz/knowledge/builtin_vault"

if [ "$#" -lt 1 ]; then
  echo "error: missing vault source dir." >&2
  echo "usage: scripts/sync-builtin-vault.sh <path-to-vault-checkout>" >&2
  exit 2
fi
SRC="$1"
[ -d "$SRC" ] || { echo "error: source dir not found: $SRC" >&2; exit 1; }

echo "[sync-builtin-vault] vendoring .md from $SRC → $DST"
rm -rf "$DST"
mkdir -p "$DST"
# Copy only first-party indexable prose (.md), preserving the directory
# tree; skip .git and `reference/external/`.
#
# WHY exclude reference/external/: that subtree is third-party scraped
# article content (LWN / brendangregg / et al) — useful for an internal
# knowledge graph but redistributing other people's articles inside the
# binary is a license headache, and the first-party docs already cover
# the same ground from our angle. The first-party files give the
# operator a topical starter pack; external content stays in the vault
# checkout and is not redistributed in the binary.
(cd "$SRC" && find . \
    -path ./.git -prune -o \
    -path './reference/external' -prune -o \
    -type f -name '*.md' -print) | while read -r f; do
  mkdir -p "$DST/$(dirname "$f")"
  cp "$SRC/$f" "$DST/$f"
done

count="$(find "$DST" -type f -name '*.md' | wc -l | tr -d ' ')"
echo "[sync-builtin-vault] done — $count markdown files vendored."
echo "[sync-builtin-vault] review & commit: git add core/manager/biz/knowledge/builtin_vault"
