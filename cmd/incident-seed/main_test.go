package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSeedFourDatasets(t *testing.T) {
	datasets, events, err := loadSeed(filepath.Join("..", "..", "deploy", "incident-events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(datasets) != 4 {
		t.Fatalf("datasets = %d, want 4", len(datasets))
	}
	if len(events) != 24 {
		t.Fatalf("events = %d, want 24", len(events))
	}
	if seedTenant(datasets) != "opskeeper-demo" {
		t.Fatalf("tenant = %q, want opskeeper-demo", seedTenant(datasets))
	}
}

func TestLoadSeedRejectsInvalidTimeline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "invalid.json"), []byte(`{"id":"invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadSeed(dir); err == nil {
		t.Fatal("expected invalid dataset to fail")
	}
}
