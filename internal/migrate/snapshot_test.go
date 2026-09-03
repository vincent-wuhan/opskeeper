package migrate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshot_WriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// 写
	snap := NewSnapshot("http://opskeeper.test", "v0.0.1", []TenantMap{
		{OpsKeeperProjectID: 42, OpskeeperTenantID: 1},
	})
	snap.PutEntity(EntityUsers, []map[string]any{
		{"id": 1, "email": "alice@example.com"},
		{"id": 2, "email": "bob@example.com"},
	})
	snap.PutEntity(EntityProjects, []map[string]any{
		{"id": 42, "name": "test-project", "owner_id": 1},
	})

	// .json
	path1 := filepath.Join(dir, "snap.json")
	if err := snap.WriteTo(path1); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	got1, err := ReadSnapshot(path1)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if got1.TotalRows() != 3 {
		t.Errorf("TotalRows=%d want 3", got1.TotalRows())
	}
	if len(got1.GetEntity(EntityUsers)) != 2 {
		t.Errorf("users count=%d want 2", len(got1.GetEntity(EntityUsers)))
	}

	// .json.gz
	path2 := filepath.Join(dir, "snap.json.gz")
	if err := snap.WriteTo(path2); err != nil {
		t.Fatalf("WriteTo gz: %v", err)
	}
	got2, err := ReadSnapshot(path2)
	if err != nil {
		t.Fatalf("ReadSnapshot gz: %v", err)
	}
	if got2.TotalRows() != 3 {
		t.Errorf("gz TotalRows=%d want 3", got2.TotalRows())
	}
}

func TestSnapshot_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	// 写一个版本不兼容的 snapshot
	content := `{"header":{"version":"v999","exported_at":"2026-01-01T00:00:00Z","source":"x"},"entities":{}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil {
		t.Error("ReadSnapshot must fail on version mismatch")
	}
}

func TestSnapshot_InspectSnapshot(t *testing.T) {
	dir := t.TempDir()
	snap := NewSnapshot("http://test", "", nil)
	snap.Header.ExportedAt = time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "inspect.json")
	if err := snap.WriteTo(path); err != nil {
		t.Fatal(err)
	}
	meta, err := InspectSnapshot(path)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}
	if meta.Header.Source != "http://test" {
		t.Errorf("Source=%s want http://test", meta.Header.Source)
	}
	if meta.Size == 0 {
		t.Error("Size must be > 0")
	}
}

func TestSnapshot_TotalRows(t *testing.T) {
	snap := NewSnapshot("x", "", nil)
	if snap.TotalRows() != 0 {
		t.Errorf("empty TotalRows=%d want 0", snap.TotalRows())
	}
	snap.PutEntity(EntityUsers, []map[string]any{{"id": 1}})
	snap.PutEntity(EntityProjects, []map[string]any{{"id": 1}, {"id": 2}})
	if snap.TotalRows() != 3 {
		t.Errorf("TotalRows=%d want 3", snap.TotalRows())
	}
}
