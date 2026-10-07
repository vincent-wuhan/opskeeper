// params.go holds the argument accessors.
//
// Args arrive as map[string]any because that is the shape the tool broker and
// the LLM tool protocol both produce, and a JSON number may decode as
// float64, json.Number or int depending on the path. Every accessor funnels
// through toInt so a remediation never fails on "expected number, got
// float64" at the moment it is most needed.
package redis

import (
	"encoding/json"
	"fmt"
	"strings"
)

type params map[string]any

func (p params) requireInt(name string) (int, error) {
	raw, ok := p[name]
	if !ok {
		return 0, fmt.Errorf("redis: %s is required", name)
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("redis: %s: %w", name, err)
	}
	return v, nil
}

// requireString returns a non-blank string argument.
//
// Blank counts as absent: a parameter name of "   " is not a name, and
// letting it through would reach the server as a CONFIG SET against
// nothing.
func (p params) requireString(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("redis: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("redis: %s must be a string, got %T", name, raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("redis: %s is required and must not be blank", name)
	}
	return s, nil
}

func intArg(p map[string]interface{}, name string, def int) (int, error) {
	raw, ok := p[name]
	if !ok {
		return def, nil
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("redis: %s: %w", name, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("redis: %s must be positive, got %d", name, v)
	}
	return v, nil
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
		if v != float64(int(v)) {
			return 0, fmt.Errorf("%v is not a whole number", v)
		}
		return int(v), nil
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, err
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", raw)
	}
}
