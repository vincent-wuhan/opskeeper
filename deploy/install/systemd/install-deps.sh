#!/usr/bin/env bash
# opskeeper systemd-mode dep installer (Phase 2).
#
# Two responsibilities:
#   (1) apt/dnf-install OS-package deps: mariadb-server, nginx, grafana.
#       (We use the distro's grafana-oss package when available; otherwise
#       add the Grafana apt repo.)
#   (2) Download upstream Prom / Loki / Tempo / qdrant binaries, verify
#       sha256, install to /usr/local/bin/.
#
# Idempotent: re-runs skip what's already installed and at-target-version.
# Network required for the upstream binary fetches (~250 MB total).

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [[ -t 1 ]]; then
    C_GREEN=$'\033[0;32m'; C_YELLOW=$'\033[1;33m'; C_RED=$'\033[0;31m'
    C_CYAN=$'\033[0;36m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
else
    C_GREEN=''; C_YELLOW=''; C_RED=''; C_CYAN=''; C_BOLD=''; C_RESET=''
fi
log()  { printf '%s[INFO]%s %s\n'  "$C_GREEN"  "$C_RESET" "$*"; }
warn() { printf '%s[WARN]%s %s\n'  "$C_YELLOW" "$C_RESET" "$*"; }
err()  { printf '%s[ERROR]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }

if [[ $EUID -ne 0 ]]; then
    err "must run as root (sudo)"
    exit 1
fi

# -----------------------------------------------------------------------------
# flags
# -----------------------------------------------------------------------------
SKIP_GRAFANA=0
APT_TIMEOUT=600
# GH proxy — empty means "go direct to github.com/releases/...". CN
# operators commonly swap to https://ghproxy.com/https://github.com/ or
# similar to get past the github rate-limit + bandwidth crawl. Also
# honoured via env: OPSKEEPER_GH_PROXY=...
GH_PROXY="${OPSKEEPER_GH_PROXY:-}"
usage() {
    cat <<EOF
Usage: sudo bash install-deps.sh [OPTIONS]

Options:
  --skip-grafana          Skip grafana repo + install. Useful when:
                          - apt.grafana.com is unreachable (CN networks).
                          - operator wants to install grafana later via a
                            distro mirror or out-of-band.
                          Manager still works without grafana —
                          dashboards just won't render.
  --apt-timeout <sec>     Hard cap (default 600) for each apt/dnf install.
                          Triggered via the timeout(1) wrapper.
  --gh-proxy <prefix>     Prefix applied in front of every github.com
                          download URL — for environments behind a slow /
                          rate-limited route to github. Examples:
                            --gh-proxy https://ghproxy.com/
                            --gh-proxy https://mirror.ghproxy.com/
                          The prefix must end with '/'. Also settable via
                          OPSKEEPER_GH_PROXY env.
  -h, --help              Print this help.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-grafana) SKIP_GRAFANA=1; shift ;;
        --apt-timeout) APT_TIMEOUT="${2:-}"; shift 2 ;;
        --apt-timeout=*) APT_TIMEOUT="${1#*=}"; shift ;;
        --gh-proxy) GH_PROXY="${2:-}"; shift 2 ;;
        --gh-proxy=*) GH_PROXY="${1#*=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) err "unknown flag: $1"; usage; exit 2 ;;
    esac
done

# Helper to prefix every github.com URL.
gh_url() {
    local u="$1"
    if [[ -n "$GH_PROXY" ]]; then
        # Strip trailing / from proxy if present, then re-attach as we
        # join.
        local p="${GH_PROXY%/}"
        printf '%s/%s' "$p" "$u"
    else
        printf '%s' "$u"
    fi
}

# -----------------------------------------------------------------------------
# Pinned upstream versions. These mirror what docker-compose.yml uses today,
# so behaviour is identical across modes. Bump in lock-step with the compose
# images so a re-package picks up upgrades.
# -----------------------------------------------------------------------------
PROM_VERSION=2.54.0
LOKI_VERSION=3.4.0
TEMPO_VERSION=2.5.0
QDRANT_VERSION=1.11.3

