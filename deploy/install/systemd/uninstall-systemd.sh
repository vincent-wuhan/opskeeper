#!/usr/bin/env bash
# opskeeper pure-systemd uninstaller. Mirror of install-systemd.sh.
#
# Default: stop + disable + remove unit files; preserve data dirs + env.
# --purge: also nuke /var/lib/opskeeper* and the service users.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [[ -t 1 ]]; then
    C_RED=$'\033[0;31m'; C_GREEN=$'\033[0;32m'; C_YELLOW=$'\033[1;33m'
    C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
else
    C_RED=''; C_GREEN=''; C_YELLOW=''; C_BOLD=''; C_RESET=''
fi

log()  { printf '%s[INFO]%s %s\n'  "$C_GREEN"  "$C_RESET" "$*"; }
warn() { printf '%s[WARN]%s %s\n'  "$C_YELLOW" "$C_RESET" "$*"; }
err()  { printf '%s[ERROR]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }

PURGE=0
ASSUME_YES=0
usage() {
    cat <<EOF
Usage: sudo uninstall-systemd.sh [OPTIONS]

Options:
  --purge   Also delete /var/lib/opskeeper* + /var/log/opskeeper + service users.
            Manager + dep data (DB, vectors, metrics, logs) is lost.
  --yes     Skip the confirmation prompt (only with --purge).
  -h        Print this help.

Without --purge, units are stopped + removed but data + users remain so a
later install-systemd.sh resumes where you left off.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge) PURGE=1; shift ;;
        --yes|-y) ASSUME_YES=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) err "unknown flag: $1"; usage; exit 2 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    err "must run as root (sudo)"
    exit 1
fi

UNITS=(opskeeper.service opskeeper-frontier.service \
       prometheus.service loki.service tempo.service qdrant.service)
SYSTEMD_DIR=/etc/systemd/system

# -----------------------------------------------------------------------------
# stop + disable
# -----------------------------------------------------------------------------
for u in "${UNITS[@]}"; do
    if systemctl is-active --quiet "$u" 2>/dev/null; then
        systemctl stop "$u" || warn "stop $u failed"
        log "stopped $u"
    fi
    if systemctl is-enabled --quiet "$u" 2>/dev/null; then
        systemctl disable "$u" >/dev/null 2>&1 || warn "disable $u failed"
        log "disabled $u"
    fi
done

# -----------------------------------------------------------------------------
# stragglers — manager + frontier might be wedged outside systemd's view
# -----------------------------------------------------------------------------
for proc in /usr/local/bin/opskeeper /usr/local/bin/opskeeper-frontier; do
    pids=$(pgrep -f "^$proc" 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
        warn "killing straggler $proc (pids: $pids)"
        kill -TERM $pids 2>/dev/null || true
        sleep 2
        pids=$(pgrep -f "^$proc" 2>/dev/null || true)
        if [[ -n "$pids" ]]; then
            warn "force-killing $proc (pids: $pids)"
            kill -KILL $pids 2>/dev/null || true
        fi
    fi
done

# -----------------------------------------------------------------------------
# unit files + binaries
# -----------------------------------------------------------------------------
for u in "${UNITS[@]}"; do
    if [[ -f "$SYSTEMD_DIR/$u" ]]; then
        rm -f "$SYSTEMD_DIR/$u"
        log "removed $SYSTEMD_DIR/$u"
    fi
done
for bin in opskeeper opskeeper-frontier; do
    if [[ -f "/usr/local/bin/$bin" ]]; then
        rm -f "/usr/local/bin/$bin"
        log "removed /usr/local/bin/$bin"
    fi
done
systemctl daemon-reload

# -----------------------------------------------------------------------------
# stop-only short-circuit
# -----------------------------------------------------------------------------
if [[ $PURGE -eq 0 ]]; then
    echo ""
    echo "${C_BOLD}${C_GREEN}stop-only uninstall complete${C_RESET}"
    echo "  - units stopped + removed"
    echo "  - data dirs preserved (/var/lib/opskeeper*, /var/log/opskeeper)"
    echo "  - service users preserved (opskeeper, opskeeper-prometheus, ...)"
    echo "  - configs preserved (/etc/opskeeper/)"
    echo ""
    echo "Re-install with: sudo bash install-systemd.sh"
    echo "Wipe data with:  sudo bash uninstall-systemd.sh --purge"
    exit 0
fi

# -----------------------------------------------------------------------------
# purge — confirm + delete
# -----------------------------------------------------------------------------
if [[ $ASSUME_YES -eq 0 ]]; then
    printf "%sThis deletes ALL opskeeper data:\n" "$C_YELLOW"
    printf "  - /var/lib/opskeeper* (manager state, prom TSDB, loki/tempo store, qdrant vectors)\n"
    printf "  - /var/log/opskeeper (all logs)\n"
    printf "  - /etc/opskeeper (configs, secrets)\n"
    printf "  - service users (opskeeper, opskeeper-prometheus, opskeeper-loki, opskeeper-tempo, opskeeper-qdrant)\n"
    printf "Continue? [y/N] %s" "$C_RESET"
    read -r answer
    case "$answer" in
        y|Y|yes|YES) ;;
        *) log "aborted"; exit 0 ;;
    esac
fi

for d in /var/lib/opskeeper /var/lib/opskeeper-prometheus /var/lib/opskeeper-loki \
         /var/lib/opskeeper-tempo /var/lib/opskeeper-qdrant /var/log/opskeeper \
         /etc/opskeeper; do
    if [[ -d "$d" ]]; then
        rm -rf "$d"
        log "removed $d"
    fi
done

for u in opskeeper opskeeper-prometheus opskeeper-loki opskeeper-tempo opskeeper-qdrant; do
    if id "$u" &>/dev/null; then
        userdel "$u" 2>/dev/null || warn "userdel $u failed"
        log "removed user $u"
    fi
done

echo ""
echo "${C_BOLD}${C_GREEN}purge complete${C_RESET}"
echo "  - units removed"
echo "  - data dirs removed"
echo "  - service users removed"
echo ""
echo "Note: OS-package deps (mariadb-server, nginx, grafana, the prom/loki/"
echo "tempo/qdrant binaries you may have placed in /usr/local/bin) were NOT"
echo "touched. Remove with your package manager if no longer needed."
