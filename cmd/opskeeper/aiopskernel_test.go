package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	aiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/chat2query"

	managerbizaiops "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agentkernel"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	managersvcaiops "github.com/vincent-wuhan/opskeeper/core/manager/service/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// TestEveryRegisteredMutatingToolHasADeclaredApprovalOwner is the drift
// detector the kernel's boot check relies on, run against the real registry
// rather than against a fixture.
//
// The kernel can only see a tool's class; it cannot see that cloud_bash
// blocks on its own approval card or that the coordination primitives have
// never been gated. So every mutating tool has to be declared, and a new one
// that nobody declared must fail here — at build time — instead of appearing
// in production as a second approval card for a single action.
func TestEveryRegisteredMutatingToolHasADeclaredApprovalOwner(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := aiopstools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, log)
	bag := reg.BuildBaseTools()
	if bag == nil {
		t.Fatal("BuildBaseTools returned no bag")
	}

	gate := agentkernel.NewDeferredGate(nil, selfSettledToolNames())
	missing, err := agentkernel.UndeclaredMutatingTools(context.Background(), bag.SchemasForLLM(), gate.Declares)
	if err != nil {
		t.Fatalf("UndeclaredMutatingTools: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("mutating tools with no declared approval owner: %v\n"+
			"add each to selfSettledToolNames (with the mechanism that approves it) "+
			"or register it with the kernel gate where its executor is built", missing)
	}
}

// testWriter keeps the registry's error logs visible in a failing run
// without printing them for a passing one.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// TestThePostConstructionToolsAreDeclared covers the tools that are bolted
// onto the runtime AFTER it is built. They are the reason the declaration
// check runs on the final bag rather than on the registry's: cloud_bash is
// not in the registry at all, so a check that stopped there would pass while
// the most dangerous tool in the product went undeclared.
func TestThePostConstructionToolsAreDeclared(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	tools := []aiopstoolsbase.BaseTool{
		// The proposer-backed shell tools (main.go's AppendToolBag).
		aiopstools.NewBashToolWithProposer(nil, nil, nil, nil, log),
		aiopstools.NewCloudBashTool(nil, log),
		aiopstools.NewInstallSkillTool(nil, log),
		aiopstools.NewServePageTool(nil, log),
		aiopstools.NewSendIMMessageTool(nil, log),
		// The coordination trio, added through the worker-spawner wiring.
		aiopstools.NewAgentTool(nil, nil, log),
		aiopstools.NewSendMessageTool(nil, log),
		aiopstools.NewTaskStopTool(nil, log),
	}

	gate := agentkernel.NewDeferredGate(nil, selfSettledToolNames())
	missing, err := agentkernel.UndeclaredMutatingTools(context.Background(), tools, gate.Declares)
	if err != nil {
		t.Fatalf("UndeclaredMutatingTools: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("post-construction mutating tools with no declared approval owner: %v", missing)
	}
}

// TestTheDeclarationListHasNoDuplicates keeps the boot log honest: the count
// it prints is read as "how many tools were declared", and a duplicate would
// make that number disagree with the bag.
func TestTheDeclarationListHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range selfSettledToolNames() {
		if n == "" {
			t.Fatal("the declaration list contains an empty name")
		}
		if seen[n] {
			t.Fatalf("%q is declared twice", n)
		}
		seen[n] = true
	}
}

