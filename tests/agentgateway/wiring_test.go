// The two numbers that decide how much a fleet can spend live in two modules
// that cannot see each other: the recommended per-node rate is a constant in
// the gateway package, and the effective default is an env lookup in the
// config package, which sits below the gateway and cannot import it.
//
// That split is the right dependency direction and a bad place to keep a
// single fact. If they drift, the documentation says one thing and the
// deployment does another, and the only symptom is a fleet that is either
// throttled harder than promised or not throttled at all — both invisible
// until an incident. So the fact is checked here, in the one place both are
// importable.
package agentgateway_test

import (
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domains/server/llmgw"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

func TestTheDocumentedDefaultRateIsTheOneTheDeploymentGets(t *testing.T) {
	if config.DefaultEdgeRequestsPerMinute != llmgw.DefaultEdgeRequestsPerMinute {
		t.Errorf("a deployment gets %d model requests per minute per node, but the gateway "+
			"documents %d as its recommended rate; one of them is a lie in the documentation "+
			"an operator reads before a rollout",
			config.DefaultEdgeRequestsPerMinute, llmgw.DefaultEdgeRequestsPerMinute)
	}
}
