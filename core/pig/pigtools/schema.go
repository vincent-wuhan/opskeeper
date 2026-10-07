// Package pigtools renders OpsKeeper's host tool catalogue as PiG tool
// declarations.
//
// It exists because the two catalogues answer different questions.
// ports.ToolSchema is a governance record: a tool's class decides whether a
// viewer may reach it, its origin decides whether policy treats it as
// runtime-discovered, and its routing hint can be overridden by a skill
// manifest without rewriting the tool's own description. ai.ToolSchema is what
// a provider is shown, and a provider interprets exactly three fields.
//
// Keeping the two apart is the point. Folding the governance fields into the
// model-facing schema would leak the node's classification of a tool to a
// third party, which buys nothing because no provider reads them. Folding
// the model-facing fields into the governance record would make the policy
// layer depend on a JSON Schema it never validates.
//
// This package is the only place the two meet for schema purposes.
// pigagent does the same conversion for tools it also has to execute, and
// shares nothing with this file beyond the value it produces.
package pigtools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Schemas renders a whole catalogue, preserving order.
//
// A tool that cannot be rendered aborts the batch with its name and index
// rather than being skipped. Silently dropping a tool produces a model that
// refuses an investigation it was told it could run, and the refusal carries
// no clue that the tool was ever registered — the failure appears as model
// behaviour, which is the hardest kind to trace back to a schema.
func Schemas(catalogue []ports.ToolSchema) ([]ai.ToolSchema, error) {
	if len(catalogue) == 0 {
		return nil, nil
	}
	out := make([]ai.ToolSchema, 0, len(catalogue))
	for i, src := range catalogue {
		rendered, err := Schema(src)
		if err != nil {
			return nil, fmt.Errorf("pigtools: tool[%d]: %w", i, err)
		}
		out = append(out, rendered)
	}
	return out, nil
}

// Schema renders one governance record as a provider-facing declaration.
//
// Description folds in WhenToUse. OpsKeeper keeps the two apart at authoring
// time because a skill manifest may override the routing hint without
// rewriting the tool's purpose statement, but the model sees a single blurb,
// so they are joined here — at the last moment, on the way to the wire —
// rather than on the way into storage.
func Schema(src ports.ToolSchema) (ai.ToolSchema, error) {
	if src.Name == "" {
		return ai.ToolSchema{}, fmt.Errorf("tool has no name")
	}
	params, err := parameters(src.Parameters)
	if err != nil {
		return ai.ToolSchema{}, fmt.Errorf("tool %q: %w", src.Name, err)
	}
	return ai.ToolSchema{
		Name:        src.Name,
		Description: describe(src),
		Parameters:  params,
	}, nil
}

// describe joins the purpose statement and the routing hint into the one
// blurb a provider sees. Neither Class nor Origin is included: they are
// host policy, and a model cannot act on them.
func describe(src ports.ToolSchema) string {
	hint := strings.TrimSpace(src.WhenToUse)
	desc := src.Description
	switch {
	case hint == "":
		return desc
	case desc == "":
		return hint
	default:
		return desc + " When to use: " + hint
	}
}

// parameters converts the stored JSON Schema into the map form PiG carries.
//
// An absent or empty schema becomes object-with-no-properties rather than
// nil. That is what a parameterless tool means, and the alternative is worse
// than an empty schema: a tool that takes no arguments and a tool whose
// schema failed to parse look identical to the model, and only one of them
// works.
func parameters(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return emptyObjectSchema(), nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parameters are not a JSON object: %w", err)
	}
	if out == nil {
		return emptyObjectSchema(), nil
	}
	return out, nil
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