// TestTheSelectedDriverDecidesWhichKernelIsBuilt is the wiring gate for the
// two PiG drivers (decision 86).
//
// The assembly point is where a driver stops being a type and becomes a
// deployment, and every way that can go wrong is invisible from inside
// core/pig: a Kernel built when the operator asked for a Session, a Session
// built from a runtime the process never created, a driver selection that
// silently falls back to the default. None of those fail a unit test in the
// module that owns the type, because the module has no opinion about which
// one an operator asked for.
//
// The test asserts the concrete type, not the behaviour, and that is
// deliberate. Behavioural equality between the two is already held by the
// differential golden in core/pig/pigagent; what is unproven here is that
// the value in the env var reaches the constructor.
func TestTheSelectedDriverDecidesWhichKernelIsBuilt(t *testing.T) {
	rt, err := pigcoding.NewRuntime(pigcoding.RuntimeOptions{
		AgentDir: t.TempDir(),
		CWD:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	models := &pigmodel.Registry{}
	base := agentKernelInput{
		Models:     models,
		Gate:       agentkernel.NewDeferredGate(nil, selfSettledToolNames()),
		Sessions:   &stubSessions{},
		PiGRuntime: rt,
	}

	t.Run("pig builds the bare loop", func(t *testing.T) {
		in := base
		in.Driver = managersvcaiops.KernelPig
		got, err := newAgentKernel(in)
		if err != nil {
			t.Fatalf("newAgentKernel: %v", err)
		}
		if _, ok := got.(*pigagent.Kernel); !ok {
			t.Fatalf("driver %q built %T, want *pigagent.Kernel", in.Driver, got)
		}
	})

	t.Run("pig-sdk builds the session driver", func(t *testing.T) {
		in := base
		in.Driver = managersvcaiops.KernelPigSDK
		got, err := newAgentKernel(in)
		if err != nil {
			t.Fatalf("newAgentKernel: %v", err)
		}
		if _, ok := got.(*pigagent.SessionKernel); !ok {
			t.Fatalf("driver %q built %T, want *pigagent.SessionKernel", in.Driver, got)
		}
	})

	// A nil runtime has to be refused, and the refusal has to NAME the
	// driver. NewSessionKernel already rejects one, so asserting only "an
	// error came back" would pass against a check that says nothing an
	// operator can act on — and the message is the only reason the outer
	// check exists at all.
	t.Run("pig-sdk refuses a missing runtime and says which driver wanted it", func(t *testing.T) {
		in := base
		in.Driver = managersvcaiops.KernelPigSDK
		in.PiGRuntime = nil
		_, err := newAgentKernel(in)
		if err == nil {
			t.Fatal("newAgentKernel accepted the session driver with no PiG runtime")
		}
		if !strings.Contains(err.Error(), string(managersvcaiops.KernelPigSDK)) {
			t.Fatalf("error %q does not name the driver that needed a runtime; an operator reading the boot log "+
				"cannot tell a wiring mistake from a constructor bug", err)
		}
	})

	// The converse: the bare loop must not care. It predates the runtime and
	// a nil here is the normal case for every deployment that has not opted
	// in, so requiring one would make the default configuration fail.
	t.Run("pig ignores the runtime", func(t *testing.T) {
		in := base
		in.Driver = managersvcaiops.KernelPig
		in.PiGRuntime = nil
		if _, err := newAgentKernel(in); err != nil {
			t.Fatalf("newAgentKernel refused the bare loop over a nil runtime: %v", err)
		}
	})
}

// stubSessions satisfies SessionRepo by embedding it.
//
// Nothing calls through it: this test asserts which kernel the assembly
// builds, and the persister's own behaviour is covered where it lives, in
// agentkernel. Embedding rather than implementing keeps this file from
// becoming a second copy of a twenty-method interface — and the breakage
// that copy would cause on every added method is the reason it is not
// written. A nil embedded interface also makes an accidental call panic
// rather than quietly succeed against nothing.
type stubSessions struct{ managerbizaiops.SessionRepo }

// ROADMAP C.1 is marked ☑ delivered, and the chat_to_query BaseTool it names
// was absent from every manager that ever ran: the registry registers the
// tool only when the translator has an LLM client, and the one setter that
// hands it one had no caller. The table, the store, the translator, the
// validator and the executor were all built, migrated and tested; the last
// twenty lines of wiring were the ones nobody wrote.
//
// These two tests hold both halves of that. The first is the tool's presence,
// which is what a delivery claim means; the second is that the documented
// disable path still works, so the presence cannot be bought by registering
// the tool unconditionally.
type stubCompleter struct{}

func (stubCompleter) Complete(context.Context, pigmodel.Request) (*pigai.AssistantMessage, error) {
	return &pigai.AssistantMessage{}, nil
}

func toolNames(t *testing.T, bag *aiopstools.ToolBag) []string {
	t.Helper()
	var out []string
	for _, tool := range bag.AllTools() {
		if tool == nil {
			continue
		}
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		out = append(out, info.Name)
	}
	return out
}

func TestChatToQueryReachesTheToolBagOnceItsTranslatorIsWired(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := aiopstools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, log)
	reg.SetChatToQueryLLM(stubCompleter{})

	names := toolNames(t, reg.BuildBaseTools())
	if !slices.Contains(names, chat2query.ToolNameChatToQuery) {
		t.Fatalf("the tool bag holds %d tools and none is %s: a BaseTool the roadmap calls "+
			"delivered, and that no deployment has ever handed the model", len(names), chat2query.ToolNameChatToQuery)
	}
}

// The gate on the LLM client is deliberate — an operator disables NL→Query
// by not constructing the client — so this is the half that must not move.
func TestChatToQueryIsStillAbsentWhenNoLLMClientIsWired(t *testing.T) {
	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := aiopstools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, log)

	names := toolNames(t, reg.BuildBaseTools())
	if slices.Contains(names, chat2query.ToolNameChatToQuery) {
		t.Fatalf("%s is present with no LLM client: the tool would be advertised to the model "+
			"and then fail every call", chat2query.ToolNameChatToQuery)
	}
}
