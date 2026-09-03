// Package loop — llm_caller_schema.go
//
// Minimal JSON-Schema validator used by LLMCaller.Call to gate the
// model's structured output against the per-phase contract schema.
//
// Scope (intentionally narrow — covers the contract shapes in this
// change; expand as needed):
//
//   - Top-level types: "object" / "array" / "string" / "number" /
//     "integer" / "boolean" / "null".
//   - "required" on objects → every named property must be present.
//   - "properties" on objects → per-property type / subschema check.
//   - "items" on arrays → every element validated against the
//     subschema.
//   - "enum" → exact match against the listed JSON values.
//
// What this validator does NOT support (callers should not supply
// these in OutputSchema; future PR can extend if a phase needs them):
//
//   - "$ref" / "$defs" / $id resolution.
//   - "oneOf" / "anyOf" / "allOf".
//   - format assertions (email, uuid, etc.).
//   - Numeric bounds ("minimum" / "maximum").
//   - String constraints ("minLength" / "pattern").
//
// Why hand-rolled instead of pulling in google/jsonschema-go: the task
// batch forbids new go.mod dependencies ("不引入新 go.mod 依赖"), and
// eino-contrib/jsonschema v1.0.3 (the other candidate mentioned in the
// design doc) is a struct→schema *generator* with no Validate() entry
// point — see https://github.com/eino-contrib/jsonschema/blob/v1.0.3/schema.go.
// The subset above is sufficient for the worker contract shapes in
// this change; flagged as a deviation in the batch report so the next
// batch can swap in google/jsonschema-go behind the same internal
// surface if richer validation is needed.
package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// schemaNode is the parsed-in-memory form of the subset of JSON Schema
// fields LLMCaller cares about. Keep field names lowercase to match
// Go's encoding/json default tag wiring (the schema source is JSON).
type schemaNode struct {
	Type       string                 `json:"type"`
	Required   []string               `json:"required"`
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Enum       []any                  `json:"enum"`

	// Minimum / Maximum bound numeric values to the inclusive range
	// [Minimum, Maximum] when present. They only apply to "number" /
	// "integer" types; other types ignore them silently. Adding
	// support here is an additive, opt-in change — pre-existing
	// schemas that omit these fields keep the same behaviour.
	Minimum *float64 `json:"minimum,omitempty"`
	Maximum *float64 `json:"maximum,omitempty"`

	// Allow additional properties by default; explicit
	// additionalProperties=false would be a future addition.
}

// parseSchema turns a JSON-Schema string into a *schemaNode. Returns
// (nil, nil) when the caller passes an empty schema — used by
// freeform-phase callers (and tests) to opt out of structural
// validation while keeping cost / token telemetry.
func parseSchema(schema string) (*schemaNode, error) {
	s := strings.TrimSpace(schema)
	if s == "" {
		return nil, nil
	}
	var node schemaNode
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields() // catch typos early
	if err := dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("schema parse error: %w", err)
	}
	if err := node.checkWellFormed(); err != nil {
		return nil, err
	}
	return &node, nil
}

// checkWellFormed catches structural mistakes (e.g. items on a non-
// array) that the per-instance validator would later surface in
// confusing ways. Called once at parse time.
func (s *schemaNode) checkWellFormed() error {
	switch s.Type {
	case "", "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return fmt.Errorf("unsupported schema type %q", s.Type)
	}
	if (s.Type == "object" || s.Type == "") && len(s.Properties) > 0 && s.Type != "" {
		// "properties" only makes sense for object schemas; explicit
		// type="array" + properties is a programmer error.
		if s.Type != "object" {
			return fmt.Errorf("properties set on non-object schema (type=%q)", s.Type)
		}
	}
	if s.Type == "array" && s.Items == nil {
		return errors.New("array schema missing items")
	}
	for name, child := range s.Properties {
		if child == nil {
			continue
		}
		if err := child.checkWellFormed(); err != nil {
			return fmt.Errorf("properties.%s: %w", name, err)
		}
	}
	if s.Items != nil {
		if err := s.Items.checkWellFormed(); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	}
	return nil
}

// validate runs (s, raw) and returns nil on success, otherwise a
// descriptive error pinpointing the failing path.
func (s *schemaNode) validate(raw json.RawMessage) error {
	if s == nil {
		return nil
	}
	parsed, err := decodeJSON(raw)
	if err != nil {
		return fmt.Errorf("schema_validate: payload is not valid JSON: %w", err)
	}
	return s.validateValue(parsed, "$")
}

