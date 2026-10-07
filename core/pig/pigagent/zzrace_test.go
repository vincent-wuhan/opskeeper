package pigagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

func twoToolStep() ai.FauxResponseStep {
	return ai.FauxStaticStep(ai.FauxResponse{
		Content: []ai.FauxContentBlock{
			ai.FauxToolCall("a", map[string]any{}, "tc-1"),
			ai.FauxToolCall("b", map[string]any{}, "tc-2"),
		},
		StopReason: string(ai.StopReasonToolUse),
	})
}

func TestRaceTwoParallelTools(t *testing.T) {
	model := newFauxModel(t, twoToolStep(), textStep("done"))
	sink := &collectSink{}
	k, err := NewKernel(KernelOptions{
		Models: &fauxResolver{model: model},
		Deps: func(context.Context, ports.AgentRequest) (Deps, error) {
			return Deps{Tools: staticBag{tools: []ports.Tool{
				&fakeTool{schema: ports.ToolSchema{Name: "a", Class: domain.ClassRead, Parameters: json.RawMessage(`{"type":"object"}`)}, out: "A"},
				&fakeTool{schema: ports.ToolSchema{Name: "b", Class: domain.ClassRead, Parameters: json.RawMessage(`{"type":"object"}`)}, out: "B"},
			}}}, nil
		},
		Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("NewKernel: %v", err)
	}
	if _, err := k.Run(ports.WithSink(context.Background(), sink), ports.AgentRequest{SessionID: "s", UserText: "go"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("frames: %v", frameTypes(sink.Frames()))
	if n := len(sink.ofType(wire.StreamToolStart)); n != 2 {
		t.Fatalf("tool_start frames = %d, want 2", n)
	}
}
