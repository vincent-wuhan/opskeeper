package ledgercheck

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestEveryPlanCapabilityTheLedgerTracksIsNamedInTheProgressSection is the
// seventeenth gate, and it exists because of a capability nothing was
// tracking.
//
// The plan's section 5 says stage 3 delivers "多集群/多租户规模化" — two
// pillars. The multi-cluster half was built (decision 123 onward: five
// packages, a production wiring, 0.97). The multi-tenant half was never
// looked at, and when decision 197 finally looked, it turned out that the
// three places that carry a tenant each carry it in a private context key —
// or, in the tool path, not in a context at all — so there is no shared
// mechanism to read and `patternTenantFromCtx` is a stub that ignores its
// argument.
//
// None of that was visible in the progress tables, because stage 3's row is
// about "控制面瘦身与联邦" and multi-tenancy is in neither. A capability the
// plan names and no table tracks has an invisible progress value, and an
// invisible value cannot go red.
//
// So the list below is a list of things the plan names. It is short, it is
// hand-maintained, and that is the honest shape of the problem: the plan
// lives outside this repository, so a check cannot diff the two. What it can
// do is refuse to let a named pillar quietly leave the progress section once
// somebody has written it down.
var planNamedCapabilities = []struct {
	name string
	why  string
}{
	{
		name: "多租户",
		// Registered by decision 197. The plan's stage 3 names it as half of
		// "多集群/多租户规模化"; the progress section has to keep saying so,
		// because the alternative is a capability whose progress is 0 and
		// which no reader can find.
		why: "the plan's section 5 names multi-tenancy as one of stage 3's two pillars, and decision 197 found it had no shared tenant-passing mechanism at all",
	},
	{
		name: "多集群",
		// Tracked since decision 123. Listed here so that removing the word
		// from the progress section — which is what happened to it before
		// decision 123 rewrote the row — is a red test rather than a silent
		// regression.
		why: "the plan's section 5 names multi-cluster as the other half of stage 3, and the federation it built has to stay visible",
	},
}

func TestEveryPlanCapabilityTheLedgerTracksIsNamedInTheProgressSection(t *testing.T) {
	// The quotes are not claims this gate makes — they are how the other
	// gates in this package read a passed-back value out of a table. Here
	// they would be a way to satisfy the search without saying anything, so
	// they are stripped first.
	progress := quotedRE.ReplaceAllString(progressSection(t), "")

	var missing []string
	for _, cap := range planNamedCapabilities {
		if !strings.Contains(progress, cap.name) {
			missing = append(missing, fmt.Sprintf(
				"the plan names %q and %s, but the progress section never mentions it; "+
					"a capability no table tracks has an invisible progress value, and an invisible "+
					"value cannot go red",
				cap.name, cap.why))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("a capability the plan names is not tracked in the progress section:\n  %s",
			strings.Join(missing, "\n  "))
	}
}