# Pinned sha256s. Verified against upstream release manifests at package time
# where upstream publishes them. If you bump a version above, update the
# sha256 values here and in dist/package.sh together.
HOST_ARCH="${OPSKEEPER_SYSTEMD_ARCH:-$(uname -m)}"
case "$HOST_ARCH" in
    x86_64|amd64)
        STACK_TARGET=linux-amd64
        PROM_ASSET="prometheus-${PROM_VERSION}.linux-amd64.tar.gz"
        PROM_EXTRACT_DIR="prometheus-${PROM_VERSION}.linux-amd64"
        PROM_SHA=465e1393a0cca9705598f6ffaf96ffa78d0347808ab21386b0c6aaec2cf7aa13
        LOKI_ASSET="loki-linux-amd64.zip"
        LOKI_BIN="loki-linux-amd64"
        LOKI_SHA=fb07349f21cc86eec1162d81f90ad2706280cd731eabc5456ecd8e21a5df8404
        TEMPO_ASSET="tempo_${TEMPO_VERSION}_linux_amd64.tar.gz"
        TEMPO_SHA=a708a86230fa43478e8a30174787a1171fbfdc33ad135ce1625769dbadc16e38
        QDRANT_ASSET="qdrant-x86_64-unknown-linux-gnu.tar.gz"
        QDRANT_SHA=4000a4924c118cc88296f879aad25bebb5869bb5baac7801bec8860a96396914
        ;;
    aarch64|arm64)
        STACK_TARGET=linux-arm64
        PROM_ASSET="prometheus-${PROM_VERSION}.linux-arm64.tar.gz"
        PROM_EXTRACT_DIR="prometheus-${PROM_VERSION}.linux-arm64"
        PROM_SHA=ed50b67cb833a225ec2a53b487c6e20372b20e56dce226423fa8611c8aa50392
        LOKI_ASSET="loki-linux-arm64.zip"
        LOKI_BIN="loki-linux-arm64"
        LOKI_SHA=0e5d9aa98ccfd7114c74e87201963fe70c0de0d051b8359dd7cafe37a9f2e492
        TEMPO_ASSET="tempo_${TEMPO_VERSION}_linux_arm64.tar.gz"
        TEMPO_SHA=4c96c11e4950541fcc190be620bf8551e8b2bc645fee0883464ac8a9b363f8d6
        QDRANT_ASSET="qdrant-aarch64-unknown-linux-musl.tar.gz"
        QDRANT_SHA=e164496afa9e4cacdd5679be550f735320e51b2e74d6ce6fbcb0b8260ed4c7d3
        ;;
    *)
        err "unsupported CPU architecture: $HOST_ARCH (supported: x86_64/amd64, aarch64/arm64)"
        exit 2
        ;;
esac
log "detected arch $STACK_TARGET"

PREFIX_BIN=/usr/local/bin
DOWNLOAD_DIR=/var/cache/opskeeper-install
mkdir -p "$DOWNLOAD_DIR"

# -----------------------------------------------------------------------------
# distro detect
# -----------------------------------------------------------------------------
PKG_MGR=
PKG_INSTALL=
PKG_UPDATE=
if command -v apt-get >/dev/null 2>&1; then
    PKG_MGR=apt
    PKG_INSTALL="apt-get install -y --no-install-recommends"
    PKG_UPDATE="apt-get update"
elif command -v dnf >/dev/null 2>&1; then
    PKG_MGR=dnf
    PKG_INSTALL="dnf install -y"
    PKG_UPDATE="dnf makecache"
elif command -v yum >/dev/null 2>&1; then
    PKG_MGR=yum
    PKG_INSTALL="yum install -y"
    PKG_UPDATE="yum makecache"
else
    err "no supported package manager found (apt / dnf / yum)"
    exit 2
fi
log "detected $PKG_MGR"

# -----------------------------------------------------------------------------
# step 1 — OS-package deps
# -----------------------------------------------------------------------------
log "installing OS-package deps: mariadb-server, nginx, grafana"
$PKG_UPDATE

# mariadb + nginx come from the distro repo unconditionally.
# libstdc++6 + libgcc-s1 are the runtime deps of libonnxruntime.so (the
# local embedder, installed below). Near-universal already, but listed so
# a minimal host doesn't fail the .so dlopen with a cryptic loader error.
case "$PKG_MGR" in
    apt)
        $PKG_INSTALL mariadb-server nginx ca-certificates curl gnupg unzip tar \
            libstdc++6 libgcc-s1
        ;;
    dnf|yum)
        $PKG_INSTALL mariadb-server nginx ca-certificates curl gnupg unzip tar \
            libstdc++ libgcc
        ;;
