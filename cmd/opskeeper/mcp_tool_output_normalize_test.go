package main

import (
	"encoding/json"
	"testing"
)

// The defect: a BaseTool whose output is not a JSON document (an HTML error
// page, a bare string) reached canonicalDigest as a RawMessage, failed its
// unmarshal, and the whole tools/call ended as RPC error -32000 — a
// read-only tool became unusable over MCP. The fix is this envelope: every
// consumer downstream of invokeLoopMCPTool sees a JSON document.

func TestNormalizeMCPToolOutputPassesValidJSONThroughUntouched(t *testing.T) {
	for _, doc := range []string{
		`{"incidents":[],"count":0}`,
		`[1,2,3]`,
		`"plain json string"`,
		`42`,
		`null`,
		`  {"spaced": true}  `,
	} {
		got, err := normalizeMCPToolOutput(doc)
		if err != nil {
			t.Fatalf("normalize(%q): %v", doc, err)
		}
		if string(got) != doc {
			t.Errorf("valid JSON document must pass through unchanged: got %q want %q", string(got), doc)
		}
	}
}

func TestNormalizeMCPToolOutputWrapsNonJSON(t *testing.T) {
	for _, raw := range []string{
		"<html>error page</html>",
		"",
		"plain text",
		"{not json",
	} {
		got, err := normalizeMCPToolOutput(raw)
		if err != nil {
			t.Fatalf("normalize(%q): %v", raw, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("envelope for %q is not a JSON object: %v (raw: %q)", raw, err, string(got))
		}
		if decoded["output"] != raw {
			t.Errorf("envelope for %q lost the original string: got %v", raw, decoded["output"])
		}
		if len(decoded) != 1 {
			t.Errorf("envelope for %q has extra fields: %v", raw, decoded)
		}
	}
}
