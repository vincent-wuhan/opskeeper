package opskeepersreautonomy

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code, for the same reason the other two toolsets'
// tables are: every entry declares a name, a description and a schema, and
// every entry hands the call to the host. Nothing here interprets an
// argument or touches the machine.
//
// This package currently has exactly one entry, and the count is the point
// rather than a coincidence. A second tool would be a second way for a
// model to ask a node to change something with nobody watching, and the
// whole safety argument for that rests on there being a closed list of
// signed argument vectors rather than a shape the model fills in. One tool
// that takes an action *name* is a pointer into that list. A tool that
// takes a command is the list itself, handed over.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end rather than a translation
	// somewhere in the middle.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, as a raw literal so a schema
	// change shows up in review as a schema change.
	Parameters string
}

// tools is the whole inventory, sorted by name.
var tools = []toolSpec{
	{
		Name:        "host_autonomy_run",
		Label:       "自治自愈动作",
		Description: "Ask this node to perform one of its **pre-declared** self-heal actions, for the window when the control plane is unreachable. MUTATING: the action runs on the host with nobody available to approve it, and the command is whatever the package's reviewed manifest declares — you choose the action's NAME, not its command, and there is no parameter through which a command could be supplied. While the control plane is reachable the host defers this call to the approval gate like any other change, so a human decides; while it is gone, the node runs the signed argument vector itself. Use it only for an action listed in this node's manifest. Do not retry a call whose outcome is unknown — the action is spent by its idempotency key, and a second attempt is a second action. NOT for: anything not declared in the manifest — call the ordinary tool for that and let a human approve it.",
		Parameters: `{
  "type": "object",
  "required": ["action", "target", "window"],
  "properties": {
    "action": {
      "type": "string",
      "minLength": 1,
      "description": "The action's name as declared in the package manifest's autonomy.actions. An undeclared name is deferred to the approval gate, never run."
    },
    "target": {
      "type": "string",
      "minLength": 1,
      "description": "The resource the action is aimed at, e.g. a service instance name. Expands into the action's idempotency key."
    },
    "window": {
      "type": "string",
      "minLength": 1,
      "description": "Identifier for this occurrence, e.g. the incident id. One window runs once; a second call with the same window is refused as a replay."
    }
  },
  "additionalProperties": false
}`,
	},
}

// ToolNames is the inventory, in the order it is declared.
//
// It exists for the same reason the other two toolsets export it: a test —
// or a package-review diff — needs to read the list without reaching into
// the table, and a list that can only be read by the code that owns it is a
// list nothing else can check.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