esac

# grafana — distro packages are usually stale; pull from grafana's repo.
# Failure here is non-fatal: manager + telemetry stack still work, only
# dashboards go missing. The apt.grafana.com endpoint is known to be
# flaky over some CN networks — we hard-cap the install so a hang
# doesn't wedge the whole install-deps run.
if (( SKIP_GRAFANA )); then
    warn "grafana install skipped (--skip-grafana)"
elif command -v grafana-server >/dev/null 2>&1; then
    log "grafana already present — skipping repo setup"
else
    log "adding grafana repo + installing grafana (timeout=${APT_TIMEOUT}s)"
    set +e
    case "$PKG_MGR" in
        apt)
            install -d -m 0755 /etc/apt/keyrings
            timeout "$APT_TIMEOUT" bash -c '
                curl -fsSL --max-time 30 https://apt.grafana.com/gpg.key \
                    | gpg --dearmor -o /etc/apt/keyrings/grafana.gpg &&
                echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" \
                    > /etc/apt/sources.list.d/grafana.list &&
                apt-get update &&
                apt-get install -y --no-install-recommends grafana
            '
            rc=$?
            ;;
        dnf|yum)
            cat > /etc/yum.repos.d/grafana.repo <<'EOF'
[grafana]
name=grafana
baseurl=https://rpm.grafana.com
repo_gpgcheck=1
enabled=1
gpgcheck=1
gpgkey=https://rpm.grafana.com/gpg.key
sslverify=1
sslcacert=/etc/pki/tls/certs/ca-bundle.crt
EOF
            timeout "$APT_TIMEOUT" $PKG_INSTALL grafana
            rc=$?
            ;;
    esac
    set -e
    if [[ $rc -ne 0 ]]; then
        warn "grafana install failed/timeout (rc=$rc) — continuing without it"
        warn "manager + telemetry stack will run; dashboards unavailable"
        warn "to install grafana later:"
        warn "  apt-get install grafana    # from grafana repo"
        warn "OR via a CN mirror:"
        warn "  https://mirrors.tuna.tsinghua.edu.cn/grafana/apt/"
        # remove the half-written sources list so apt-get update doesn't
        # break later from a broken repo entry.
        rm -f /etc/apt/sources.list.d/grafana.list 2>/dev/null || true
    fi
fi

# -----------------------------------------------------------------------------
# step 2 — upstream binary downloads
# -----------------------------------------------------------------------------
fetch_and_verify() {
    # fetch_and_verify <name> <url> <sha256> <dst-path-in-cache>
    local name="$1" url="$2" sha="$3" dst="$4"
    if [[ -f "$dst" ]]; then
        local actual
        actual=$(sha256sum "$dst" | awk '{print $1}')
        if [[ "$actual" == "$sha" ]]; then
            log "$name cached + sha256 ok"
            return 0
        fi
        warn "$name cached but sha256 mismatch — re-downloading"
        rm -f "$dst"
    fi
    log "downloading $name → $dst"
    curl -fsSL -o "$dst" "$url"
    local actual
    actual=$(sha256sum "$dst" | awk '{print $1}')
    if [[ "$actual" != "$sha" ]]; then
        err "$name sha256 mismatch! expected $sha got $actual"
        rm -f "$dst"
        return 1
    fi
    log "$name sha256 ok"
}

# install_bin <name> <src-in-extracted-dir> <dst-bin-name>
install_bin() {
    local name="$1" src="$2" dst_name="$3"
    if [[ ! -f "$src" ]]; then
        err "$name extracted but $src missing"
        return 1
    fi
    install -m 0755 -o root -g root "$src" "$PREFIX_BIN/$dst_name"
    log "installed $PREFIX_BIN/$dst_name"
}

