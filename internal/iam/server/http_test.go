package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	biz "github.com/vincent-wuhan/opskeeper/internal/iam/biz/user"
	iammodel "github.com/vincent-wuhan/opskeeper/internal/iam/model"
	"github.com/vincent-wuhan/opskeeper/internal/iam/service"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

type serverTestRepo struct {
	users []*iammodel.User
}

func newServerTestRepo(t *testing.T) *serverTestRepo {
	t.Helper()
	return &serverTestRepo{}
}

func (r *serverTestRepo) Create(_ context.Context, user *iammodel.User) error {
	user.ID = uint64(len(r.users) + 1)
	copy := *user
	r.users = append(r.users, &copy)
	return nil
}

func (r *serverTestRepo) GetByEmail(_ context.Context, email string) (*iammodel.User, error) {
	for _, user := range r.users {
		if user.Email == email {
			return copyUser(user), nil
		}
	}
	return nil, errs.ErrNotFound
}

func (r *serverTestRepo) GetByID(_ context.Context, id uint64) (*iammodel.User, error) {
	for _, user := range r.users {
		if user.ID == id {
			return copyUser(user), nil
		}
	}
	return nil, errs.ErrNotFound
}

func (r *serverTestRepo) List(_ context.Context) ([]*iammodel.User, error) {
	users := make([]*iammodel.User, 0, len(r.users))
	for _, user := range r.users {
		users = append(users, copyUser(user))
	}
	return users, nil
}

func (r *serverTestRepo) Count(_ context.Context) (int64, error) { return int64(len(r.users)), nil }
func (r *serverTestRepo) Delete(_ context.Context, id uint64) error {
	for index, user := range r.users {
		if user.ID == id {
			r.users = append(r.users[:index], r.users[index+1:]...)
			return nil
		}
	}
	return errs.ErrNotFound
}
func (r *serverTestRepo) UpdateRole(_ context.Context, id uint64, role string) error {
	user, err := r.GetByID(context.Background(), id)
	if err != nil {
		return err
	}
	user.Role = role
	return r.updateByID(user)
}
func (r *serverTestRepo) UpdateProfile(_ context.Context, id uint64, displayName, phone string) error {
	user, err := r.GetByID(context.Background(), id)
	if err != nil {
		return err
	}
	user.DisplayName = displayName
	user.Phone = phone
	return r.updateByID(user)
}
func (r *serverTestRepo) UpdateStatus(_ context.Context, id uint64, status string) error {
	user, err := r.GetByID(context.Background(), id)
	if err != nil {
		return err
	}
	user.Status = status
	return r.updateByID(user)
}
func (r *serverTestRepo) UpdateSuperuser(_ context.Context, id uint64, superuser bool) error {
	user, err := r.GetByID(context.Background(), id)
	if err != nil {
		return err
	}
	user.IsSuperuser = superuser
	return r.updateByID(user)
}
func (r *serverTestRepo) UpdatePassHash(_ context.Context, id uint64, hash string) error {
	user, err := r.GetByID(context.Background(), id)
	if err != nil {
		return err
	}
	user.PassHash = hash
	return r.updateByID(user)
}
func (r *serverTestRepo) updateByID(target *iammodel.User) error {
	for index, user := range r.users {
		if user.ID == target.ID {
			r.users[index] = target
			return nil
		}
	}
	return errs.ErrNotFound
}
func copyUser(user *iammodel.User) *iammodel.User {
	copy := *user
	return &copy
}

func TestIssueAgentTeamsToken_RequiresAdminAndSignsRoleScope(t *testing.T) {
	signer := auth.NewSigner("test-secret", 15*time.Minute, 24*time.Hour)
	usecase := biz.NewUsecase(newServerTestRepo(t), signer, nil)
	if err := usecase.BootstrapAdmin(t.Context(), "root@example.com", "admin-password"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	handler := NewHandler(service.New(usecase, nil), nil)
	router := chi.NewRouter()
	handler.RegisterPublic(router)
	router.Group(func(protected chi.Router) {
		protected.Use(auth.Middleware(signer))
		handler.RegisterProtected(protected)
	})

	adminToken := loginForTest(t, router, "root@example.com", "admin-password")
	request := httptest.NewRequest(http.MethodPost, "/v1/agentteams/token", bytes.NewBufferString(`{
		"tenant_id":"tenant-a","worker":"opskeeper-investigator","role":"investigator",
		"allowed_tools":["loop.investigate"],"ttl_seconds":300
	}`))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("X-Opskeeper-Version", "v1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	response := struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
		TokenType string `json:"token_type"`
	}{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	claims, err := signer.Verify(response.Token)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if claims.AgentTeams == nil || claims.AgentTeams.Worker != "opskeeper-investigator" ||
		claims.AgentTeams.Role != "investigator" || claims.TokenType != auth.AgentTeamsTokenType {
		t.Fatalf("claims = %+v", claims)
	}
	if response.ExpiresIn != 300 || response.TokenType != "Bearer" {
		t.Fatalf("response = %+v", response)
	}
}

func TestIssueAgentTeamsToken_RejectsUserRole(t *testing.T) {
	signer := auth.NewSigner("test-secret", 15*time.Minute, 24*time.Hour)
	usecase := biz.NewUsecase(newServerTestRepo(t), signer, nil)
	if err := usecase.BootstrapAdmin(t.Context(), "root@example.com", "admin-password"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	if _, err := usecase.Register(t.Context(), "user@example.com", "user-password", "user"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	handler := NewHandler(service.New(usecase, nil), nil)
	router := chi.NewRouter()
	handler.RegisterPublic(router)
	router.Group(func(protected chi.Router) {
		protected.Use(auth.Middleware(signer))
		handler.RegisterProtected(protected)
	})

	userToken := loginForTest(t, router, "user@example.com", "user-password")
	request := httptest.NewRequest(http.MethodPost, "/v1/agentteams/token", bytes.NewBufferString(`{
		"tenant_id":"tenant-a","worker":"opskeeper-investigator","role":"investigator",
		"allowed_tools":["loop.investigate"]
	}`))
	request.Header.Set("Authorization", "Bearer "+userToken)
	request.Header.Set("X-Opskeeper-Version", "v1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestIssueAgentTeamsToken_InvalidRoleDoesNotPanic(t *testing.T) {
	signer := auth.NewSigner("test-secret", 15*time.Minute, 24*time.Hour)
	usecase := biz.NewUsecase(newServerTestRepo(t), signer, nil)
	if err := usecase.BootstrapAdmin(t.Context(), "root@example.com", "admin-password"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	handler := NewHandler(service.New(usecase, nil), nil)
	router := chi.NewRouter()
	handler.RegisterPublic(router)
	router.Group(func(protected chi.Router) {
		protected.Use(auth.Middleware(signer))
		handler.RegisterProtected(protected)
	})
	adminToken := loginForTest(t, router, "root@example.com", "admin-password")
	request := httptest.NewRequest(http.MethodPost, "/v1/agentteams/token", bytes.NewBufferString(`{
		"tenant_id":"tenant-a","worker":"worker-1","role":"unknown",
		"allowed_tools":["loop.investigate"]
	}`))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("X-Opskeeper-Version", "v1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func loginForTest(t *testing.T, router http.Handler, email, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal login: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	response := struct {
		AccessToken string `json:"access_token"`
	}{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return response.AccessToken
}
