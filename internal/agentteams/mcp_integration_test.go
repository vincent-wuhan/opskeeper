//go:build integration
// +build integration

// Package mcp_integration_test 验证 stdio MCP server 与 opskeeper HTTP /v1/mcp 的端到端联通。
//
// 运行方式：
//   go test -tags=integration -count=1 -v ./plugins/opskeeper-teamharness/mcp/
//
// 前置：需要 opskeeper backend 在 OPSKEEPER_BACKEND_URL 上监听 + Bearer auth 中间件可用。
// 本测试通过 httptest 启动 fake opskeeper 后端来模拟。

package agentteams_test

import (
	"bufio"
	"io"

	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStdioMCP_E2E_WithFakeBackend(t *testing.T) {
	// Skip if python3 or server.py not available
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH:", err)
	}
	serverDir, err := filepath.Abs(filepath.Join("..", "..", "plugins", "opskeeper-teamharness", "mcp"))
	if err != nil {
		t.Skip("server.py directory cannot be resolved:", err)
	}
	if _, err := os.Stat(filepath.Join(serverDir, "server.py")); err != nil {
		t.Skip("server.py not found:", err)
	}
	// 1. 启动 fake opskeeper HTTP backend
	var receivedAuth string
	var receivedVersion string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedVersion = r.Header.Get("X-Opskeeper-Version")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		switch method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "loop.investigate", "description": "fake", "inputSchema": map[string]any{"type": "object"}},
					},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": `{"incident_id":"inc-1","root_cause":"fake","confidence":0.95}`},
					},
				},
			})
		default:
			http.Error(w, "method not found", http.StatusNotFound)
		}
	}))
	defer backend.Close()

	// 2. spawn stdio MCP server with backend URL + fake GWKey
	cmd := exec.Command("python3", "server.py")
	cmd.Dir = serverDir
	cmd.Env = append(os.Environ(),
		"OPSKEEPER_BACKEND_URL="+backend.URL,
		"OPSKEEPER_GATEWAY_KEY=test-key-abc123",
		"OPSKEEPER_TENANT_ID=tenant-test",
		"OPSKEEPER_TIMEOUT=5",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// 3. send tools/list request
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","method":"tools/list","id":1}`+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 4. read response (line-by-line; server doesn't close stdout)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	respCh := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			respCh <- scanner.Text()
		}
	}()
	select {
	case resp := <-respCh:
		var r map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(resp)), &r); err != nil {
			t.Fatalf("unmarshal: %v body=%q", err, resp)
		}
		result, _ := r["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) == 0 {
			t.Fatalf("expected non-empty tools list, got %q", resp)
		}
		t.Logf("received %d tools", len(tools))
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for response")
	}

	// 5. send tools/call request
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"loop.investigate","arguments":{"incident_id":"inc-1"}}}`+"\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	// 6. verify backend received Bearer + X-Opskeeper-Version
	time.Sleep(100 * time.Millisecond)
	if !strings.HasPrefix(receivedAuth, "Bearer ") {
		t.Fatalf("expected Bearer auth, got %q", receivedAuth)
	}
	if receivedVersion != "v1" {
		t.Fatalf("expected X-Opskeeper-Version=v1, got %q", receivedVersion)
	}
	t.Logf("backend received Authorization=%q X-Opskeeper-Version=%q",
		receivedAuth[:20]+"...", receivedVersion)
}
