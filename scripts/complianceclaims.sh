#!/usr/bin/env bash
# What the Data-Guard vocabulary promises versus what the tree enforces.
#
# Same discipline as plansecurity.sh: `go test -run` exits 0 when the name
# matches nothing, so every name is run alone and its result is only believed
# after the run is shown to have matched something. A gate written out of test
# names must be able to say how many of them it ran.
#
# The registry these assertions read is core/manager/dataguard/enforcement.go.
# It answers one question per promise — enforced, inert, or declared — and the
# assertions here are what make the answer an observation rather than an
# opinion.
set -euo pipefail

cd "$(dirname "$0")/.."

names=(
	TestEveryEnforcedClaimIsReachableFromProductionCode
	TestEveryInertClaimIsStillUnreachableFromProductionCode
	TestNoDeclaredClaimNamesAnImplementation
	TestEveryAdvertisedControlIsClassified
	TestEverySensitivityLevelThatPromisesAControlIsClassified
	TestEveryClaimSaysWhatIsActuallyTrue
	TestADeclaredRowNamesNoFunctionAnywhereInTheTree
)

for name in "${names[@]}"; do
	out="$(GOWORK=off go test ./cmd/opskeeper/ -count=1 -v -run "^${name}\$" 2>&1)" || {
		echo "$out"
		echo "compliance-claims-check: $name failed"
		exit 1
	}
	passed="$(printf '%s' "$out" | grep -c '^--- PASS' || true)"
	if [ "$passed" -lt 1 ]; then
		echo "compliance-claims-check: no test is named $name."
		echo "  The assertion was renamed or deleted, and without this check the gate would"
		echo "  have gone on reporting success while asserting nothing."
		exit 1
	fi
done

echo "compliance-claims-check: ${#names[@]} named assertion(s) ran; every Data-Guard promise still matches the tree"
