// params.go holds the argument accessors for the Host adapter.
package host

import (
	"encoding/json"
	"fmt"
	"strings"
)

type params map[string]any

func (p params) requireString(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("host: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("host: %s must be a string, got %T", name, raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("host: %s is required and must not be blank", name)
	}
	return s, nil
}

func (p params) optionalString(name string) (string, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("host: %s must be a string, got %T", name, raw)
	}
	return strings.TrimSpace(s), nil
}

// optionalInt reads an integer argument, falling back to def when it is
// absent or null.
//
// The fallback is the only place this adapter invents a value, and it is
// confined to a bound (how many rows, how many days) rather than to an
// identifier: a default limit changes how much is shown, while a default
// pid would change what is killed. Identifiers go through requireString or
// are absent, never defaulted.
func (p params) optionalInt(name string, def int) (int, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return def, nil
	}
	n, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("host: %s: %w", name, err)
	}
	return n, nil
}

func toInt(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case float32:
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("not an integer: %q", v.String())
		}
		return int(n), nil
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
			return 0, fmt.Errorf("not an integer: %q", v)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", raw)
	}
}
