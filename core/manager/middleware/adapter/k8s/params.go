// params.go holds the argument accessors and the identifier validators.
//
// Two things make these necessary rather than convenient.
//
// The first is that arguments arrive as map[string]any, because that is the
// shape the tool broker and the LLM tool protocol both produce. A JSON
// number may have decoded as float64, json.Number or int depending on the
// path it travelled, so every accessor funnels through toInt: a remediation
// must not fail with "expected number, got float64" at the moment it is most
// needed.
//
// The second is that Kubernetes resource names are interpolated into URL
// paths. `kubectl delete pod ../../nodes/kube-1` is not a thing that can be
// typed at a shell prompt, but it is a thing a model can put in a JSON tool
// call. Every name and namespace that reaches a path is therefore validated
// against the API server's own DNS subdomain rule before it is used, and a
// value that fails is refused rather than escaped: escaping would make the
// request well-formed and the intent unknown, and there is no legitimate
// resource name that needs escaping.
package k8s

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type params map[string]any

// dns1123Subdomain is the API server's own rule for namespaces, pods,
// deployments, nodes and label values' key parts.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// maxNameLength is the API server's limit on a DNS subdomain name.
const maxNameLength = 253

// requireName returns a validated Kubernetes object name.
func (p params) requireName(name string) (string, error) {
	s, err := p.requireString(name)
	if err != nil {
		return "", err
	}
	if err := validateName(name, s); err != nil {
		return "", err
	}
	return s, nil
}

// optionalName returns a validated name, or "" when absent.
func (p params) optionalName(name string) (string, error) {
	s, err := p.optionalString(name)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", nil
	}
	if err := validateName(name, s); err != nil {
		return "", err
	}
	return s, nil
}

func validateName(param, value string) error {
	if len(value) > maxNameLength {
		return fmt.Errorf("k8s: %s is %d characters; the API server allows at most %d", param, len(value), maxNameLength)
	}
	if !dns1123Subdomain.MatchString(value) {
		return fmt.Errorf("k8s: %s %q is not a valid Kubernetes object name (lowercase alphanumerics, '-', '.')", param, value)
	}
	return nil
}

// labelSelector is passed to the API server as a query parameter, and the
// API server parses it as an expression. It is not a shell string and cannot
// break out of its own grammar, but it is still URL-encoded by the caller
// and bounded here so a selector cannot be used to smuggle a very large
// query.
const maxSelectorLength = 1024

func (p params) optionalSelector(name string) (string, error) {
	s, err := p.optionalString(name)
	if err != nil {
		return "", err
	}
	if len(s) > maxSelectorLength {
		return "", fmt.Errorf("k8s: %s is longer than %d characters", name, maxSelectorLength)
	}
	return s, nil
}

func (p params) requireString(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("k8s: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("k8s: %s must be a string, got %T", name, raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("k8s: %s is required and must not be blank", name)
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
		return "", fmt.Errorf("k8s: %s must be a string, got %T", name, raw)
	}
	return strings.TrimSpace(s), nil
}

// requireInt reads a required integer argument.
func (p params) requireInt(name string) (int, error) {
	raw, ok := p[name]
	if !ok {
		return 0, fmt.Errorf("k8s: %s is required", name)
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("k8s: %s: %w", name, err)
	}
	return v, nil
}

// requireBool reads a required boolean argument.
func (p params) requireBool(name string) (bool, error) {
	raw, ok := p[name]
	if !ok {
		return false, fmt.Errorf("k8s: %s is required", name)
	}
	b, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("k8s: %s must be a boolean, got %T", name, raw)
	}
	return b, nil
}

// optionalBool reads a boolean argument, defaulting when absent.
func (p params) optionalBool(name string, def bool) (bool, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return def, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return def, fmt.Errorf("k8s: %s must be a boolean, got %T", name, raw)
	}
	return b, nil
}

func intArg(p map[string]interface{}, name string, def int) (int, error) {
	raw, ok := p[name]
	if !ok {
		return def, nil
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("k8s: %s: %w", name, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("k8s: %s must be positive, got %d", name, v)
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
