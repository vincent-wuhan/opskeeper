// Package agentteams 提供 AgentTeams 集成的客户端（MinIO state、Higress consumer）。
//
// AgentTeams Manager + 6 Worker 通过 stdio MCP proxy (mcp/server.py) 调 opskeeper。
// 顶层状态由 Manager 写入 MinIO（与 AgentTeams 原生 manage-state.sh 共享 bucket），
// opskeeper 提供 Get/Put 接口供 Manager 调用。
//
// 路径约定：shared/opskeeper/tasks/{task_id}/state.json
package agentteams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// State 是 AgentTeams 顶层元状态（与 opskeeper loop 7 阶段分层）。
//
// 顶层 6 阶段：
//
//	detected → rca → critic_audit → repair_execution → verification → postmortem
//
// 内部 7 阶段 orchestrator 细节不对调用方可见。
type State struct {
	SchemaVersion string         `json:"schema_version"` // "v1"
	TaskID        string         `json:"task_id"`
	Phase         string         `json:"phase"`  // detected/rca/critic_audit/...
	Status        string         `json:"status"` // in_progress/completed/blocked
	AssignedTo    string         `json:"assigned_to,omitempty"`
	BlastRadius   string         `json:"blast_radius,omitempty"` // host/cluster/tenant_wide
	RetryCount    int            `json:"retry_count,omitempty"`
	Phases        map[string]any `json:"phases,omitempty"` // 每阶段结果
	Audit         []AuditEvent   `json:"audit,omitempty"`
	HITL          *HITLRecord    `json:"hitl,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Version       int64          `json:"version"` // CAS version
}

// AuditEvent 状态推进审计事件。
type AuditEvent struct {
	Event  string    `json:"event"` // dispatch / phase_advance / hitl_decision / tool_call
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Actor  string    `json:"actor,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	// TraceID 是 plugin stdio MCP server 透传的 W3C trace id（32 hex）；
	// 通过 backend mcp middleware 写入 ctx，handler 取出来写到 audit log，
	// 这样 state.json.audit[] 与 LoongSuite / Tempo trace 可按 trace_id 关联。
	TraceID string `json:"trace_id,omitempty"`
}

// HITLRecord 人类介入决策记录（ADR-019 双签）。
type HITLRecord struct {
	RequestID   string     `json:"request_id"`
	BlastRadius string     `json:"blast_radius"`
	Required    []string   `json:"required"` // 双签角色列表
	Decision    string     `json:"decision"` // pending/approve/reject
	Signers     []string   `json:"signers,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

// MinIOClient 抽象 MinIO S3 操作。本包不直接依赖 minio SDK。
type MinIOClient interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// HTTPMinIOClient 通过 HTTP 调 opskeeper 内部的 /v1/state/{task_id} 接口。
// 这是推荐用法：opskeeper 自己持有 MinIO SDK 依赖，对外仅暴露 HTTP。
type HTTPMinIOClient struct {
	baseURL    string
	httpClient *http.Client
	authToken  string // opskeeper 内部服务 token
}

// NewHTTPMinIOClient 构造。
func NewHTTPMinIOClient(baseURL, authToken string) *HTTPMinIOClient {
	return &HTTPMinIOClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		authToken:  authToken,
	}
}

func (c *HTTPMinIOClient) keyFor(taskID string) string {
	return path.Join("shared/opskeeper/tasks", taskID, "state.json")
}

// Put 写 state.json。
func (c *HTTPMinIOClient) Put(ctx context.Context, taskID string, body []byte) error {
	url := c.baseURL + "/v1/state/" + taskID
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("state.Put: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("state.Put http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("state.Put: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// Get 读 state.json。
func (c *HTTPMinIOClient) Get(ctx context.Context, taskID string) ([]byte, error) {
	url := c.baseURL + "/v1/state/" + taskID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("state.Get: %w", err)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("state.Get http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrStateNotFound
	}
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("state.Get: HTTP %d: %s", resp.StatusCode, string(buf))
	}
	return io.ReadAll(resp.Body)
}

var ErrStateNotFound = errors.New("state not found")

// CASLock 提供状态写的乐观并发控制（基于 version 字段）。
type CASLock struct {
	mu       sync.Mutex
	versions map[string]int64
}

// NewCASLock 构造。
func NewCASLock() *CASLock {
	return &CASLock{versions: make(map[string]int64)}
}

// CompareAndSwap 校验 version 后写入；冲突返回 ErrCASConflict。
func (l *CASLock) CompareAndSwap(ctx context.Context, c MinIOClient, taskID string, newState *State) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	expected, has := l.versions[taskID]
	if has && newState.Version != expected+1 {
		return fmt.Errorf("CAS conflict: expected version %d, got %d (task %s)", expected+1, newState.Version, taskID)
	}

	body, err := json.Marshal(newState)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if err := c.Put(ctx, taskID, body); err != nil {
		return err
	}
	l.versions[taskID] = newState.Version
	return nil
}

// LoadState 是便捷的 Get + JSON decode。
func LoadState(ctx context.Context, c MinIOClient, taskID string) (*State, error) {
	raw, err := c.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}
	return &s, nil
}

// PutState 是便捷的 JSON encode + Put（不带 CAS，调用方需自己保证）。
func PutState(ctx context.Context, c MinIOClient, taskID string, s *State) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.Put(ctx, taskID, body)
}
