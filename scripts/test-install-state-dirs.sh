#!/usr/bin/env bash
# Focused tests for deploy/install/state-dirs.sh and the two scripts that use it.
#
# Why this exists: the host data directories are the one thing in the install
# path where getting it wrong produces a stack that looks healthy and cannot
# write. A missing chown is not an error — `|| true` hides it, `docker compose
# up` then creates the directory as root, and the nonroot process fails later
# at "mkdir page dir: permission denied" or worse, silently forgets a
# federation membership and re-enrols every cluster. Nothing in the install
# output says so.
#
# These tests run entirely in a sandbox: no docker, no root, no network. chown
# is intercepted rather than performed, because the test needs to assert the
# ORDER of the three steps and only a fake can observe it — a real chown of a
# directory that does not exist fails, and the failure is exactly what the
# production code swallows.
#
# Run: bash scripts/test-install-state-dirs.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIRS="${REPO_ROOT}/deploy/install/state-dirs.sh"
INSTALL_SH="${REPO_ROOT}/deploy/install/install.sh"
UPGRADE_SH="${REPO_ROOT}/deploy/install/upgrade.sh"
PACKAGE_SH="${REPO_ROOT}/dist/package.sh"

WORK="$(mktemp -d -t statedirs-test-XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

fails=0
pass() { printf 'PASS: %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; fails=$((fails + 1)); }

# --- a sandbox where chown and chmod are observable -------------------------
#
# Both record every invocation to a log. chown additionally records whether
# the directory it was handed already existed: that single fact is the whole
# difference between an install that works and one that silently writes
# nothing, and it is invisible from the outside of a real chown.
build_sandbox() {
    mkdir -p "$WORK/bin"
    cat > "$WORK/bin/chown" <<'EOF'
#!/usr/bin/env bash
target="${@: -1}"
if [[ ! -d "$target" ]]; then
    printf 'MISSING %s\n' "$*" >> "$FAKE_CHOWN_LOG"
else
    printf 'OK %s\n' "$*" >> "$FAKE_CHOWN_LOG"
fi
exit 0
EOF
    cat > "$WORK/bin/chmod" <<'EOF'
#!/usr/bin/env bash
printf 'CHMOD %s\n' "$*" >> "$FAKE_CHMOD_LOG"
exit 0
EOF
    chmod +x "$WORK/bin/chown" "$WORK/bin/chmod"
    : > "$WORK/chown.log"
    : > "$WORK/chmod.log"
    export FAKE_CHOWN_LOG="$WORK/chown.log" FAKE_CHMOD_LOG="$WORK/chmod.log"
}

# --- 1. the file parses ------------------------------------------------------
if bash -n "$STATE_DIRS" 2>"$WORK/syntax.log"; then
    pass "state-dirs.sh parses"
else
    fail "state-dirs.sh does not parse: $(cat "$WORK/syntax.log")"
fi

# --- 2. the list itself is well-formed --------------------------------------
# shellcheck source=/dev/null
source "$STATE_DIRS"
LIST="$WORK/list.txt"
opskeeper_state_dirs > "$LIST"

if [[ -s "$LIST" ]]; then
    pass "opskeeper_state_dirs emits a non-empty list"
else
    fail "opskeeper_state_dirs emitted nothing"
fi

bad_rows="$(awk 'NF < 2 || NF > 3 { print NR": "$0 }' "$LIST")"
if [[ -z "$bad_rows" ]]; then
    pass "every row has a directory and a uid (and at most a mode)"
else
    fail "rows with the wrong column count: ${bad_rows}"
fi

# n:n and nothing else. A "-" — "create it, do not chown it" — used to be
# allowed here and it made the purge question unanswerable from this file:
# opskeeper_purge_dirs decides what uninstall.sh deletes by comparing the uid
# column against the manager's, and "-" is not the manager's, so a directory
# marked "-" is a service's data for uninstall and an ownerless directory for
# install. qdrant carried exactly that "-" until it was written 0:0, which
# produces the same result on disk (install.sh runs as root) and answers the
# question. Re-introducing "-" has to fail here rather than re-introduce the
# ambiguity.
bad_uid="$(awk 'NF >= 2 && $2 !~ /^[0-9]+:[0-9]+$/ { print NR": "$0 }' "$LIST")"
if [[ -z "$bad_uid" ]]; then
    pass "every uid column is n:n — no ownerless third kind"
else
    fail "rows whose uid column is not n:n: ${bad_uid}"
fi

dupes="$(awk '{ print $1 }' "$LIST" | sort | uniq -d)"
if [[ -z "$dupes" ]]; then
    pass "no directory is listed twice"
else
    fail "directories listed more than once: ${dupes}"
fi

# --- 3. running it creates every directory ----------------------------------
DATA="$WORK/data"
build_sandbox
opskeeper_state_dirs | PATH="$WORK/bin:$PATH" opskeeper_ensure_state_dirs "$DATA"

missing=""
while read -r dir _; do
    [[ -d "$DATA/$dir" ]] || missing+=" $dir"
done < <(opskeeper_state_dirs)
if [[ -z "$missing" ]]; then
    pass "every listed directory exists after a run"
else
    fail "directories not created:${missing}"
fi

# --- 4. THE ONE THAT MATTERS: chown never sees a missing directory ----------
#
# This is the assertion the whole file exists for. Chown runs after mkdir in
# the source, and a fake is the only thing that can tell: a real chown of a
# missing path exits non-zero, the caller swallows that with `|| true`, and
# the install continues into a directory docker will create as root.
ordered_bad="$(grep -c '^MISSING' "$WORK/chown.log" || true)"
if [[ "$ordered_bad" -eq 0 ]]; then
    pass "every chown ran against a directory that already existed"
else
    fail "$ordered_bad chown call(s) ran before the directory existed — mkdir and chown are in the wrong order"
fi

# --- 5. each chown names the right uid --------------------------------------
wrong_uid="$(grep '^OK' "$WORK/chown.log" | while read -r _ rest; do
    dir="${rest##* }"
    want="$(awk -v d="$dir" '$1 == d { print $2 }' "$LIST")"
    case "$rest" in
        *"$want "*) ;;
        *) printf '%s wanted %s\n' "$rest" "$want" ;;
    esac
