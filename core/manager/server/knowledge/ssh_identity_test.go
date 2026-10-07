package knowledge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/knowledge"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/knowledge"
)

// --- 决策 336：一把能登上生产机的私钥 --------------------------------------------
//
// 这一族的每一行都在说「别把钥匙写进去」，所以用例也全部围绕这句话：
//
//	private_key 是这次请求的**输入**——它就在请求体里，写进行里等于把一把
//	  能登上 prod 的私钥发到每一个能读链的人手上；
//	public_key 是响应体里的那几百个字符——它是公开的，但没人会在链里 grep 它，
//	  而它会让每一行都臃肿；
//	fingerprint 才是该留的：它是钥匙的稳定公开身份，回答「还是同一把吗」
//	  而本身不可用；hosts 回答「这把能打开哪些机器」。
//
// 这套断言只有一件事是真正重要的：**它们必须是反向的**——把私钥塞进 payload
// 必须让用例红，否则这些断言只是注释。

type sshStore struct {
	biz.RepoStore // everything else is out of scope and panics if reached
	rows          []*model.SSHIdentity
	next          uint64
}

func (s *sshStore) ListSSHIdentities(context.Context) ([]*model.SSHIdentity, error) {
	return s.rows, nil
}
func (s *sshStore) CreateSSHIdentity(_ context.Context, row *model.SSHIdentity) error {
	s.next++
	row.ID = s.next
	s.rows = append(s.rows, row)
	return nil
}
func (s *sshStore) GetSSHIdentity(_ context.Context, id uint64) (*model.SSHIdentity, error) {
	for _, row := range s.rows {
		if row.ID == id {
			return row, nil
		}
	}
	return nil, nil
}
func (s *sshStore) UpdateSSHIdentity(_ context.Context, id uint64, name, hostsJSON, knownHosts string) error {
	for _, row := range s.rows {
		if row.ID == id {
			row.Name, row.HostsJSON, row.KnownHosts = name, hostsJSON, knownHosts
			return nil
		}
	}
	return nil
}
func (s *sshStore) DeleteSSHIdentity(_ context.Context, id uint64) error {
	out := s.rows[:0]
	for _, row := range s.rows {
		if row.ID != id {
			out = append(out, row)
		}
	}
	s.rows = out
	return nil
}

func newSSHRouter(t *testing.T) (http.Handler, *sshStore) {
	t.Helper()
	store := &sshStore{}
	uc, err := biz.New(context.Background(), store, newMemVec(), idEmbed{}, t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("biz.New: %v", err)
	}
	r := chi.NewRouter()
	NewHandler(uc).Register(r)
	return r, store
}

func sshCall(t *testing.T, router http.Handler, method, path, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
	req = req.WithContext(auditport.WithSlot(req.Context()))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	ev, ok := auditport.GetAuditEvent(req.Context())
	return rec, ev, ok
}

// realOpenSSHKey returns a genuine ed25519 private key in OpenSSH PEM form.
// A hand-written fake PEM would be rejected by the parser before the handler
// ever reached the audit call, and the test would then be passing for a reason
// that has nothing to do with what it claims to check.
func realOpenSSHKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "opskeeper-test")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

