package hitl

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRegisterAddsRoutesWithoutLateMiddleware(t *testing.T) {
	router := chi.NewMux()
	NewHandler(nil).Register(router)

	request := httptest.NewRequest(http.MethodPost, "/v1/hitl/proposals", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
