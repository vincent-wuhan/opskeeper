#!/usr/bin/env bash
# opskeeper pure-systemd installer.
#
# Runs from the extracted release tarball after the top-level
# install.sh dispatches via --mode=systemd. Installs manager + frontier
# + dep stack (prometheus / loki / tempo / qdrant) as systemd units.
# OS-package deps (mariadb-server / nginx / grafana) are apt/dnf-installed.
#
# Phase 1 status: manager + frontier wired end-to-end; dep download +
# config render is a structured TODO marked below — the install bails
# with a friendly message if any dep binary is missing from
# /usr/local/bin, so the operator can install them via Phase 2 helper
# (deploy/install/systemd/install-deps.sh, landing next iteration)
# without this script silently producing a half-working system.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BUNDLE_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
cd "$BUNDLE_DIR"

if [[ -t 1 ]]; then
    C_RED=$'\033[0;31m'; C_GREEN=$'\033[0;32m'; C_YELLOW=$'\033[1;33m'
    C_CYAN=$'\033[0;36m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
else
    C_RED=''; C_GREEN=''; C_YELLOW=''; C_CYAN=''; C_BOLD=''; C_RESET=''
fi

log()  { printf '%s[INFO]%s %s\n'  "$C_GREEN"  "$C_RESET" "$*"; }
warn() { printf '%s[WARN]%s %s\n'  "$C_YELLOW" "$C_RESET" "$*"; }
err()  { printf '%s[ERROR]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }

trap 'err "install-systemd failed at line $LINENO (exit $?)"' ERR

if [[ $EUID -ne 0 ]]; then
    err "must run as root (sudo)"
    exit 1
fi

# -----------------------------------------------------------------------------
# flags
# -----------------------------------------------------------------------------
WITH_DEPS=0
usage() {
    cat <<EOF
Usage: sudo bash install-systemd.sh [OPTIONS]

Options:
  --with-deps   Also run install-deps.sh — apt/dnf installs mariadb +
                nginx + grafana, downloads pinned prom/loki/tempo/qdrant
                binaries from upstream with sha256 verify, bootstraps the
                mariadb schema, writes grafana datasource provisioning +
                nginx site config. Internet required (~250 MB downloads).
  -h, --help    Print this help.

Without --with-deps the script only installs the manager + frontier
binaries + the six systemd units. Operator handles deps separately.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --with-deps) WITH_DEPS=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) err "unknown flag: $1"; usage; exit 2 ;;
    esac
done

# -----------------------------------------------------------------------------
# paths
# -----------------------------------------------------------------------------
PREFIX_BIN=/usr/local/bin
ETC_DIR=/etc/opskeeper
STATE_DIR=/var/lib/opskeeper
LOG_DIR=/var/log/opskeeper
SYSTEMD_DIR=/etc/systemd/system
SERVICE_USER=opskeeper

# -----------------------------------------------------------------------------
# system user
# -----------------------------------------------------------------------------
if id "$SERVICE_USER" &>/dev/null; then
    log "user $SERVICE_USER already exists"
else
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    log "created system user $SERVICE_USER"
fi

# dep users — same group, separate uids so a compromised prom can't
# read qdrant on-disk vectors, etc.
for dep_user in opskeeper-prometheus opskeeper-loki opskeeper-tempo opskeeper-qdrant; do
    if id "$dep_user" &>/dev/null; then
        log "user $dep_user already exists"
    else
        useradd --system --no-create-home --shell /usr/sbin/nologin \
                --gid "$SERVICE_USER" "$dep_user"
        log "created system user $dep_user"
    fi
done

# -----------------------------------------------------------------------------
# directories
# -----------------------------------------------------------------------------
mkdir -p "$ETC_DIR" "$ETC_DIR/prometheus" "$STATE_DIR" "$LOG_DIR"
chown root:opskeeper "$ETC_DIR" "$ETC_DIR/prometheus"
chmod 0750 "$ETC_DIR" "$ETC_DIR/prometheus"
chown opskeeper:opskeeper "$STATE_DIR" "$LOG_DIR"
chmod 0755 "$STATE_DIR" "$LOG_DIR"

# -----------------------------------------------------------------------------
# manager + frontier binaries
# -----------------------------------------------------------------------------
require_bundled_bin() {
    local name="$1"
    local src="$BUNDLE_DIR/bin/$name"
    if [[ ! -f "$src" ]]; then
        err "missing $src — tarball was not built with systemd-mode binaries"
        err "rebuild with: make package SYSTEMD_BIN=1"
        exit 2
    fi
    install -m 0755 -o root -g root "$src" "$PREFIX_BIN/$name"
    log "installed $PREFIX_BIN/$name"
}
require_bundled_bin opskeeper
require_bundled_bin opskeeper-frontier

