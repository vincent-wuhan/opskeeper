package injector

import (
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// TestEveryShippedCaseProducesAnInjectSpec is the gate on the assembly path
// between the corpus and the injector.
//
// FromSchemaStep reads the step's Duration and Params. Until the loader filled
// them, this function could not succeed for any shipped case: Duration came
// back as "" and time.ParseDuration rejected it, so the only way through was
// the unit test's hand-built step. Params being nil was the quieter half — the
// injection would have run with defaults while the case file said `table:
// orders`. Both halves are pinned here against the corpus itself.
func TestEveryShippedCaseProducesAnInjectSpec(t *testing.T) {
	// 语料住在 core/harness（决策 297 之后），注入器在 core/faults。
	// 相对路径要跨一个模块——这正是"语料可以被第三方 fork 走、注入器不行"
	// 这条边界的形状。
	cases, err := schema.NewLoader(filepath.Join("..", "..", "harness", "cases")).LoadAll()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases loaded")
	}
	for _, caseObj := range cases {
		for index, step := range caseObj.Inject {
			spec, err := FromSchemaStep(step, 42, "staging")
			if err != nil {
				t.Errorf("%s step %d: %v", caseObj.ID, index, err)
				continue
			}
			if spec.Duration <= 0 {
				t.Errorf("%s step %d: duration = %v", caseObj.ID, index, spec.Duration)
			}
			if len(spec.Params) != len(step.Params) {
				t.Errorf("%s step %d: spec carries %d params, step declared %d",
					caseObj.ID, index, len(spec.Params), len(step.Params))
			}
		}
	}
}
