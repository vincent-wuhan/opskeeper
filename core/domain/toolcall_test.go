package domain

import (
	"context"
	"reflect"
	"testing"
)

// These two structs and the interface below are not wire types, so there is no
// JSON tag to pin. What pins them is the opposite problem: the MCP audit sink
// reads every field and writes it into an audit payload under a **fixed key**,
// so a field that is renamed or dropped compiles, passes every test in this
// module, and silently stops writing one of those keys to the ledger.
//
// That is the failure this file exists for. The field lists are asserted by
// name and count, and the companion test in core/manager/server/mcp asserts
// that every one of them still reaches the payload.

// TestToolStartEventCarriesTheInputsThePendingRowNeeds pins the start event's
// shape. `DeviceID` is here as a pointer and not flattened to a uint64: a
// call made outside a device's context has no device to name, and a zero there
// would name device 0.
func TestToolStartEventCarriesTheInputsThePendingRowNeeds(t *testing.T) {
	typ := reflect.TypeOf(ToolStartEvent{})
	if typ.NumField() != 6 {
		t.Fatalf("ToolStartEvent has %d fields, want 6", typ.NumField())
	}
	want := map[string]string{
		"ToolName":  "string",
		"ArgsJSON":  "string",
		"Tenant":    "string",
		"UserID":    "uint64",
		"DeviceID":  "*uint64",
		"StartedAt": "time.Time",
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		kind, ok := want[f.Name]
		if !ok {
			t.Errorf("ToolStartEvent.%s is new; every field here is written into the audit "+
				"payload under a fixed key, so a new one needs that key and a test for it", f.Name)
			continue
		}
		if got := f.Type.String(); got != kind {
			t.Errorf("ToolStartEvent.%s is %s, want %s. The audit payload writes this field "+
				"under a key a reader already knows; changing the type changes what that key "+
				"means without anything failing to compile", f.Name, got, kind)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("ToolStartEvent is missing %s, which the pending chat_tool_calls row needs", name)
	}
}

// TestToolEndEventCarriesTheResultAndTheFailure is the end event's twin, and
// the one that has to keep `Err` as an `error`: the sink branches on it to
// choose between a success and a failure status, so a widening to `any` or a
// rename to `Failure` would leave the branch compiling and always taking the
// wrong side.
func TestToolEndEventCarriesTheResultAndTheFailure(t *testing.T) {
	typ := reflect.TypeOf(ToolEndEvent{})
	if typ.NumField() != 4 {
		t.Fatalf("ToolEndEvent has %d fields, want 4", typ.NumField())
	}
	want := map[string]string{
		"ResultJSON": "string",
		"Err":        "error",
		"EndedAt":    "time.Time",
		"Duration":   "time.Duration",
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		kind, ok := want[f.Name]
		if !ok {
			t.Errorf("ToolEndEvent.%s is new; the end payload's key set is asserted in "+
				"core/manager/server/mcp and a new field needs a key there too", f.Name)
			continue
		}
		if got := f.Type.String(); got != kind {
			t.Errorf("ToolEndEvent.%s is %s, want %s", f.Name, got, kind)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("ToolEndEvent is missing %s", name)
	}
}

// TestTheToolCallAuditSinkIsTwoMethods guards the port against widening by
// accretion, which is the way a port dies: each new method has a reason, and
// no single one of them looks like the one that made it wrong.
func TestTheToolCallAuditSinkIsTwoMethods(t *testing.T) {
	typ := reflect.TypeOf((*ToolCallAuditSink)(nil)).Elem()
	if typ.Kind() != reflect.Interface {
		t.Fatalf("ToolCallAuditSink is a %s, want an interface", typ.Kind())
	}
	if typ.NumMethod() != 2 {
		t.Fatalf("ToolCallAuditSink has %d methods, want 2: %v", typ.NumMethod(), methodNames(typ))
	}
	for _, want := range []string{"OnToolEnd", "OnToolStart"} {
		if _, ok := typ.MethodByName(want); !ok {
			t.Errorf("ToolCallAuditSink has no %s; the decorators package aliases this "+
				"interface and the MCP sink implements it, so a rename breaks both", want)
		}
	}
}

func methodNames(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumMethod(); i++ {
		out = append(out, typ.Method(i).Name)
	}
	return out
}

// TestAPortConsumerCanBeSatisfiedWithoutTheProducersPackage is the property
// the whole move was for, stated where it can be checked cheaply: a type
// declared in this package implementing the port must satisfy it, and the
// compiler must not need anything from core/manager to say so.
type fakeSink struct{ started, ended int }

func (f *fakeSink) OnToolStart(context.Context, ToolStartEvent) (string, error) {
	f.started++
	return "id", nil
}

func (f *fakeSink) OnToolEnd(context.Context, string, ToolEndEvent) error {
	f.ended++
	return nil
}

func TestAPortConsumerCanBeSatisfiedWithoutTheProducersPackage(t *testing.T) {
	var sink ToolCallAuditSink = &fakeSink{}
	if _, err := sink.OnToolStart(context.Background(), ToolStartEvent{}); err != nil {
		t.Fatalf("OnToolStart: %v", err)
	}
	if err := sink.OnToolEnd(context.Background(), "id", ToolEndEvent{}); err != nil {
		t.Fatalf("OnToolEnd: %v", err)
	}
}
