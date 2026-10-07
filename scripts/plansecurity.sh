#!/usr/bin/env bash
# The plan's section 6 security block, one named assertion at a time.
#
# `go test -run` exits 0 when the name matches nothing. That single fact is the
# difference between a gate and a comment: rename one of the tests below and a
# gate written out of test names keeps passing while asserting nothing at all.
# Decision 348 recorded the opposite failure -- a gate that was permanently red
# and therefore owned nothing. This is the same disease with the opposite
# symptom, and it needs the opposite guard, so each name is checked for having
# matched something before its result is believed.
#
# The four lines are the plan's, verbatim in intent:
#   1. the three fence cases of 1.3 (replay, lease expiry, a sibling call)
#   2. a node's credential must not drive another node's inference
#   3. the three autonomy escapes (tampered argv, over-wide radius, ceiling)
#   4. the coverage axis, run by the Makefile through eval-coverage
set -euo pipefail

cd "$(dirname "$0")/.."

run_named() {
	local dir="$1" pkg="$2" names="$3" label="$4"
	local name out passed
	# Each name is run on its own. Checking that the group matched something
	# would let a rename hide behind its two still-correct siblings, which is
	# the same silent-green failure one level down: three assertions were
	# named and the gate has to be able to say it ran three.
	IFS='|' read -r -a split <<<"$names"
	for name in "${split[@]}"; do
		out="$(cd "$dir" && GOWORK=off go test "$pkg" -count=1 -v -run "^${name}\$" 2>&1)" || {
			echo "$out"
			echo "plan-security-check: $label -- $name failed"
			exit 1
		}
		passed="$(printf '%s' "$out" | grep -c '^--- PASS' || true)"
		if [ "$passed" -lt 1 ]; then
			echo "plan-security-check: $label has no test named $name."
			echo "  The assertion was renamed or deleted, and without this check the gate"
			echo "  would have gone on asserting nothing while reporting success."
			exit 1
		fi
	done
	echo "plan-security-check: $label ran ${#split[@]} named assertion(s)"
}

run_named core/edge ./policygate/ \
	'TestTheSameCallSubmittedEightTimesIsOneQuestionAndOneExecution|TestAGrantIsCollectableInsideItsLeaseAndNotAfterIt|TestASecondMutatingCallInOneConversationWaitsRatherThanQueuing' \
	'the fence (replay, lease expiry, a sibling call)'

run_named core/domains ./server/llmgw/ \
	'TestANodesCredentialCannotDriveAnotherNodesInference' \
	"a node's credential against another node's inference"

run_named core/edge ./autonomy/ \
	'TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed|TestOnlyDeclaredRadiiRun|TestTheCeilingIsEnforcedHereAndNotOnlyAtAdmission' \
	'the three autonomy escapes (argv, radius, ceiling)'