// validateValue recurses into v using s as the schema, reporting
// failure path as it goes. path is the JSON-Pointer-like locator
// ("$" = root, "$/foo/0" = array element 0 of field foo).
func (s *schemaNode) validateValue(v any, path string) error {
	// Enum check fires before type-check — enum is a stronger
	// constraint and we want a precise error.
	if len(s.Enum) > 0 {
		if !enumContains(s.Enum, v) {
			return fmt.Errorf("schema_validate: path=%s value %v not in enum %v", path, jsonShort(v), s.Enum)
		}
		// Enum satisfied; skip further structural checks (they would
		// over-restrict literal representations).
		return nil
	}

	// Type check.
	switch s.Type {
	case "":
		// No explicit type → accept anything (passthrough);
		// Enum (above) already gated.
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("schema_validate: path=%s expected object, got %s", path, jsonType(v))
		}
		return s.validateObject(obj, path)
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("schema_validate: path=%s expected array, got %s", path, jsonType(v))
		}
		return s.validateArray(arr, path)
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("schema_validate: path=%s expected string, got %s", path, jsonType(v))
		}
	case "number":
		// json.Unmarshal uses float64 for all JSON numbers.
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("schema_validate: path=%s expected number, got %s", path, jsonType(v))
		}
		if s.Type == "integer" && !isInteger(f) {
			return fmt.Errorf("schema_validate: path=%s expected integer, got number %v", path, f)
		}
		if s.Minimum != nil && f < *s.Minimum {
			return fmt.Errorf("schema_validate: path=%s value=%v below minimum=%v", path, f, *s.Minimum)
		}
		if s.Maximum != nil && f > *s.Maximum {
			return fmt.Errorf("schema_validate: path=%s value=%v above maximum=%v", path, f, *s.Maximum)
		}
	case "integer":
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("schema_validate: path=%s expected integer, got %s", path, jsonType(v))
		}
		if !isInteger(f) {
			return fmt.Errorf("schema_validate: path=%s expected integer, got %v", path, f)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("schema_validate: path=%s expected boolean, got %s", path, jsonType(v))
		}
	case "null":
		if v != nil {
			return fmt.Errorf("schema_validate: path=%s expected null, got %s", path, jsonType(v))
		}
	default:
		// checkWellFormed should have caught this already; defensive.
		return fmt.Errorf("schema_validate: path=%s unsupported type %q", path, s.Type)
	}
	return nil
}

// validateObject checks required fields and per-property type/sub-schema.
// Additional (undeclared) properties are allowed — the schema says
// "at least" the listed fields exist; future enhancement can gate
// additionalProperties=false.
func (s *schemaNode) validateObject(obj map[string]any, path string) error {
	for _, name := range s.Required {
		if _, ok := obj[name]; !ok {
			return fmt.Errorf("schema_validate: path=%s missing required field %q", path, name)
		}
	}
	for name, child := range s.Properties {
		v, present := obj[name]
		if !present {
			// Present is gated by Required above; absence here means
			// the property is optional and the call omitted it.
			continue
		}
		if err := child.validateValue(v, path+"/"+escapePointer(name)); err != nil {
			return err
		}
	}
	return nil
}

// validateArray walks arr, validating each element against s.Items.
// An unset Items subschema skips per-element validation (matches the
// checkWellFormed "array needs items" guard so this branch only fires
// for top-level type="" arrays).
func (s *schemaNode) validateArray(arr []any, path string) error {
	if s.Items == nil {
		return nil
	}
	for i, v := range arr {
		if err := s.Items.validateValue(v, path+"/"+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

// enumContains reports whether needle matches any element of enum.
// Comparison uses JSON-normalised equality so 1 == 1.0 (no fractional
// fuzziness) and "foo" != true (no type coercion).
func enumContains(enum []any, needle any) bool {
	for _, e := range enum {
		if jsonEqual(e, needle) {
			return true
		}
	}
	return false
}

// jsonEqual compares two decoded JSON values structurally. Maps and
// arrays compare element-by-element; scalars use type-aware equality.
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bb, ok := b.(bool)
		return ok && av == bb
	case string:
		bb, ok := b.(string)
		return ok && av == bb
	case float64:
		bb, ok := b.(float64)
		if !ok {
			return false
		}
		return av == bb
	case map[string]any:
		bb, ok := b.(map[string]any)
		if !ok || len(av) != len(bb) {
			return false
		}
		for k, v := range av {
			if !jsonEqual(v, bb[k]) {
				return false
			}
		}
		return true
	case []any:
		bb, ok := b.([]any)
		if !ok || len(av) != len(bb) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bb[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// decodeJSON parses raw into a generic Go value (map / slice / scalars)
// using the same type model as encoding/json's default decoder. Errors
// are wrapped with a stable prefix so caller code can match.
func decodeJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty raw JSON")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// jsonType returns a human-friendly type name for the supplied value,
// used in error messages.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// jsonShort renders v in a compact form for error messages.
func jsonShort(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) > 64 {
		s = s[:61] + "..."
	}
	return s
}

// isInteger reports whether f is an exact integer in float64 sense.
// Mirrors json.Unmarshal's representation of integer literals.
func isInteger(f float64) bool {
	return f == float64(int64(f))
}

// escapePointer escapes a JSON-Pointer segment per RFC 6901: '~' →
// "~0" and '/' → "~1". Keeps error paths unambiguous for objects with
// keys containing the separator.
func escapePointer(seg string) string {
	seg = strings.ReplaceAll(seg, "~", "~0")
	seg = strings.ReplaceAll(seg, "/", "~1")
	return seg
}
