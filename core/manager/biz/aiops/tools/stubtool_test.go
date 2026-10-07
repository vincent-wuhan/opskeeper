package tools

import (
	"context"
	"encoding/json"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"testing"
)

// stubTool is a BaseTool that answers with its own name and nothing else.
//
// It exists in this package as well as in toolcore because a test double
// is not shared vocabulary: it has no business being importable from
// another package, and the two copies drifting apart cannot break anything
// because neither is used by production code.
type stubTool struct {
	name, description, whenToUse, class, params string
}

func (s *stubTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	params := s.params
	if params == "" {
		params = `{"type":"object"}`
	}
	return &basetool.ToolInfo{
		Name:        s.name,
		Description: s.description,
		WhenToUse:   s.whenToUse,
		Parameters:  json.RawMessage(params),
		Class:       s.class,
	}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, _ string, _ ...basetool.InvokeOption) (string, error) {
	return `{"ok":true,"by":"` + s.name + `"}`, nil
}

func newStub(name, desc string) *stubTool {
	return &stubTool{
		name: name, description: desc, whenToUse: "use when " + name,
		class: "read", params: `{"type":"object","properties":{"x":{"type":"string"}}}`,
	}
}

// containsToolName reports whether any tool in the slice carries the given
// Name. It is here for the same reason newStub is: the toolbag tests took
// this helper with them, and a test that loses its reader to a refactor
// fails for a reason that has nothing to do with what it is testing.
func containsToolName(t *testing.T, tools []basetool.BaseTool, name string) bool {
	t.Helper()
	for _, x := range tools {
		info, err := x.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if info.Name == name {
			return true
		}
	}
	return false
}

// fakeHostResolver stands in for the shared device resolver in the tests
// that stayed behind. The host cluster has its own copy; neither is
// reachable from production code, so the two cannot drift into anything
// that matters.
type fakeHostResolver struct {
	mapping map[uint64]uint64
	err     error
}

func (f *fakeHostResolver) ResolveEdgeID(_ context.Context, deviceID uint64) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.mapping == nil {
		return 0, nil
	}
	return f.mapping[deviceID], nil
}
