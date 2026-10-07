package topology

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDbgDecode(t *testing.T) {
	body := fmt.Sprintf(`{"name":%q,"display_name":%q,"propagates_failure":%t,"direction":%q,"semantics_tag":"hard_dep"}`,
		"calls", "calls", true, "src_to_dst")
	t.Logf("BODY=%q valid=%v", body, json.Valid([]byte(body)))
	var m map[string]any
	t.Logf("err=%v", json.Unmarshal([]byte(body), &m))
}
