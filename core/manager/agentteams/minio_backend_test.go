package agentteams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMinIOBackend_Get404ReturnsErrStateNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	u := srv.URL[7:] // strip "http://"
	b := NewMinIOBackend(u, "ak", "sk", "test-bucket", false)
	_, err := b.Get(context.Background(), "task-1")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if err != ErrStateNotFound {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestMinIOBackend_PutRoundTrip(t *testing.T) {
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			stored = buf[:n]
			// verify signature header exists
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stored)
		}
	}))
	defer srv.Close()

	u := srv.URL[7:]
	b := NewMinIOBackend(u, "ak", "sk", "test-bucket", false)
	ctx := context.Background()

	body := []byte(`{"task_id":"incident-1","phase":"rca"}`)
	if err := b.Put(ctx, "incident-1", body); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := b.Get(ctx, "incident-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, body)
	}
}