// generate：平台自己铸钥匙，私钥只在这个响应体里出现一次。留下的必须是指纹
// 与主机列表，而不是钥匙本身。
func TestGeneratedKeyIsRecordedByFingerprintAndHostsOnly(t *testing.T) {
	router, _ := newSSHRouter(t)

	rec, ev, ok := sshCall(t, router, http.MethodPost, "/v1/knowledge/ssh-identities/generate",
		`{"name":"prod-deploy","hosts":["git@github.com"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	// Sanity: the response really did carry key material, so the assertions
	// below are not vacuous — they are exactly the ones that could pass for
	// the wrong reason if the response had been empty.
	if !strings.Contains(rec.Body.String(), "public_key") {
		t.Fatalf("body = %s, want a public key for the operator to paste", rec.Body.String())
	}
	if !ok {
		t.Fatal("no row: \"who minted the key that is on those hosts\" has no other source")
	}
	if ev.Action != auditport.ActionSSHKeyGenerate {
		t.Errorf("action = %q, want ssh_key_generate", ev.Action)
	}
	blob, _ := json.Marshal(ev.Payload)
	for _, forbidden := range []string{"PRIVATE KEY", "ssh-ed25519 AAAA"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("the payload carries key material (%q): %s", forbidden, blob)
		}
	}
	payload := payloadMap(t, ev.Payload)
	if payload["fingerprint"] == nil || payload["fingerprint"] == "" {
		t.Errorf("payload = %v, want the fingerprint — it is the stable answer to \"same key?\"", ev.Payload)
	}
	if hosts, _ := payload["hosts"].([]any); len(hosts) != 1 || hosts[0] != "git@github.com" {
		t.Errorf("payload = %v, want the host list — \"which machines can this open\" is the question", ev.Payload)
	}
}

// register：私钥是**请求体**。它离处理器最近，最容易被顺手写下。
func TestRegisteredKeyNeverEchoesThePrivateKeyIntoTheChain(t *testing.T) {
	router, _ := newSSHRouter(t)

	_, ev, ok := sshCall(t, router, http.MethodPost, "/v1/knowledge/ssh-identities",
		`{"name":"byo-key","private_key":`+mustJSON(t, realOpenSSHKey(t))+`,"hosts":["git@gitlab.corp"]}`)
	if !ok {
		t.Fatal("no row")
	}
	if ev.Action != auditport.ActionSSHKeyRegister {
		t.Errorf("action = %q, want ssh_key_register", ev.Action)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), "BEGIN OPENSSH") {
		t.Fatalf("the private key reached the chain: %s", blob)
	}
	if ev.ResourceType != auditport.ResourceGitKey {
		t.Errorf("resource type = %q — the table already had a name for this thing and it was unused", ev.ResourceType)
	}
}

// 改 hosts 等于扩大一把私钥的可达范围：界面长得像编辑表单，实质是一次授权变更。
func TestUpdatingHostsIsRecordedBecauseItWidensReach(t *testing.T) {
	router, store := newSSHRouter(t)

	sshCall(t, router, http.MethodPost, "/v1/knowledge/ssh-identities/generate",
		`{"name":"prod-deploy","hosts":["git@github.com"]}`)

	_, ev, ok := sshCall(t, router, http.MethodPatch, "/v1/knowledge/ssh-identities/1",
		`{"name":"prod-deploy","hosts":["git@github.com","git@gitlab.corp"]}`)
	if !ok || ev.Action != auditport.ActionSSHKeyUpdate {
		t.Fatalf("update row = %+v (ok=%v)", ev, ok)
	}
	payload := payloadMap(t, ev.Payload)
	hosts, _ := payload["hosts"].([]any)
	if len(hosts) != 2 {
		t.Errorf("payload = %v, want the widened host list — the new reach is the whole point of the row", ev.Payload)
	}
	if len(store.rows) != 1 || !strings.Contains(store.rows[0].HostsJSON, "gitlab.corp") {
		t.Errorf("store = %+v, want the update to have landed", store.rows)
	}
}

// 删除那一行必须在删除**之前**读到名字与可达主机：删完之后链上剩下的只有一个数字 id。
func TestDeleteRecordsWhoLostWhichKeyAndWhereItCouldOpenDoors(t *testing.T) {
	router, _ := newSSHRouter(t)
	sshCall(t, router, http.MethodPost, "/v1/knowledge/ssh-identities/generate",
		`{"name":"prod-deploy","hosts":["git@github.com"]}`)

	rec, ev, ok := sshCall(t, router, http.MethodDelete, "/v1/knowledge/ssh-identities/1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !ok || ev.Action != auditport.ActionSSHKeyDelete {
		t.Fatalf("delete row = %+v (ok=%v)", ev, ok)
	}
	if ev.ResourceName != "prod-deploy" {
		t.Errorf("delete row has no name: %+v", ev)
	}
	payload := payloadMap(t, ev.Payload)
	if payload["fingerprint"] == nil || payload["fingerprint"] == "" {
		t.Errorf("delete row has no fingerprint: %+v — after deletion that is the only trace of the key", ev.Payload)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// payloadMap reads a struct payload as a map. The handlers pass typed structs
// (so the shape is checked at compile time), while these assertions want to
// poke at individual keys — and a type assertion on a struct silently yields a
// nil map, which turns every field check below into a check of nothing.
func payloadMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
