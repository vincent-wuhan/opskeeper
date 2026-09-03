package middleware

import (
	"testing"
)

func TestGitArtifactTableName(t *testing.T) {
	if got, want := (GitArtifact{}).TableName(), "git_artifacts"; got != want {
		t.Errorf("GitArtifact.TableName() = %q, want %q", got, want)
	}
	if got, want := (RuntimeSymbolLink{}).TableName(), "runtime_symbol_links"; got != want {
		t.Errorf("RuntimeSymbolLink.TableName() = %q, want %q", got, want)
	}
}

func TestSymbolTypeConstants(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{SymbolTypePGQuery, "pg_query"},
		{SymbolTypeRedisCmd, "redis_cmd"},
		{SymbolTypeK8sImage, "k8s_image"},
		{SymbolTypeHTTPRoute, "http_route"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("SymbolType = %q, want %q", c.got, c.want)
		}
	}
}

func TestIndexStatusConstants(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{IndexStatusQueued, "queued"},
		{IndexStatusRunning, "running"},
		{IndexStatusCompleted, "completed"},
		{IndexStatusFailed, "failed"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("IndexStatus = %q, want %q", c.got, c.want)
		}
	}
}
