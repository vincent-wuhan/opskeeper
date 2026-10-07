package imbridge_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizbridge "github.com/vincent-wuhan/opskeeper/core/manager/biz/imbridge"
	store "github.com/vincent-wuhan/opskeeper/core/manager/data/imbridge/store"
	srvim "github.com/vincent-wuhan/opskeeper/core/manager/server/imbridge"
)

// 这个包里此前一个测试都没有。决策 310 给它补上第一批，
// 而这批测试要钉住的是一件安全性质，而不只是"路由能跑通"：
// **审计链记录"谁读了密钥"，但绝不记录密钥本身。**

func newHandler(t *testing.T) (*srvim.Handler, *bizbridge.UC) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	uc := bizbridge.NewUC(store.New(db))
	// bridge 与 apps 只被 webhook 路径用；这四个 admin 路由不碰它们，
	// 所以这里传 nil 是诚实的——不是为了绕过什么，而是因为不相关。
	return srvim.NewHandler(nil, nil, uc, nil), uc
}

func call(t *testing.T, h *srvim.Handler, method, path, role string, uid uint64, body string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	r := chi.NewRouter()
	h.RegisterProtected(r)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := auditport.WithSlot(req.Context())
	ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: uid, Role: role})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	ev, set := auditport.GetAuditEvent(req.Context())
	return rec, ev, set
}

const secret = "t-aB3xQ-secret-do-not-log"

func createApp(t *testing.T, h *srvim.Handler, uid uint64) uint64 {
	t.Helper()
	body := `{"provider":"feishu","mode":"stream","name":"ops bot","app_id":"cli_001","app_secret":"` + secret + `","verify_token":"vtok","enabled":true}`
	rec, ev, set := call(t, h, http.MethodPost, "/v1/im/apps", tenantctx.RoleAdmin, uid, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("createApp: code = %d (%s)", rec.Code, rec.Body)
	}
	var dto struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.ID == 0 || !set || ev.Action != auditport.ActionIMAppCreate {
		t.Fatalf("create produced id=%d ev=%+v set=%v", dto.ID, ev, set)
	}
	return dto.ID
}

// TestSecretRevealIsAuditedWithoutLoggingTheSecret 是这个包里最重要的一条。
//
// 这条路由把**明文 app_secret** 原样回给管理员。所以审计必须回答
// "谁读了密钥"，同时**绝不能把密钥抄进链**——否则审计日志本身就变成
// 第二个明文密钥存储面，而这个接口存在的全部意义就是少泄漏一处。
func TestSecretRevealIsAuditedWithoutLoggingTheSecret(t *testing.T) {
	h, _ := newHandler(t)
	id := createApp(t, h, 7)

	rec, ev, set := call(t, h, http.MethodPost, "/v1/im/apps/"+itoa(id)+"/reveal", tenantctx.RoleAdmin, 7, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["app_secret"] != secret {
		t.Fatalf("reveal did not return the secret: %q", out["app_secret"])
	}
	if !set || ev.Action != auditport.ActionIMAppSecretReveal || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v), want a successful im_app_secret_reveal", ev, set)
	}
	// 逐字检查整个事件体，而不是只看某个字段——因为"密钥出现在别处"
	// 才是最坏的那种漏。
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the audit event carries the secret itself: %s", blob)
	}
	p := ev.Payload.(map[string]any)
	if p["app_secret_set"] != true {
		t.Fatalf("app_secret_set = %v, want true — 读没读得动密钥要能回答", p["app_secret_set"])
	}
	if p["app_id"] != "cli_001" || p["provider"] != "feishu" {
		t.Fatalf("payload does not identify which webhook: %v", p)
	}
}

// TestNonAdminRevealAttemptIsAudited：越权读密钥的尝试。
// 它此前连 403 都不留痕，而这是最需要被看见的一类请求。
func TestNonAdminRevealAttemptIsAudited(t *testing.T) {
	h, _ := newHandler(t)
	id := createApp(t, h, 7)

	rec, ev, set := call(t, h, http.MethodPost, "/v1/im/apps/"+itoa(id)+"/reveal", tenantctx.RoleUser, 42, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("a forbidden reveal left no failure row: %+v (set=%v)", ev, set)
	}
	if ev.Action != auditport.ActionIMAppSecretReveal {
		t.Fatalf("action = %q, want the reveal action so the two can be told apart", ev.Action)
	}
	if !strings.Contains(ev.ErrorMessage, "not an admin") {
		t.Fatalf("error = %q", ev.ErrorMessage)
	}
}

