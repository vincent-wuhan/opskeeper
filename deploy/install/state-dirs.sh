#!/usr/bin/env bash
# The host directories bind-mounted into the stack, and the uid each one's
# container process runs as. install.sh and upgrade.sh both source this; it is
# the only place the list is written down.
#
# It is a script and not a data file because the thing that is easy to get
# wrong is not the list, it is the ORDER of the three steps. A chown of a
# directory that does not exist yet is a no-op that `|| true` hides; the
# directory is then created by `docker compose up` as root — so the mount is
# present, the manifest is right, nothing anywhere errors, and the nonroot
# process cannot write. The only order that produces a working install is
# mkdir, then chown, then chmod, and an install that gets the order wrong is
# indistinguishable from one that has no directories at all.
#
# The list itself was three times over — a mkdir block, a manager chown block
# and a per-service chown block — and the three had already drifted: upgrade.sh
# created pages/workspace/tools and install.sh did not. That drift is the
# whole argument for this file.

# opskeeper_ensure_state_dirs <data-dir> [<conf>]
#
# Creates every directory under <data-dir> and gives it the uid its container
# process runs as, reading the list from <conf> or from stdin when <conf> is
# omitted. Safe to re-run: it is what makes an upgrade re-assert ownership
# after an operator has deleted or renamed a directory.
#
# The list is piped in rather than passed as a path because the caller cannot
# write a file into the data dir before this has run — making the data dir is
# part of what this does. An earlier revision staged the list at
# "$DATA_DIR/.state-dirs.list" and therefore needed a mkdir that the function
# was supposed to be performing.
opskeeper_ensure_state_dirs() {
    local data_dir="$1" conf="${2:-/dev/stdin}"
    local dir uid mode

    while read -r dir uid mode || [[ -n "$dir" ]]; do
        # A comment or a blank line, tested with `case` rather than
        # `[[ … ]] && continue` because the latter returns non-zero on a
        # false test and `set -e` in the callers would read that as an error.
        case "$dir" in
            ''|'#'*) continue ;;
        esac
        if [[ -z "${uid:-}" ]]; then
            printf 'opskeeper_ensure_state_dirs: %s: %q has no uid column\n' "$conf" "$dir" >&2
            return 1
        fi

        mkdir -p "$data_dir/$dir"
        if [[ "$uid" != "-" ]]; then
            # `|| true` on purpose: a chown needs root, and a non-root re-run
            # of the install should still create the tree and carry on.
            chown -R "$uid" "$data_dir/$dir" 2>/dev/null || true
        fi
        if [[ -n "${mode:-}" ]]; then
            chmod -R "$mode" "$data_dir/$dir" 2>/dev/null || true
        fi
    done < "$conf"
}

# Format: <dir-under-data-dir>  <uid:gid>  [chmod-mode]
#
# The uid column decides two things at once, and that is deliberate. It is
# the owner install gives the directory, and it is also what tells uninstall.sh
# whether the directory is the manager's (keep — an operator parks other state
# under the data root and a re-install reuses it) or a service's own persistent
# data (purge). An earlier revision carried a `-` for "create but do not chown"
# and that third kind broke the question: qdrant runs as root, so `-` said
# nothing about whether its volumes were the manager's or its own, and the two
# readings had to be settled by reading uninstall.sh. Writing 0:0 says the
# same thing as `-` did in practice — the directory ended up root-owned
# either way, because install.sh runs as root — and now it is answerable.
#
# embeddings carries a mode because fastembed-go reads the staged model and
# needs it world-readable. Bumping an image tag in docker-compose.yml without
# updating the uid here fails on first boot: chown to the wrong uid, service
# cannot write.
opskeeper_state_dirs() {
    cat <<'LIST'
embeddings   65532:65532 0755
skills       65532:65532
pages        65532:65532
workspace    65532:65532
tools        65532:65532
repos        65532:65532
plugins      65532:65532
federation   65532:65532
crystallize  65532:65532
mysql        999:999
prometheus   65534:65534
loki         10001:10001
tempo        10001:10001
grafana      472:472
qdrant       0:0
LIST
}

# opskeeper_manager_uid is the uid Dockerfile.opskeeper gives the state root
# and then runs as. Every directory in that uid is the manager's own state.
opskeeper_manager_uid() { printf '65532:65532\n'; }

# opskeeper_purge_dirs lists the directories uninstall.sh deletes: every one
# that is NOT the manager's, because those hold a service's own persistent
# data and keeping them is the bug a re-install walks into.
#
# The rule used to live only as a hand-written list inside uninstall.sh, with
# nothing connecting it to this file. The two agreed by hand on six entries,
# and the consequence of them drifting is the incident uninstall.sh's own
# comment records from 2026-05-20: a fresh install picks up the previous mysql
# data directory, which still holds the old password, while the new .env gets
# a new one, and the manager crashloops on "Access denied for user
# 'opskeeper'@…". Nothing about that failure points at this list.
opskeeper_purge_dirs() {
    local manager uid
    manager="$(opskeeper_manager_uid)"
    local dir rest
    while read -r dir rest; do
        case "$dir" in
            ''|'#'*) continue ;;
        esac
        uid="${rest%% *}"
        if [[ "$uid" != "$manager" ]]; then
            printf '%s\n' "$dir"
        fi
    done < <(opskeeper_state_dirs)
}
