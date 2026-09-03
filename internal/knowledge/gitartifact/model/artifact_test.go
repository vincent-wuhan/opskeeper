package model

import (
	"strings"
	"testing"
	"time"
)

func TestArtifact_Validate_OK(t *testing.T) {
	a := &Artifact{
		RepoURL:     "https://github.com/x/y",
		Commit:      "abc123def456789012345678901234567890abcd",
		Branch:      "main",
		ArtifactURL: "s3://x/y",
		Meta:        map[string]interface{}{"build_id": "b1"},
		BuildAt:     time.Now(),
	}
	if err := a.Validate(); err != nil {
		t.Errorf("expected OK, got %v", err)
	}
}

func TestArtifact_Validate_SHA64(t *testing.T) {
	sha64 := strings.Repeat("a", 64)
	a := &Artifact{Commit: sha64, RepoURL: "x", Branch: "b", ArtifactURL: "u", Meta: map[string]interface{}{"build_id": "x"}, BuildAt: time.Now()}
	if err := a.Validate(); err != nil {
		t.Errorf("SHA-64 should be OK: %v", err)
	}
}

func TestArtifact_Validate_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Artifact)
	}{
		{"no_repo", func(a *Artifact) { a.RepoURL = "" }},
		{"short_commit", func(a *Artifact) { a.Commit = "abc" }},
		{"no_branch", func(a *Artifact) { a.Branch = "" }},
		{"no_artifact_url", func(a *Artifact) { a.ArtifactURL = "" }},
		{"no_build_at", func(a *Artifact) { a.BuildAt = time.Time{} }},
		{"no_meta", func(a *Artifact) { a.Meta = nil }},
		{"no_build_id", func(a *Artifact) { a.Meta = map[string]interface{}{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Artifact{
				RepoURL:     "x",
				Commit:      strings.Repeat("a", 40),
				Branch:      "b",
				ArtifactURL: "u",
				Meta:        map[string]interface{}{"build_id": "x"},
				BuildAt:     time.Now(),
			}
			tc.mut(a)
			if err := a.Validate(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestArtifact_BuildID(t *testing.T) {
	a := &Artifact{Meta: map[string]interface{}{"build_id": "gh-12345"}}
	if got := a.BuildID(); got != "gh-12345" {
		t.Errorf("BuildID = %q, want gh-12345", got)
	}
	a2 := &Artifact{}
	if got := a2.BuildID(); got != "" {
		t.Errorf("BuildID (no meta) = %q, want empty", got)
	}
}

func TestIndexStatus_Constants(t *testing.T) {
	cases := []struct {
		s        IndexStatus
		expected string
	}{
		{IndexStatusQueued, "queued"},
		{IndexStatusRunning, "running"},
		{IndexStatusCompleted, "completed"},
		{IndexStatusFailed, "failed"},
		{IndexStatusStale, "stale"},
	}
	for _, tc := range cases {
		if string(tc.s) != tc.expected {
			t.Errorf("IndexStatus %q != %q", tc.s, tc.expected)
		}
	}
}