func TestRevealMissingAppIsAudited(t *testing.T) {
	h, _ := newHandler(t)
	rec, ev, set := call(t, h, http.MethodPost, "/v1/im/apps/9999/reveal", tenantctx.RoleAdmin, 7, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("revealing a nonexistent app returned 200: %s", rec.Body)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v), want a failure row", ev, set)
	}
}

// TestUpdateRecordsWhatChangedAndNotTheSecret：update 走的是同一个
// appPayload，所以它同样会带着明文密钥进来——审计载荷必须一样只记存在性。
func TestUpdateRecordsWhatChangedAndNotTheSecret(t *testing.T) {
	h, _ := newHandler(t)
	id := createApp(t, h, 7)

	body := `{"provider":"feishu","mode":"stream","name":"ops bot renamed","app_id":"cli_001","app_secret":"rotated-` + secret + `","enabled":false}`
	rec, ev, set := call(t, h, http.MethodPut, "/v1/im/apps/"+itoa(id), tenantctx.RoleAdmin, 7, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d (%s)", rec.Code, rec.Body)
	}
	if !set || ev.Action != auditport.ActionIMAppUpdate {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), "rotated-"+secret) {
		t.Fatalf("update audit carries the new secret: %s", blob)
	}
	p := ev.Payload.(map[string]any)
	if p["name"] != "ops bot renamed" || p["enabled"] != false {
		t.Fatalf("payload does not carry the resulting state: %v", p)
	}
	if p["app_secret_set"] != true {
		t.Fatalf("app_secret_set = %v, want true", p["app_secret_set"])
	}
}

// TestDeleteAuditsWhichWebhookDisappeared：删完之后 app_id 就再也
// 取不回来了，所以载荷里必须有删除**之前**读到的那一份。
func TestDeleteAuditsWhichWebhookDisappeared(t *testing.T) {
	h, _ := newHandler(t)
	id := createApp(t, h, 7)

	rec, ev, set := call(t, h, http.MethodDelete, "/v1/im/apps/"+itoa(id), tenantctx.RoleAdmin, 7, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if !set || ev.Action != auditport.ActionIMAppDelete || ev.Status != auditport.StatusSuccess {
		t.Fatalf("ev = %+v (set=%v)", ev, set)
	}
	p := ev.Payload.(map[string]any)
	if p["app_id"] != "cli_001" {
		t.Fatalf("delete did not record which webhook went away: %v", p)
	}
	blob, _ := json.Marshal(ev)
	if strings.Contains(string(blob), secret) {
		t.Fatalf("delete audit carries the secret: %s", blob)
	}
}

func TestNonAdminCreateDeleteAreAudited(t *testing.T) {
	h, _ := newHandler(t)
	for _, tc := range []struct {
		name, method, path string
		wantAction         string
	}{
		{"create", http.MethodPost, "/v1/im/apps", auditport.ActionIMAppCreate},
		{"delete", http.MethodDelete, "/v1/im/apps/1", auditport.ActionIMAppDelete},
	} {
		rec, ev, set := call(t, h, tc.method, tc.path, tenantctx.RoleViewer, 5, `{"provider":"feishu","app_id":"x","name":"n"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: code = %d, want 403", tc.name, rec.Code)
		}
		if !set || ev.Action != tc.wantAction || ev.Status != auditport.StatusFailure {
			t.Fatalf("%s: ev = %+v (set=%v), want a failure %s", tc.name, ev, set, tc.wantAction)
		}
	}
}

// TestCreateValidationFailureIsAudited：一个被拒的创建也是一次尝试。
func TestCreateValidationFailureIsAudited(t *testing.T) {
	h, _ := newHandler(t)
	rec, ev, set := call(t, h, http.MethodPost, "/v1/im/apps", tenantctx.RoleAdmin, 7, `{"provider":"nosuch","app_id":"x","name":"n"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !set || ev.Status != auditport.StatusFailure {
		t.Fatalf("ev = %+v (set=%v), want a failure row", ev, set)
	}
	if !strings.Contains(ev.ErrorMessage, "provider") {
		t.Fatalf("error = %q, want the validation reason", ev.ErrorMessage)
	}
}

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }
