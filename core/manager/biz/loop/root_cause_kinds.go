package loop

import (
	"encoding/json"
	"fmt"
	"sync"
)

// rootCauseKindsOnce guards the one-time parse of investigatedOutputSchema.
var rootCauseKindsOnce = sync.OnceValues(func() ([]string, error) {
	var schema struct {
		Properties struct {
			RootCauseObject struct {
				Properties struct {
					Kind struct {
						Enum []string `json:"enum"`
					} `json:"kind"`
				} `json:"properties"`
			} `json:"root_cause_object"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(investigatedOutputSchema), &schema); err != nil {
		return nil, fmt.Errorf("loop: parse investigatedOutputSchema: %w", err)
	}
	kinds := schema.Properties.RootCauseObject.Properties.Kind.Enum
	if len(kinds) == 0 {
		// A schema that lost its enum would make every incident look like
		// it had an unconstrained root cause, and the evaluation would
		// score that as correct. Failing here keeps the schema honest.
		return nil, fmt.Errorf("loop: investigatedOutputSchema declares no root_cause_object.kind enum")
	}
	return kinds, nil
})

// RootCauseKinds returns the closed set of root-cause categories the
// investigated phase is allowed to emit — exactly the enum the model is
// constrained to.
//
// It is derived from investigatedOutputSchema rather than restated, so it
// cannot drift: the same constant the model is prompted with is the one
// this reads. A restated copy would be a comment with a test-shaped hole in
// it, and the failure it invites is invisible — a kind added to the prompt
// would be missing here, and the evaluation would call a correct diagnosis
// an invalid one.
//
// The error is returned rather than swallowed. An empty set reads as "this
// system can name no root causes", which is the one answer that is always
// wrong.
func RootCauseKinds() ([]string, error) {
	return rootCauseKindsOnce()
}
