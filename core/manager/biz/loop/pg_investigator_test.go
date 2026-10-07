package loop

import (
	"context"
	"testing"
)

func TestPGStatInvestigatorToolset_RequiresDSN(t *testing.T) {
	if _, err := NewPGStatInvestigatorToolset(""); err == nil {
		t.Fatal("err = nil, want DSN required")
	}
}

func TestPGStatInvestigatorToolset_ListPGRemediation(t *testing.T) {
	toolset, err := NewPGStatInvestigatorToolset("postgres://user:pass@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	options, err := toolset.ListRemediations(context.Background(), "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].Action != "pg_terminate_backend" || options[0].Risk != "mutating" {
		t.Fatalf("options = %+v", options)
	}
}
