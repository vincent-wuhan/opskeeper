package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
)

func sampleArtifact(id string) *model.Artifact {
	return &model.Artifact{
		ID:          id,
		PublicID:    id,
		RepoURL:     "https://github.com/x/y",
		Commit:      strings.Repeat("a", 40),
		Branch:      "main",
		ArtifactURL: "s3://x/y",
		Meta:        map[string]interface{}{"build_id": "b-" + id},
		BuildAt:     time.Now(),
		IndexStatus: model.IndexStatusQueued,
	}
}

// --- MemoryStore ---

func TestMemoryStore_PutGet(t *testing.T) {
	s := NewMemoryStore()
	a := sampleArtifact("ga-1")
	if err := s.Put(context.Background(), a); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "ga-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PublicID != a.PublicID {
		t.Errorf("PublicID = %q, want %q", got.PublicID, a.PublicID)
	}
}

func TestMemoryStore_Get_NotFound(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Get(context.Background(), "missing")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestMemoryStore_Put_Validates(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Put(context.Background(), nil); err == nil {
		t.Error("expected error on nil")
	}
	if err := s.Put(context.Background(), &model.Artifact{}); err == nil {
		t.Error("expected error on empty PublicID")
	}
}

func TestMemoryStore_Put_Overwrites(t *testing.T) {
	s := NewMemoryStore()
	a := sampleArtifact("ga-1")
	s.Put(context.Background(), a)
	a.IndexStatus = model.IndexStatusCompleted
	s.Put(context.Background(), a)
	got, _ := s.Get(context.Background(), "ga-1")
	if got.IndexStatus != model.IndexStatusCompleted {
		t.Errorf("IndexStatus = %q, want completed", got.IndexStatus)
	}
}

func TestMemoryStore_Delete(t *testing.T) {
	s := NewMemoryStore()
	s.Put(context.Background(), sampleArtifact("ga-1"))
	if err := s.Delete(context.Background(), "ga-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := s.Get(context.Background(), "ga-1")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound after delete, got %T", err)
	}
}

func TestMemoryStore_Delete_NotFound(t *testing.T) {
	s := NewMemoryStore()
	err := s.Delete(context.Background(), "missing")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestMemoryStore_List_FilterByTenant(t *testing.T) {
	s := NewMemoryStore()
	a1 := sampleArtifact("ga-1")
	a1.TenantID = 1
	a2 := sampleArtifact("ga-2")
	a2.TenantID = 2
	s.Put(context.Background(), a1)
	s.Put(context.Background(), a2)
	list, _ := s.List(context.Background(), ListFilter{TenantID: 1})
	if len(list) != 1 {
		t.Errorf("len = %d, want 1", len(list))
	}
}

func TestMemoryStore_List_FilterByBranch(t *testing.T) {
	s := NewMemoryStore()
	a1 := sampleArtifact("ga-1")
	a1.Branch = "main"
	a2 := sampleArtifact("ga-2")
	a2.Branch = "feature"
	s.Put(context.Background(), a1)
	s.Put(context.Background(), a2)
	list, _ := s.List(context.Background(), ListFilter{Branch: "main"})
	if len(list) != 1 {
		t.Errorf("len = %d, want 1", len(list))
	}
}

func TestMemoryStore_List_SortDescByBuildAt(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	a1 := sampleArtifact("ga-1")
	a1.BuildAt = now
	a2 := sampleArtifact("ga-2")
	a2.BuildAt = now.Add(-time.Hour) // older
	s.Put(context.Background(), a1)
	s.Put(context.Background(), a2)
	list, _ := s.List(context.Background(), ListFilter{})
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].PublicID != "ga-1" {
		t.Errorf("first = %q, want ga-1 (newer)", list[0].PublicID)
	}
}

func TestMemoryStore_List_Limit(t *testing.T) {
	s := NewMemoryStore()
	for i := 0; i < 10; i++ {
		s.Put(context.Background(), sampleArtifact("ga-"+string(rune('a'+i))))
	}
	list, _ := s.List(context.Background(), ListFilter{Limit: 3})
	if len(list) != 3 {
		t.Errorf("len = %d, want 3 (limit)", len(list))
	}
}

func TestMemoryStore_Size(t *testing.T) {
	s := NewMemoryStore()
	if size, _ := s.Size(context.Background()); size != 0 {
		t.Errorf("empty size = %d, want 0", size)
	}
	s.Put(context.Background(), sampleArtifact("ga-1"))
	s.Put(context.Background(), sampleArtifact("ga-2"))
	if size, _ := s.Size(context.Background()); size != 2 {
		t.Errorf("size = %d, want 2", size)
	}
}

// --- JSONFileStore ---

func TestJSONFileStore_PutGet_Persists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifacts.json")
	s, err := NewJSONFileStore(path)
	if err != nil {
		t.Fatalf("NewJSONFileStore: %v", err)
	}
	defer s.Close()
	a := sampleArtifact("ga-1")
	if err := s.Put(context.Background(), a); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 重新加载验证持久化
	s2, err := NewJSONFileStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer s2.Close()
	got, err := s2.Get(context.Background(), "ga-1")
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if got.PublicID != "ga-1" {
		t.Errorf("PublicID = %q, want ga-1", got.PublicID)
	}
}

func TestJSONFileStore_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewJSONFileStore(filepath.Join(dir, "x.json"))
	defer s.Close()
	_, err := s.Get(context.Background(), "missing")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T", err)
	}
}

func TestJSONFileStore_Delete_Persists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	s, _ := NewJSONFileStore(path)
	defer s.Close()
	s.Put(context.Background(), sampleArtifact("ga-1"))
	s.Delete(context.Background(), "ga-1")
	// 重新加载
	s2, _ := NewJSONFileStore(path)
	defer s2.Close()
	_, err := s2.Get(context.Background(), "ga-1")
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound after delete+reload, got %T", err)
	}
}

func TestJSONFileStore_Load_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewJSONFileStore(path)
	if err == nil {
		t.Error("expected error on corrupt file")
	}
}

func TestJSONFileStore_Load_NonExistent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONFileStore(filepath.Join(dir, "new.json"))
	if err != nil {
		t.Errorf("expected OK for non-existent file, got %v", err)
	}
	if s == nil {
		t.Error("expected non-nil store")
	}
}

func TestJSONFileStore_List(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewJSONFileStore(filepath.Join(dir, "a.json"))
	defer s.Close()
	s.Put(context.Background(), sampleArtifact("ga-1"))
	s.Put(context.Background(), sampleArtifact("ga-2"))
	list, _ := s.List(context.Background(), ListFilter{})
	if len(list) != 2 {
		t.Errorf("len = %d, want 2", len(list))
	}
}

func TestJSONFileStore_Size(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewJSONFileStore(filepath.Join(dir, "a.json"))
	defer s.Close()
	if size, _ := s.Size(context.Background()); size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
	s.Put(context.Background(), sampleArtifact("ga-1"))
	if size, _ := s.Size(context.Background()); size != 1 {
		t.Errorf("size = %d, want 1", size)
	}
}
