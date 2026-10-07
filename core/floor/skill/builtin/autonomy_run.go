package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// This tool is the only way a node acts on its own.
//
// It is deliberately shaped so that the model cannot express a command. Its
// parameters are an action *name* from a signed declaration, a target, and
// an occurrence — three strings the model chooses from and the host
// resolves. There is no argv parameter, no command string, and no way to
// name a binary, because the alternative is a tool whose description has to
// promise that the model will only use it for declared actions, and a
// promise is not a boundary. The argv comes from the manifest, was compared
// against the manifest, and is executed as written.
//
// The tool is registered on every node and refuses on any node that has no
// autonomy declared, which is the state of every package that ships today.
// It is a host executor, so its presence in the registry does not put it in
// front of a model: only a manifest that names it makes the gate willing to
// dispatch it. That is what keeps the read-only profile read-only, and it
// is why adding this file does not change plugin coverage.

func init() { skill.Register(&AutonomyRun{}) }

// AutonomyRun asks the node to perform one of its declared self-heal
// actions, with nobody available to approve it.
type AutonomyRun struct{}

// AutonomyOutcome is what the host's arbiter reports back.
type AutonomyOutcome struct {
	// Verdict is "run", "defer" or "refuse".
	Verdict string `json:"verdict"`
	// Reason is a human-readable explanation, carried through so the model
	// can tell a refusal it should not retry from one it should.
	Reason string `json:"reason,omitempty"`
	// Ran is true only when the action actually executed.
	Ran bool `json:"ran"`
	// ExitCode is the action's own exit status.
	ExitCode int    `json:"exit_code,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// AutonomyRunner is the host's self-heal decision.
//
// It is declared here rather than imported: this package sits on the shared
// floor, which is not allowed to reach into a bounded context, and the
// alternative — a floor type that the node plane satisfies — would make the
// decision's shape a contract the whole system has to move in lockstep. The
// two types this needs are a string and three answers.
type AutonomyRunner interface {
	// PerformAutonomy asks the arbiter about one declared action and, if it
	// is allowed, runs it.
	PerformAutonomy(ctx context.Context, action, target, window string) (AutonomyOutcome, error)
}

var (
	autonomyMu     sync.RWMutex
	autonomyRunner AutonomyRunner
)

// SetAutonomyRunner installs the node's arbiter. It is called once, by the
// composition root, and a nil runner puts the node back in the state it
// starts in: a tool that refuses.
func SetAutonomyRunner(r AutonomyRunner) {
	autonomyMu.Lock()
	autonomyRunner = r
	autonomyMu.Unlock()
}

func currentAutonomyRunner() AutonomyRunner {
	autonomyMu.RLock()
	defer autonomyMu.RUnlock()
	return autonomyRunner
}

// ToolKey is the registered name of the autonomy tool.
//
// It is a constant rather than a literal repeated at the two places that
// have to recognise this tool by name — the node's approval policy and the
// node's tool router — because those two places are the whole of what makes
// autonomy reachable, and a router that spells the name differently from
// the registry is a router that never matches. It is exported because the
// node's policy layer is in another module from the skill it is special-
// casing, and a cross-module special case written as a string is a special
// case nobody can find.
const ToolKey = "host_autonomy_run"

// Metadata describes the tool to a model that may call it.
func (AutonomyRun) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:      ToolKey,
		Name:     "自治自愈动作",
		Class:    skill.ClassDangerous,
		Category: "autonomy",
		Description: "在本节点执行一个**已声明**的自治自愈动作, 用于控制面失联期间. " +
			"只接受动作名 (action)、目标 (target) 与本次事件标识 (window); " +
			"命令本身由插件清单声明, 模型无法指定. " +
			"控制面在线时一律返回 defer 并走正常审批. " +
			"NOT for: 任何未在清单 autonomy.actions 中声明的操作 —— 那些请直接调用对应工具, 由人审批.",
		Params: skill.ParamSchema{
			{Name: "action", Param: skill.Param{Type: "string", Desc: "清单中 autonomy.actions 声明的动作名"}},
			{Name: "target", Param: skill.Param{Type: "string", Desc: "动作作用的资源, 例如服务实例名"}},
			{Name: "window", Param: skill.Param{Type: "string", Desc: "本次事件的标识; 同一个 key 只会被执行一次"}},
		},
		ResultPreview: "{verdict: run|defer|refuse, ran, exit_code, stdout, stderr, reason?}",
		// Bounded because this is the one tool whose output is a command's
		// rather than a probe's, and a command can print forever.
		Limits: domain.ToolLimits{OutputBytes: 65536, TimeoutSeconds: 120},
	}
}

type autonomyRunParams struct {
	Action string `json:"action"`
	Target string `json:"target"`
	Window string `json:"window"`
}

type autonomyRunResult struct {
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
	Ran      bool   `json:"ran"`
	ExitCode int    `json:"exit_code,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// Execute asks the arbiter and reports the answer.
//
// Every failure is a refusal in the answer rather than an error the model
// has to interpret: a model that is told "defer" learns the control plane is
// there, and a model that is told "refuse" learns the action is not
// permitted right now. Both are answers. An exception here would surface as
// an extension fault, which is a fourth thing that means none of them.
func (a AutonomyRun) Execute(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var p autonomyRunParams
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("host_autonomy_run: %w", err)
		}
	}
	if p.Action == "" {
		return nil, fmt.Errorf("host_autonomy_run: an action name is required; this tool takes the name of a declared action, not a command")
	}
	runner := currentAutonomyRunner()
	if runner == nil {
		return json.Marshal(autonomyRunResult{
			Verdict: "refuse",
			Reason:  "this node has no autonomy declared, so it has nothing it may do without asking a human",
		})
	}
	outcome, err := runner.PerformAutonomy(ctx, p.Action, p.Target, p.Window)
	if err != nil {
		return json.Marshal(autonomyRunResult{
			Verdict: "refuse",
			Reason:  "the node could not ask its own autonomy arbiter: " + err.Error(),
		})
	}
	return json.Marshal(autonomyRunResult(outcome))
}
