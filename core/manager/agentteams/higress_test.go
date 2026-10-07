package agentteams

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInferRoleFromConsumerName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"manager-alice", "manager"},
		{"worker-bob", "worker"},
		{"admin-root", "admin"},
		{"manager-qwenpaw-001", "manager"},
		{"worker-investigator", "worker"},
		{"manager", "manager"},
		{"worker", "worker"},
		{"admin", "admin"},
		{"unknown-consumer", "unknown"},
		{"", "unknown"},
	}
	for _, c := range cases {
		got := inferRoleFromConsumerName(c.name)
		if got != c.want {
			t.Errorf("inferRoleFromConsumerName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHigressHTTPClientResolveConsumer(t *testing.T) {
	var loginCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/session/login":
			loginCalled = true
			http.SetCookie(writer, &http.Cookie{Name: "_hi_sess", Value: "test-session"})
			writer.WriteHeader(http.StatusCreated)
		case "/v1/consumers":
			if !loginCalled {
				t.Error("consumer lookup happened before login")
			}
			if cookie, err := request.Cookie("_hi_sess"); err != nil || cookie.Value != "test-session" {
				t.Fatalf("consumer lookup cookie = %+v, err = %v", cookie, err)
			}
			if got := request.URL.Query().Get("apikey"); got != "test-key" {
				t.Fatalf("apikey = %q", got)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"data": []map[string]string{
					{"name": "manager"},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewHigressHTTPClient(server.URL, "admin", "password")
	consumerName, apiKeyID, role, err := client.ResolveConsumer(context.Background(), "test-key")
	if err != nil {
		t.Fatalf("ResolveConsumer() error = %v", err)
	}
	if consumerName != "manager" || apiKeyID != "" || role != "manager" {
		t.Fatalf("ResolveConsumer() = (%q, %q, %q)", consumerName, apiKeyID, role)
	}
}

func TestHigressHTTPClientResolveConsumerFiltersReturnedCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/session/login":
			http.SetCookie(writer, &http.Cookie{Name: "_hi_sess", Value: "test-session"})
			writer.WriteHeader(http.StatusCreated)
		case "/v1/consumers":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"name": "manager", "credentials": []map[string]any{{"values": []string{"manager-key"}}}},
					{"name": "opskeeper-repairer", "credentials": []map[string]any{{"values": []string{"repairer-key"}}}},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewHigressHTTPClient(server.URL, "admin", "password")
	consumerName, _, role, err := client.ResolveConsumer(context.Background(), "repairer-key")
	if err != nil {
		t.Fatalf("ResolveConsumer() error = %v", err)
	}
	if consumerName != "opskeeper-repairer" || role != "repairer" {
		t.Fatalf("ResolveConsumer() = (%q, %q)", consumerName, role)
	}
}

func TestCanonicalQuery(t *testing.T) {
	// nil query
	if got := canonicalQuery(nil); got != "" {
		t.Errorf("canonicalQuery(nil) = %q, want empty", got)
	}
	// simple query
	if got := canonicalQuery(map[string][]string{"apikey": {"abc"}}); got != "apikey=abc" {
		t.Errorf("canonicalQuery: got %q, want %q", got, "apikey=abc")
	}
}