done)"
if [[ -z "$wrong_uid" ]]; then
    pass "each chown carried the uid the list gives that directory"
else
    fail "chowns with the wrong uid: ${wrong_uid}"
fi

# --- 6. every directory is chowned, and none of them is the manager's by
#        accident. An earlier revision carried a "-" for "create but do not
#        chown" and that third kind made the purge question unanswerable from
#        the list, because qdrant runs as root and "-" said nothing about whose
#        data it holds. Writing 0:0 says both things and produces the same
#        result, because install.sh runs as root.
all_chowned="$(wc -l < "$WORK/chown.log" | tr -d ' ')"
if [[ "$all_chowned" -eq "$(grep -c '[^[:space:]]' "$LIST")" ]]; then
    pass "every listed directory was chowned, including the root-owned one"
else
    fail "$all_chowned chowns for $(grep -c '[^[:space:]]' "$LIST") directories"
fi
# The uid precedes the path in a chown argument list, so the pattern has to
# as well. Writing 'qdrant 0:0' here made the assertion fail against a log
# that said `OK -R 0:0 .../qdrant` — the very thing it was checking for.
if grep -E '^OK -R 0:0 .*/qdrant$' "$WORK/chown.log" >/dev/null; then
    pass "qdrant is chowned to root, the uid its container runs as"
else
    fail "qdrant was not chowned to 0:0 (log: $(grep qdrant "$WORK/chown.log" || echo none))"
fi

# --- 7. the mode column reaches chmod ----------------------------------------
if grep -q '0755' "$WORK/chmod.log" && grep -q 'embeddings' "$WORK/chmod.log"; then
    pass "embeddings' mode reached chmod"
else
    fail "embeddings' 0755 mode never reached chmod (log: $(cat "$WORK/chmod.log"))"
fi

# --- 8. a row with no uid is an error, not a silent skip ---------------------
# A directory created with nobody owning it is the failure this all prevents,
# so a malformed row must stop the install rather than pass through.
if printf 'orphan\n' | PATH="$WORK/bin:$PATH" opskeeper_ensure_state_dirs "$WORK/data2" 2>"$WORK/orphan.log"; then
    fail "a row with no uid column was accepted"
else
    pass "a row with no uid column is refused"
fi

# --- 9. both install paths actually use it -----------------------------------
for script in "$INSTALL_SH" "$UPGRADE_SH"; do
    name="$(basename "$script")"
    if grep -q 'source "\$SCRIPT_DIR/state-dirs.sh"' "$script" \
       && grep -q 'opskeeper_ensure_state_dirs "\$OPSKEEPER_DATA_DIR"' "$script"; then
        pass "$name sources the shared list and runs it"
    else
        fail "$name does not source and run the shared list — it may still carry its own copy of the directories"
    fi
    # The old shape was a `mkdir -p` list naming each directory inline. If
    # one is back, the single-source property is gone even though the new
    # call is still there.
    leftovers="$(grep -c 'OPSKEEPER_DATA_DIR/[a-z]' "$script" || true)"
    if [[ "$leftovers" -eq 0 ]]; then
        pass "$name names no data directory inline any more"
    else
        fail "$script still names $leftovers data director(ies) inline — two lists will drift again"
    fi