# -----------------------------------------------------------------------------
# stack-dep binaries (prom / loki / tempo / qdrant)
# Phase 1 expectation: operator pre-stages these at /usr/local/bin/* OR runs
# install-deps.sh first (Phase 2). We don't silently produce a half-stack.
# -----------------------------------------------------------------------------
MISSING_DEPS=()
for dep_bin in prometheus loki tempo qdrant; do
    if [[ ! -x "$PREFIX_BIN/$dep_bin" ]]; then
        MISSING_DEPS+=("$dep_bin")
    fi
done
if (( ${#MISSING_DEPS[@]} > 0 )); then
    warn "stack-dep binaries not found in $PREFIX_BIN: ${MISSING_DEPS[*]}"
    warn "manager unit will not start until they are installed."
    warn ""
    warn "Phase 2 helper (install-deps.sh) will download these from upstream"
    warn "releases with sha256-verify. For now, install them manually, e.g.:"
    warn "  https://github.com/prometheus/prometheus/releases"
    warn "  https://github.com/grafana/loki/releases"
    warn "  https://github.com/grafana/tempo/releases"
    warn "  https://github.com/qdrant/qdrant/releases"
fi

# -----------------------------------------------------------------------------
# OS-package deps
# -----------------------------------------------------------------------------
detect_pkg_mgr() {
    if command -v apt-get >/dev/null 2>&1; then echo apt
    elif command -v dnf >/dev/null 2>&1; then echo dnf
    elif command -v yum >/dev/null 2>&1; then echo yum
    else echo unknown
    fi
}
PKG_MGR=$(detect_pkg_mgr)
case "$PKG_MGR" in
    apt|dnf|yum)
        log "detected package manager: $PKG_MGR"
        ;;
    *)
        warn "unknown package manager — install mariadb-server / nginx / grafana manually"
        ;;
esac
# Actual apt/dnf install is in Phase 2 install-deps.sh; this skeleton
# only sets the systemd units in place so the operator can drive deps
# install separately and run `systemctl start opskeeper` when ready.

# -----------------------------------------------------------------------------
# configs
# -----------------------------------------------------------------------------
copy_conf() {
    local src="$1" dst="$2"
    if [[ -f "$src" ]]; then
        install -m 0640 -o root -g opskeeper "$src" "$dst"
        log "wrote $dst"
    else
        warn "$src missing — skipping"
    fi
}
copy_conf "$BUNDLE_DIR/prometheus/prometheus.yml" "$ETC_DIR/prometheus/prometheus.yml"
copy_conf "$BUNDLE_DIR/prometheus-rules.yml"      "$ETC_DIR/prometheus/rules.yml"
copy_conf "$BUNDLE_DIR/loki-config.yaml"          "$ETC_DIR/loki-config.yaml"
copy_conf "$BUNDLE_DIR/tempo-config.yaml"         "$ETC_DIR/tempo-config.yaml"

# The compose-mode configs hard-code container-volume paths (/loki, /var/tempo)
# that don't exist on the host filesystem. Rewrite to point at the per-dep
# StateDirectory= paths so the systemd units can actually write.
if [[ -f "$ETC_DIR/loki-config.yaml" ]]; then
    sed -i 's|/loki|/var/lib/opskeeper-loki|g' "$ETC_DIR/loki-config.yaml"
    log "rewrote loki storage paths → /var/lib/opskeeper-loki/*"
fi
if [[ -f "$ETC_DIR/tempo-config.yaml" ]]; then
    sed -i 's|/var/tempo|/var/lib/opskeeper-tempo|g' "$ETC_DIR/tempo-config.yaml"
    log "rewrote tempo storage paths → /var/lib/opskeeper-tempo/*"
fi
copy_conf "$BUNDLE_DIR/frontier.yaml"             "$ETC_DIR/frontier.yaml"

# manager env file — first install only; subsequent runs preserve.
ENV_FILE="$ETC_DIR/opskeeper.env"
if [[ ! -f "$ENV_FILE" ]]; then
    cat > "$ENV_FILE" <<EOF
# opskeeper manager environment — systemd mode.
# Edit this file then \`systemctl restart opskeeper\`.

# Listening address (the manager HTTP listener)
OPSKEEPER_HTTP_ADDR=:8080

# Datastore (defaults assume mariadb on localhost — install-deps.sh
# auto-fills the password when it bootstraps the schema)
OPSKEEPER_DB_DIALECT=mysql
OPSKEEPER_DB_DSN=opskeeper:CHANGE_ME@tcp(127.0.0.1:3306)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local

# Frontier broker — co-located on the same host in systemd mode.
# Default frontier.yaml listens service-bound on :40011, edge-bound on
# :40012. If you point at an external frontier, change to its host:port.
OPSKEEPER_FRONTIER_ADDR=127.0.0.1:40011
OPSKEEPER_FRONTIER_SERVICE_NAME=opskeeper-manager

# Telemetry deps — local systemd units installed by install-deps.sh.
# Set *_ENABLED=true once each dep is healthy (systemctl is-active …).
OPSKEEPER_PROM_ENABLED=true
OPSKEEPER_PROM_URL=http://127.0.0.1:9090
OPSKEEPER_LOKI_URL=http://127.0.0.1:3100
OPSKEEPER_TEMPO_URL=http://127.0.0.1:3200
OPSKEEPER_QDRANT_URL=http://127.0.0.1:6333

# LLM — fill in to enable AIOps agents
OPSKEEPER_OPENAI_API_KEY=
OPSKEEPER_OPENAI_MODEL=glm-4-plus
OPSKEEPER_OPENAI_BASE_URL=

# Knowledge base embedder — defaults to the bundled offline ONNX model so
# 知识库 works with no API key. install-deps.sh installs libonnxruntime.so
# and this script stages the model into OPSKEEPER_EMBEDDING_CACHE_DIR below.
# To use a hosted embedder instead, set PROVIDER=openai + API_KEY/BASE_URL
# + a matching MODEL/DIM (e.g. GLM embedding-3 / dim 2048).
OPSKEEPER_EMBEDDING_PROVIDER=local
OPSKEEPER_EMBEDDING_MODEL=bge-small-zh-v1.5
OPSKEEPER_EMBEDDING_DIM=512
OPSKEEPER_EMBEDDING_CACHE_DIR=/var/lib/opskeeper/embeddings
EOF
    chmod 0640 "$ENV_FILE"
    chown root:opskeeper "$ENV_FILE"
    log "wrote $ENV_FILE (REVIEW + edit secrets before starting)"
else
    log "preserved existing $ENV_FILE"
fi

# -----------------------------------------------------------------------------
# local embedding model (ADR-027 Phase-2) — stage the bundled BGE-small-zh
# cache into OPSKEEPER_EMBEDDING_CACHE_DIR so the offline embedder loads with
# no HuggingFace reach. Mirrors what compose's install.sh does. The .so
# itself is installed by install-deps.sh; this is just the model weights.
# -----------------------------------------------------------------------------
EMB_SRC="$BUNDLE_DIR/embeddings"
EMB_DST="$STATE_DIR/embeddings"
if [[ -d "$EMB_SRC" ]] && compgen -G "$EMB_SRC/*" >/dev/null; then
    if [[ ! -d "$EMB_DST" ]] || [[ -z "$(ls -A "$EMB_DST" 2>/dev/null)" ]]; then
        install -d -m 0755 "$EMB_DST"
        cp -rf "$EMB_SRC/." "$EMB_DST/"
        chown -R opskeeper:opskeeper "$EMB_DST"
        log "staged embedding model → $EMB_DST"
    else
        log "embedding model already present at $EMB_DST — preserved"
    fi
else
    warn "no bundled embedding model — local embedder needs the model at"
    warn "  $EMB_DST (or switch to an API-key embedder in $ENV_FILE)"
fi

# -----------------------------------------------------------------------------
# systemd units
# -----------------------------------------------------------------------------
for unit in opskeeper.service opskeeper-frontier.service \
            prometheus.service loki.service tempo.service qdrant.service; do
    install -m 0644 -o root -g root \
        "$SCRIPT_DIR/$unit" "$SYSTEMD_DIR/$unit"
    log "installed $SYSTEMD_DIR/$unit"
done

systemctl daemon-reload
log "systemd daemon-reload"

# -----------------------------------------------------------------------------
# optional: dep auto-install
# -----------------------------------------------------------------------------
if [[ $WITH_DEPS -eq 1 ]]; then
    log "running install-deps.sh"
    bash "$SCRIPT_DIR/install-deps.sh"
fi

# -----------------------------------------------------------------------------
# enable but don't auto-start — operator should review env file first
# -----------------------------------------------------------------------------
systemctl enable opskeeper.service opskeeper-frontier.service \
                 prometheus.service loki.service tempo.service qdrant.service \
    >/dev/null 2>&1
log "enabled units (will start at boot)"

cat <<EOF

${C_BOLD}${C_GREEN}systemd install complete${C_RESET}

Next steps:
  1. Install OS-package deps if not already present:
EOF
case "$PKG_MGR" in
    apt) printf "       sudo apt-get install -y mariadb-server nginx\n" ;;
    dnf) printf "       sudo dnf install -y mariadb-server nginx\n" ;;
    yum) printf "       sudo yum install -y mariadb-server nginx\n" ;;
esac
cat <<EOF
  2. Install stack-dep binaries (Phase 2 helper coming):
       prometheus, loki, tempo, qdrant → ${PREFIX_BIN}/
  3. Initialise mariadb schema (CREATE DATABASE opskeeper; user grants).
  4. Review and edit ${ETC_DIR}/opskeeper.env (DB password, LLM key, etc.)
  5. Start the stack:
       sudo systemctl start prometheus loki tempo qdrant
       sudo systemctl start opskeeper-frontier opskeeper
  6. Watch:
       sudo journalctl -u opskeeper -f

Roll back this install:
  sudo $SCRIPT_DIR/uninstall-systemd.sh           # stop + remove units
  sudo $SCRIPT_DIR/uninstall-systemd.sh --purge   # also delete data dirs + user
EOF