# try_bundled <name> <dst-bin-name>
# Returns 0 + installs from the bundle when bin/stack-deps/<name> ships
# in the tarball; returns 1 otherwise. Lets release builds pre-bundle
# the four upstream binaries so an offline-network install needs zero
# github reach. Bundle layout matches what dist/package.sh writes when
# OPSKEEPER_BUNDLE_STACK_BINS=1 was set at package time.
try_bundled() {
    local name="$1" dst_name="$2"
    local cand="$SCRIPT_DIR/../bin/stack-deps/$name"
    local marker="$SCRIPT_DIR/../bin/stack-deps/ARCH"
    if [[ -f "$cand" ]]; then
        if [[ -f "$marker" ]] && [[ "$(tr -d '[:space:]' < "$marker")" != "$STACK_TARGET" ]]; then
            warn "bundled $name is for $(tr -d '[:space:]' < "$marker"), host is $STACK_TARGET — downloading host arch"
            return 1
        fi
        install -m 0755 -o root -g root "$cand" "$PREFIX_BIN/$dst_name"
        log "installed $PREFIX_BIN/$dst_name (from bundle)"
        return 0
    fi
    return 1
}

# --- prometheus ---
if ! try_bundled prometheus prometheus; then
    PROM_TGZ="$DOWNLOAD_DIR/$PROM_ASSET"
    fetch_and_verify prometheus \
        "$(gh_url https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${PROM_ASSET})" \
        "$PROM_SHA" "$PROM_TGZ"
    PROM_EXTRACT="$DOWNLOAD_DIR/$PROM_EXTRACT_DIR"
    rm -rf "$PROM_EXTRACT"
    tar -xzf "$PROM_TGZ" -C "$DOWNLOAD_DIR"
    install_bin prometheus "$PROM_EXTRACT/prometheus" prometheus
fi

# --- loki ---
if ! try_bundled loki loki; then
    LOKI_ZIP="$DOWNLOAD_DIR/$LOKI_ASSET"
    fetch_and_verify loki \
        "$(gh_url https://github.com/grafana/loki/releases/download/v${LOKI_VERSION}/${LOKI_ASSET})" \
        "$LOKI_SHA" "$LOKI_ZIP"
    rm -f "$DOWNLOAD_DIR/$LOKI_BIN"
    unzip -qo "$LOKI_ZIP" -d "$DOWNLOAD_DIR"
    install_bin loki "$DOWNLOAD_DIR/$LOKI_BIN" loki
fi

# --- tempo ---
if ! try_bundled tempo tempo; then
    TEMPO_TGZ="$DOWNLOAD_DIR/$TEMPO_ASSET"
    fetch_and_verify tempo \
        "$(gh_url https://github.com/grafana/tempo/releases/download/v${TEMPO_VERSION}/${TEMPO_ASSET})" \
        "$TEMPO_SHA" "$TEMPO_TGZ"
    TEMPO_EXTRACT="$DOWNLOAD_DIR/tempo-${TEMPO_VERSION}"
    rm -rf "$TEMPO_EXTRACT" && mkdir -p "$TEMPO_EXTRACT"
    tar -xzf "$TEMPO_TGZ" -C "$TEMPO_EXTRACT"
    install_bin tempo "$TEMPO_EXTRACT/tempo" tempo
fi

# --- qdrant ---
if ! try_bundled qdrant qdrant; then
    QDRANT_TGZ="$DOWNLOAD_DIR/$QDRANT_ASSET"
    fetch_and_verify qdrant \
        "$(gh_url https://github.com/qdrant/qdrant/releases/download/v${QDRANT_VERSION}/${QDRANT_ASSET})" \
        "$QDRANT_SHA" "$QDRANT_TGZ"
    QDRANT_EXTRACT="$DOWNLOAD_DIR/qdrant-${QDRANT_VERSION}"
    rm -rf "$QDRANT_EXTRACT" && mkdir -p "$QDRANT_EXTRACT"
    tar -xzf "$QDRANT_TGZ" -C "$QDRANT_EXTRACT"
    install_bin qdrant "$QDRANT_EXTRACT/qdrant" qdrant
fi

