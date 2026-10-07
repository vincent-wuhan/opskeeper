// params.go holds the argument accessors for the MQ adapter.
//
// Args arrive as map[string]any, and a JSON number may have decoded as
// float64, json.Number or int depending on the path it travelled, so every
// accessor funnels through toInt. A remediation must not fail with "expected
// number, got float64" at the moment it is most needed.
package mq

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type params map[string]any

func (p params) requireString(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("mq: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("mq: %s must be a string, got %T", name, raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("mq: %s is required and must not be blank", name)
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
		return "", fmt.Errorf("mq: %s must be a string, got %T", name, raw)
	}
	return strings.TrimSpace(s), nil
}

func (p params) optionalBool(name string, def bool) (bool, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return def, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return def, fmt.Errorf("mq: %s must be a boolean, got %T", name, raw)
	}
	return b, nil
}

func intArg(p map[string]interface{}, name string, def int, max int) (int, error) {
	raw, ok := p[name]
	if !ok {
		return def, nil
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("mq: %s: %w", name, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("mq: %s must be positive, got %d", name, v)
	}
	if max > 0 && v > max {
		// A bounded batch is not a limitation to work around: "replay
		// everything" against an unbounded queue is a decision that belongs
		// to a human with a number in front of them, not to a default.
		return 0, fmt.Errorf("mq: %s must be at most %d, got %d", name, max, v)
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

// asInt reads a number out of a decoded JSON document.
func asInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	v, err := toInt(m[key])
	if err != nil {
		return 0
	}
	return v
}

func asFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func asString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// requireInt reads a required integer argument. A partition id of zero is
// valid, so this cannot go through intArg, which rejects anything
// non-positive — a rule that is right for limits and wrong for indexes.
func (p params) requireInt(name string) (int, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return 0, fmt.Errorf("mq: %s is required", name)
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("mq: %s: %w", name, err)
	}
	return v, nil
}

// brokerIDList reads a comma-separated list of broker ids.
//
// It is a string rather than an array because that is how a broker id
// arrives from an operator ("move it to 3,4,5") and from a terminal the
// investigation is being read in. It is parsed rather than passed through,
// so a typo in one entry is a refusal naming that entry instead of a
// replica list Kafka accepts and cannot place.
func (p params) brokerIDList(name string) ([]int, error) {
	raw, err := p.requireString(name)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("mq: %s entry %q is not a broker id; broker ids are non-negative integers", name, part)
		}
		if id < 0 {
			return nil, fmt.Errorf("mq: %s entry %q is not a broker id; broker ids are non-negative integers", name, part)
		}
		if slices.Contains(out, id) {
			return nil, fmt.Errorf("mq: %s lists broker %d twice; a replica cannot be on the same broker twice", name, id)
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mq: %s must name at least one broker", name)
	}
	if len(out) > maxReplicasPerPartition {
		return nil, fmt.Errorf("mq: %s names %d brokers, which is past the %d this adapter will submit", name, len(out), maxReplicasPerPartition)
	}
	return out, nil
}
