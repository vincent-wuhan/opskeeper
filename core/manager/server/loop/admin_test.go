// Package loop — admin_test.go: covers the test-only admin routes.
//
// Tests:
//  1. RegisterAdminRoutes no-ops when Enabled=false.
//  2. RegisterAdminRoutes no-ops when StateStore=nil.
//  3. POST .../increment increments N times + reports escalated.
//  4. POST .../increment clamps times to [1,100].
//  5. GET .../{incident_id} returns current count.
//  6. GET .../{incident_id} returns 404 on ErrNoRetryCountRow.
//  7. POST .../reset clears the count.
package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeRecoveryStateAdmin is an in-memory implementation of RecoveryStateAdmin.
type fakeRecoveryStateAdmin struct {
	mu       sync.Mutex
	counts   map[string]int
	getErr   error
	incErr   error
	resetErr error
}

func newFakeRecoveryStateAdmin() *fakeRecoveryStateAdmin {
	return &fakeRecoveryStateAdmin{counts: map[string]int{}}
}

func (f *fakeRecoveryStateAdmin) Get(_ context.Context, incidentID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return 0, f.getErr
	}
	n, ok := f.counts[incidentID]
	if !ok {
		return 0, ErrNoRetryCountRow
	}
	return n, nil
}

func (f *fakeRecoveryStateAdmin) Increment(_ context.Context, incidentID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incErr != nil {
		return 0, f.incErr
	}
	f.counts[incidentID]++
	return f.counts[incidentID], nil
}

func (f *fakeRecoveryStateAdmin) Reset(_ context.Context, incidentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resetErr != nil {
		return f.resetErr
	}
	delete(f.counts, incidentID)
	return nil
}

// buildAdminRouter wires the admin routes onto a fresh chi.Router and
// returns the router + the fake store (so tests can mutate state).
func buildAdminRouter(t *testing.T, enabled bool) (*chi.Mux, *fakeRecoveryStateAdmin) {
	t.Helper()
	store := newFakeRecoveryStateAdmin()
	r := chi.NewRouter()
	RegisterAdminRoutes(r, AdminRouteDeps{Enabled: enabled, StateStore: store})
	return r, store
}

// 1. Disabled flag → routes not registered (404 expected).
func TestRegisterAdminRoutes_Disabled(t *testing.T) {
	r, _ := buildAdminRouter(t, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/loops/recovery_state/I1/increment", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when admin disabled, got %d", w.Code)
	}
}

// 2. nil store + enabled → routes not registered.
func TestRegisterAdminRoutes_NilStore(t *testing.T) {
	r := chi.NewRouter()
	RegisterAdminRoutes(r, AdminRouteDeps{Enabled: true, StateStore: nil})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/loops/recovery_state/I1/increment", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when store nil, got %d", w.Code)
	}
}

// 3. POST .../increment N times + reports escalated=true when count > 3.
func TestAdminIncrementRetryCount_Escalated(t *testing.T) {
	r, store := buildAdminRouter(t, true)
	body, _ := json.Marshal(IncrementRequest{Times: 4})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/loops/recovery_state/INC-ESC/increment", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp IncrementResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if resp.RetryCount != 4 {
		t.Errorf("retry_count = %d, want 4", resp.RetryCount)
	}
	if !resp.Escalated {
		t.Errorf("escalated = false, want true (4 > MaxRetryCount=3)")
	}
	if resp.Incremented != 4 {
		t.Errorf("incremented = %d, want 4", resp.Incremented)
	}
	if store.counts["INC-ESC"] != 4 {
		t.Errorf("store count = %d, want 4", store.counts["INC-ESC"])
	}
}

// 4. POST .../increment clamps times to [1,100].
func TestAdminIncrementRetryCount_ClampsTimes(t *testing.T) {
	r, _ := buildAdminRouter(t, true)
	for _, tt := range []struct {
		times     int
		wantCount int
		wantCode  int
	}{
		{times: 0, wantCount: 1, wantCode: http.StatusOK},           // default 1
		{times: -5, wantCount: 1, wantCode: http.StatusOK},          // default 1
		{times: 200, wantCount: 0, wantCode: http.StatusBadRequest}, // rejected
	} {
		body, _ := json.Marshal(IncrementRequest{Times: tt.times})
		req := httptest.NewRequest(http.MethodPost,
			"/v1/admin/loops/recovery_state/I-CLAMP/increment", bytes.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tt.wantCode {
			t.Errorf("times=%d: status=%d want=%d body=%s", tt.times, w.Code, tt.wantCode, w.Body.String())
		}
	}
}

// 5. GET .../{incident_id} returns current count.
func TestAdminGetRetryCount_OK(t *testing.T) {
	r, store := buildAdminRouter(t, true)
	store.counts["I-OK"] = 7
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/loops/recovery_state/I-OK", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp GetRetryCountResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RetryCount != 7 {
		t.Errorf("retry_count = %d, want 7", resp.RetryCount)
	}
}

// 6. GET .../{incident_id} returns 404 on ErrNoRetryCountRow.
func TestAdminGetRetryCount_NotFound(t *testing.T) {
	r, _ := buildAdminRouter(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/loops/recovery_state/UNKNOWN", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// 7. POST .../reset clears the count.
func TestAdminResetRetryCount(t *testing.T) {
	r, store := buildAdminRouter(t, true)
	store.counts["I-RESET"] = 9
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/loops/recovery_state/I-RESET/reset", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if store.counts["I-RESET"] != 0 {
		t.Errorf("after reset, count = %d, want 0", store.counts["I-RESET"])
	}
}

// bonus: error from Increment → 500.
func TestAdminIncrementRetryCount_StoreError(t *testing.T) {
	r, store := buildAdminRouter(t, true)
	store.incErr = errors.New("db gone")
	body, _ := json.Marshal(IncrementRequest{Times: 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/loops/recovery_state/ERR/increment", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
