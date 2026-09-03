package changeevent

import "encoding/json"

// parseLabelsJSON decodes the JSON-encoded labels column. Returns
// nil on empty / invalid input — callers must handle nil (it means
// "no labels" not "decode error"). Decoding errors are silent
// because the column is best-effort metadata; the row is still valid.
func parseLabelsJSON(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// MarshalLabels is the inverse helper for callers that need to
// encode a map into the column format. Returns "" for nil/empty.
func MarshalLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}
