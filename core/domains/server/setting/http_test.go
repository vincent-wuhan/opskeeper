package setting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizsetting "github.com/vincent-wuhan/opskeeper/core/domains/biz/setting"
)

type llmRevealTestService struct {
	getCalls int
}

func (s *llmRevealTestService) Get(_ context.Context, _, _ string) (string, bool, error) {
	s.getCalls++
	return "secret-value", true, nil
}

func (s *llmRevealTestService) Set(_ context.Context, _, _, _ string, _ bool) error { return nil }

func (s *llmRevealTestService) List(_ context.Context, _ string) ([]bizsetting.SettingDTO, error) {
	return nil, nil
}

func (s *llmRevealTestService) Delete(_ context.Context, _, _ string) error { return nil }

func TestRevealLLMAPIKeyIsForbidden(t *testing.T) {
	svc := &llmRevealTestService{}
	router := chi.NewRouter()
	NewHandler(svc).Register(router)

	request := httptest.NewRequest(http.MethodGet, "/v1/system-settings/llm/openai_api_key/reveal", nil)
	request = request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{
		UserID: 1, Role: "admin", IsSuperuser: true,
	}))
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "forbidden" {
		t.Fatalf("code = %q, want forbidden", body.Code)
	}
	if svc.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0", svc.getCalls)
	}
}

// 一条四字符的敏感值此前会被**完整**写进审计链：截断前缀的判断是
// `len(value) > 4`，于是「短到不需要截断」的敏感值一个字节都没少。链是
// 签过名的、每个能读审计表的人都能看的东西，所以这条断言按长度扫一遍。
func TestASensitiveSettingValueNeverReachesTheChain(t *testing.T) {
	// Short enough that the old prefix guard let the whole value through,
	// and long enough that it leaked its opening four characters.
	for _, value := range []string{"abcd", "hunter2-correct-horse"} {
		svc := &llmRevealTestService{}
		router := chi.NewRouter()
		NewHandler(svc).Register(router)

		body, err := json.Marshal(map[string]any{"value": value})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/v1/system-settings/llm/openai_api_key", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auditport.WithSlot(req.Context()))
		req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{
			UserID: 1, Role: "admin", IsSuperuser: true,
		}))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		ev, set := auditport.GetAuditEvent(req.Context())
		if !set {
			t.Fatalf("%q: no audit row at all", value)
		}
		blob, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(blob), value) {
			t.Fatalf("%q: the chain carries the value: %s", value, blob)
		}
		// The prefix leak is subtler than the whole value, so check that
		// too rather than trusting the guard above.
		if len(value) > 4 && strings.Contains(string(blob), value[:4]) {
			t.Fatalf("%q: the chain carries its first four characters: %s", value, blob)
		}
		p := ev.Payload.(map[string]any)
		if p["value_digest"] == "" || p["value_digest"] == nil {
			t.Fatalf("%q: no digest, so the row cannot answer \"did it change\": %v", value, p)
		}
		if _, present := p["value_hint"]; present {
			t.Fatalf("%q: a sensitive row still carries a hint: %v", value, p)
		}
	}
}

// 非敏感的值仍然照旧记内容——那一行是给人看的，截断到 64 字符就够了。
// 把两种情况混为一谈会让人以为审计日志从此什么都不记。
func TestANonSensitiveSettingValueIsStillReadable(t *testing.T) {
	svc := &llmRevealTestService{}
	router := chi.NewRouter()
	NewHandler(svc).Register(router)

	body, _ := json.Marshal(map[string]any{"value": "primary", "sensitive": false})
	req := httptest.NewRequest(http.MethodPut, "/v1/system-settings/grafana/url", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auditport.WithSlot(req.Context()))
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{
		UserID: 1, Role: "admin", IsSuperuser: true,
	}))
	router.ServeHTTP(httptest.NewRecorder(), req)

	ev, set := auditport.GetAuditEvent(req.Context())
	if !set {
		t.Fatal("no audit row")
	}
	p := ev.Payload.(map[string]any)
	if p["value_hint"] != "primary" {
		t.Fatalf("value_hint = %v, want the value itself for a non-sensitive key", p["value_hint"])
	}
	if _, present := p["value_digest"]; present {
		t.Fatalf("a non-sensitive row grew a digest: %v", p)
	}
}