# --- libonnxruntime.so (local ONNX embedder, ADR-027 Phase-2) ---
# The opskeeper manager binary is the CGO build (fastembed-go → onnxruntime_go)
# and dlopens this .so at runtime via ONNX_PATH=/usr/lib/libonnxruntime.so
# (set in opskeeper.service). package.sh extracted it from the docker image
# into bin/; install it to /usr/lib + the SONAME symlinks + ldconfig so the
# loader resolves it. Without this, OPSKEEPER_EMBEDDING_PROVIDER=local fails to
# load the model. Compose mode bundles the .so inside the image instead.
ORT_SO=$(ls "$SCRIPT_DIR/../bin/"libonnxruntime.so.* 2>/dev/null | head -1 || true)
if [[ -n "$ORT_SO" && -f "$ORT_SO" ]]; then
    ort_base=$(basename "$ORT_SO")                 # libonnxruntime.so.1.20.1
    ort_ver="${ort_base#libonnxruntime.so.}"       # 1.20.1
    ort_major="libonnxruntime.so.${ort_ver%%.*}"   # libonnxruntime.so.1
    install -m 0755 "$ORT_SO" "/usr/lib/$ort_base"
    ln -sf "$ort_base"  "/usr/lib/$ort_major"       # libonnxruntime.so.1
    ln -sf "$ort_base"  "/usr/lib/libonnxruntime.so"
    ldconfig 2>/dev/null || true
    log "installed $ort_base → /usr/lib (+ symlinks); local embedder enabled"
else
    warn "libonnxruntime.so not bundled — OPSKEEPER_EMBEDDING_PROVIDER=local will"
    warn "  fail to load; use an API-key embedder or rebuild the package with"
    warn "  ONNXRUNTIME bundled (see dist/package.sh)."
fi

# -----------------------------------------------------------------------------
# step 3 — mariadb schema bootstrap
# -----------------------------------------------------------------------------
log "starting mariadb to bootstrap schema"
systemctl enable --now mariadb >/dev/null 2>&1 || \
    systemctl enable --now mariadb.service

# Wait for socket.
for _ in $(seq 1 30); do
    mysqladmin -uroot ping >/dev/null 2>&1 && break
    sleep 1
done

DB_PASS_FILE=/etc/opskeeper/db-password
if [[ -f "$DB_PASS_FILE" ]]; then
    DB_PASS=$(cat "$DB_PASS_FILE")
    log "reusing existing DB password from $DB_PASS_FILE"
else
    # head -c closes the pipe before tr finishes → SIGPIPE → pipefail kills
    # the whole script. Read a fixed-size buffer then map alpha-num so no
    # short-read is needed.
    DB_PASS=$(LC_ALL=C tr -dc 'A-Za-z0-9' < <(head -c 256 /dev/urandom) | cut -c1-24)
    mkdir -p /etc/opskeeper
    printf '%s' "$DB_PASS" > "$DB_PASS_FILE"
    chmod 0600 "$DB_PASS_FILE"
    chown root:opskeeper "$DB_PASS_FILE" 2>/dev/null || true
    log "generated DB password → $DB_PASS_FILE (0600)"
fi

mysql -uroot <<SQL
CREATE DATABASE IF NOT EXISTS opskeeper CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'opskeeper'@'localhost' IDENTIFIED BY '${DB_PASS}';
ALTER USER 'opskeeper'@'localhost' IDENTIFIED BY '${DB_PASS}';
GRANT ALL PRIVILEGES ON opskeeper.* TO 'opskeeper'@'localhost';
FLUSH PRIVILEGES;
SQL
log "mariadb schema bootstrapped (db=opskeeper user=opskeeper)"

# Auto-write DSN into opskeeper.env if it still has the placeholder
ENV_FILE=/etc/opskeeper/opskeeper.env
if [[ -f "$ENV_FILE" ]] && grep -q 'CHANGE_ME' "$ENV_FILE"; then
    sed -i "s|opskeeper:CHANGE_ME@|opskeeper:${DB_PASS}@|" "$ENV_FILE"
    log "updated OPSKEEPER_DB_DSN in $ENV_FILE"
fi

