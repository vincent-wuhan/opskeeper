// Package skill on the edge side wires the tunnel-level execute_skill
// handler into the shared skill registry. The agent imports this package
// (and any builtin skill packages) at startup; once the tunnel handler
// is registered, every cloud->edge MethodExecuteSkill RPC dispatches by
// key to the corresponding Executor.
package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Dispatch is the body of the edge-side execute_skill handler. It
// unmarshals the wire request, looks up the executor, runs Execute with
// the param blob, and packages the response (result or error string).
//
// Errors from the executor land in the response Error field — the RPC
// itself doesn't fail (so the manager can render the error to the
// operator); that keeps the audit trail intact and avoids the caller
// guessing whether a transport error or a skill error occurred.
func Dispatch(ctx context.Context, body []byte) ([]byte, error) {
	// tunnel.ExecuteSkillRequest is the declared shape of this body. It was
	// declared and used by nobody while this function hand-rolled an
	// identical anonymous struct, so the contract the manager marshals and
	// the contract the node reads were two literals that no compiler could
	// compare. Same for the response below.
	var req tunnel.ExecuteSkillRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("decode execute_skill body: %w", err)
	}
	if req.Key == "" {
		return marshalResp(nil, "execute_skill: key required")
	}
	exec, ok := skill.Get(req.Key)
	if !ok {
		return marshalResp(nil, fmt.Sprintf("execute_skill: unknown skill %q", req.Key))
	}
	result, err := exec.Execute(ctx, req.Params)
	if err != nil {
		return marshalResp(nil, err.Error())
	}
	return marshalResp(result, "")
}

func marshalResp(result json.RawMessage, errMsg string) ([]byte, error) {
	body, err := json.Marshal(tunnel.ExecuteSkillResponse{Result: result, Error: errMsg})
	if err != nil {
		return nil, fmt.Errorf("marshal execute_skill resp: %w", err)
	}
	return body, nil
}