done

# --- 10. the tarball carries it, or nothing installs -------------------------
# copy_opt warns and continues on a missing source, which is right for an
# optional asset and wrong for a file two scripts `source` under `set -e`.
if grep -q 'die "deploy/install/state-dirs.sh missing' "$PACKAGE_SH"; then
    pass "package.sh treats state-dirs.sh as required, not optional"
else
    fail "package.sh does not fail when state-dirs.sh is absent — a tarball without it cannot install"
fi
if grep 'deploy/install/state-dirs.sh' "$PACKAGE_SH" | grep -q 'copy_opt'; then
    fail "package.sh uses copy_opt (warn-and-continue) for a required file"
else
    pass "package.sh does not use the lenient copy_opt for it"
fi


# --- 11. the purge list, and the uninstall path that consumes it -------------
#
# This is the second consumer of the same file, and the reason the list has a
# uid column that means two things. uninstall.sh deletes a service's own
# persistent data on --purge and keeps everything the manager wrote, because
# an operator parks other state under the data root and a re-install reuses
# it. The two used to be separate hand-written lists that agreed on six
# entries by hand, and their drifting produces the failure uninstall.sh's own
# comment records from 2026-05-20: a fresh install finds the previous mysql
# data directory, which still holds the old password, while the new .env gets
# a new one, and the manager crashloops on "Access denied for user".
PURGE="$WORK/purge.txt"
opskeeper_purge_dirs > "$PURGE"

# Compared as a SET, not as a line. The order comes from the list's own line
# order, `rm -rf` does not care about it, and asserting it would make this fail
# on a reordering that changes nothing — a test that cries wolf is worse than
# no test, because the next real failure gets read as the same noise.
if [[ "$(sort "$PURGE" | tr '\n' ' ' | sed 's/ $//')" == "grafana loki mysql prometheus qdrant tempo" ]]; then
    pass "the purge list is the six service data directories, unchanged"
else
    fail "purge list is '$(sort "$PURGE" | tr '\n' ' ')', want 'grafana loki mysql prometheus qdrant tempo'"
fi

# `comm` exits non-zero when the two sets are disjoint — which is the case this
# assertion is asserting — and `set -e` reads that as a failure and exits the
# whole script before the verdict is printed. The same trap is commented in
# state-dirs.sh and I walked into it here anyway: the failure is SILENT, so
# "the script printed no verdict" was the only symptom.
#
# `grep -F -x -f` is used instead of comm for the same reason it behaves here:
# no common lines is exit 1 with no output, and the || true is where that stops
# being a problem.
manager_leak=""
while read -r d; do
    if [[ "$(awk -v d="$d" '$1 == d { print $2 }' "$LIST")" == "65532:65532" ]] \
       && grep -qxF "$d" "$PURGE"; then
        manager_leak+="$d "
    fi
done < <(awk '{ print $1 }' "$LIST")
if [[ -z "$manager_leak" ]]; then
    pass "no manager-owned directory appears in the purge list"
else
    fail "purge would delete manager state:${manager_leak}"
fi

# The failure this whole file exists for, as a hardcoded list: a --purge that
# keeps a service's data is what leaves the previous install's mysql dir in
# place, so the list must come from the one source and not from a literal.
if grep -q 'opskeeper_purge_dirs' "$REPO_ROOT/deploy/install/uninstall.sh"; then
    pass "uninstall.sh derives its purge list from this file"
else
    fail "uninstall.sh does not use opskeeper_purge_dirs"
fi
hardcoded="$(grep -cE 'for d in (mysql|grafana)' "$REPO_ROOT/deploy/install/uninstall.sh" || true)"
if [[ "$hardcoded" -eq 0 ]]; then
    pass "uninstall.sh carries no hardcoded copy of the purge list"
else
    fail "uninstall.sh still hardcodes the purge list — the two will drift"
fi
# And it must not guess the list when the file is absent: guessing is exactly
# how the 2026-05-20 failure happens, and uninstall has no fallback to offer.
if grep -q 'state-dirs.sh is missing' "$REPO_ROOT/deploy/install/uninstall.sh" \
   && grep -q 'exit 1' "$REPO_ROOT/deploy/install/uninstall.sh"; then
    pass "uninstall.sh stops rather than guessing when the list is absent"
else
    fail "uninstall.sh does not stop when state-dirs.sh is missing"
fi

printf '\n%s\n' "$([[ $fails -eq 0 ]] && echo "all state-dir tests passed" || echo "$fails test(s) failed")"
[[ $fails -eq 0 ]]