# -----------------------------------------------------------------------------
# step 4 — grafana datasource provisioning
# -----------------------------------------------------------------------------
# Only when grafana is actually installed — otherwise the group doesn't
# exist and the install(1) call fails. install-deps.sh skips grafana
# when --skip-grafana is passed or the apt fetch times out, so checking
# the group is the right signal.
if getent group grafana >/dev/null && [[ -f "$SCRIPT_DIR/grafana-provisioning/datasources.yaml" ]]; then
    install -d -m 0755 /etc/grafana/provisioning/datasources
    install -m 0640 -o root -g grafana "$SCRIPT_DIR/grafana-provisioning/datasources.yaml" \
        /etc/grafana/provisioning/datasources/opskeeper.yaml
    log "wrote grafana datasource provisioning"
elif [[ -f "$SCRIPT_DIR/grafana-provisioning/datasources.yaml" ]]; then
    warn "grafana not installed — datasource provisioning will be applied"
    warn "when you install grafana later; copy this file then:"
    warn "  $SCRIPT_DIR/grafana-provisioning/datasources.yaml"
    warn "  → /etc/grafana/provisioning/datasources/opskeeper.yaml"
fi

# -----------------------------------------------------------------------------
# step 4b — grafana server config (systemd drop-in)
# -----------------------------------------------------------------------------
# The distro grafana ships with anonymous auth OFF, sub-path serving OFF,
# and the default org role at Viewer. Behind opskeeper's nginx (which gates
# /grafana/* via auth_request on the opskeeper session) that breaks two
# things: (1) the /grafana/ reverse proxy needs serve_from_sub_path +
# root_url, and (2) Explore is Editor/Admin-gated, so a Viewer hitting
# /grafana/explore?... gets 302'd to the Grafana home — exactly the
# "日志/链路在 Grafana 打开没数据" symptom. Mirror the proven docker-compose
# grafana env (deploy/install/docker-compose.yml) via a systemd drop-in so
# both install modes behave identically.
#
# %% escapes the literal % for systemd (so grafana receives %(protocol)s).
if getent group grafana >/dev/null; then
    install -d -m 0755 /etc/systemd/system/grafana-server.service.d
    cat > /etc/systemd/system/grafana-server.service.d/10-opskeeper.conf <<'EOF'
[Service]
Environment=GF_SERVER_SERVE_FROM_SUB_PATH=true
Environment=GF_SERVER_ROOT_URL=%%(protocol)s://%%(domain)s/grafana/
Environment=GF_AUTH_ANONYMOUS_ENABLED=true
Environment=GF_AUTH_ANONYMOUS_ORG_ROLE=Editor
Environment=GF_ANALYTICS_REPORTING_ENABLED=false
Environment=GF_ANALYTICS_CHECK_FOR_UPDATES=false
EOF
    systemctl daemon-reload 2>/dev/null || true
    if systemctl is-active --quiet grafana-server 2>/dev/null; then
        systemctl restart grafana-server 2>/dev/null || true
        log "wrote grafana-server drop-in (sub-path + anonymous Editor) and restarted"
    else
        log "wrote grafana-server drop-in (sub-path + anonymous Editor)"
    fi
fi

# -----------------------------------------------------------------------------
# step 5 — nginx site config
# -----------------------------------------------------------------------------
if [[ -f "$SCRIPT_DIR/nginx-opskeeper.conf" ]]; then
    install -d -m 0755 /etc/nginx/conf.d
    install -m 0644 "$SCRIPT_DIR/nginx-opskeeper.conf" /etc/nginx/conf.d/opskeeper.conf
    log "wrote /etc/nginx/conf.d/opskeeper.conf — reload nginx after edits"
fi

# -----------------------------------------------------------------------------
# finish
# -----------------------------------------------------------------------------
cat <<EOF

${C_BOLD}${C_GREEN}deps install complete${C_RESET}

Stack-dep binaries:
  $(ls -l $PREFIX_BIN/{prometheus,loki,tempo,qdrant} 2>/dev/null | awk '{print "  ", $NF, $5"B"}')

OS packages:
  mariadb-server  $(systemctl is-active mariadb 2>/dev/null || echo not-running)
  nginx           $(systemctl is-active nginx 2>/dev/null    || echo not-running)
  grafana         $(systemctl is-active grafana-server 2>/dev/null || echo not-running)

Next:
  sudo systemctl start prometheus loki tempo qdrant
  sudo systemctl start opskeeper-frontier opskeeper
  sudo systemctl enable --now nginx grafana-server   # serve UI on :443
EOF
