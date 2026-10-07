package frontierbound

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// stubEdgeRepo is the smallest repo that lets HandleHeartbeat succeed. The
// heartbeat only writes a status and (when the node reports one) a pig
// version, so everything else returns a zero value. It exists so the test
// exercises the real handler rather than a copy of its body.
type stubEdgeRepo struct {
	status    string
	lastSeen  time.Time
	pigWrites []string
}

func (r *stubEdgeRepo) Create(context.Context, *edgemodel.Edge) error { return nil }
func (r *stubEdgeRepo) GetByID(context.Context, uint64) (*edgemodel.Edge, error) {
	return &edgemodel.Edge{}, nil
}
func (r *stubEdgeRepo) GetByAccessKey(context.Context, string) (*edgemodel.Edge, error) {
	return nil, nil
}
func (r *stubEdgeRepo) GetByName(context.Context, string) (*edgemodel.Edge, error) {
	return nil, nil
}
func (r *stubEdgeRepo) List(context.Context, edgebiz.ListFilter) ([]*edgemodel.Edge, error) {
	return nil, nil
}
func (r *stubEdgeRepo) UpdateSecretHash(context.Context, uint64, string) error { return nil }
func (r *stubEdgeRepo) UpdateStatus(_ context.Context, _ uint64, status string, lastSeen time.Time) error {
	r.status = status
	r.lastSeen = lastSeen
	return nil
}
func (r *stubEdgeRepo) UpdateName(context.Context, uint64, string) error  { return nil }
func (r *stubEdgeRepo) SetDeviceID(context.Context, uint64, uint64) error { return nil }
func (r *stubEdgeRepo) SetAgentVersion(context.Context, uint64, string) error {
	return nil
}
func (r *stubEdgeRepo) SetPigVersion(_ context.Context, _ uint64, version string) error {
	r.pigWrites = append(r.pigWrites, version)
	return nil
}
func (r *stubEdgeRepo) Delete(context.Context, uint64) error    { return nil }
func (r *stubEdgeRepo) Count(context.Context) (int64, error)    { return 0, nil }
func (r *stubEdgeRepo) CreateIfNotExists(*edgemodel.Edge) error { return nil }

// stubModelEndpoint stands in for the manager's model-endpoint resolver.
type stubModelEndpoint struct {
	baseURL string
	model   string
	calls   int
}

func (s *stubModelEndpoint) AgentEndpoint(context.Context) (string, string) {
	s.calls++
	return s.baseURL, s.model
}

func heartbeatFor(t *testing.T, w Wiring) tunnel.HeartbeatResponse {
	t.Helper()
	if w.EdgeUC == nil {
		w.EdgeUC = edgebiz.NewUsecase(&stubEdgeRepo{}, nil, nil, slog.Default())
	}
	fs, _ := installAndDispatch(t, w)
	rpc := rpcFor(t, fs, tunnel.MethodHeartbeat)

	body, err := json.Marshal(tunnel.HeartbeatRequest{EdgeID: 7, Ts: 1})
	if err != nil {
		t.Fatalf("encode heartbeat: %v", err)
	}
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: 7}, rsp)
	if rsp.err != nil {
		t.Fatalf("heartbeat rpc: %v", rsp.err)
	}
	var out tunnel.HeartbeatResponse
	if err := json.Unmarshal(rsp.data, &out); err != nil {
		t.Fatalf("the heartbeat answer is not a HeartbeatResponse: %v", err)
	}
	return out
}

// The manager names the model endpoint and the node picks it up on the beat.
// This is the plan's 0.2 second sentence, and the assertion is that the
// answer rides the heartbeat rather than needing a new RPC.
func TestTheHeartbeatCarriesTheModelEndpoint(t *testing.T) {
	resolver := &stubModelEndpoint{baseURL: "https://ops.example.com/v1", model: "opskeeper-default"}
	answer := heartbeatFor(t, Wiring{ModelEndpoint: resolver, Log: slog.Default()})

	if answer.AgentBaseURL != resolver.baseURL {
		t.Errorf("AgentBaseURL = %q, want %q", answer.AgentBaseURL, resolver.baseURL)
	}
	if answer.AgentModel != resolver.model {
		t.Errorf("AgentModel = %q, want %q", answer.AgentModel, resolver.model)
	}
	if resolver.calls == 0 {
		t.Error("the resolver was never consulted; the field is wired but unused")
	}
}

// A manager with no resolver still answers, with empty strings. An empty
// answer is the node's signal to leave its own configuration alone; a
// relative path here would point the agent at nothing.
func TestAManagerWithNoEndpointAnswersEmpty(t *testing.T) {
	answer := heartbeatFor(t, Wiring{Log: slog.Default()})
	if answer.AgentBaseURL != "" || answer.AgentModel != "" {
		t.Errorf("a manager with no resolver produced %+v; the node would be repointed at a "+
			"destination nobody named", answer)
	}
}
