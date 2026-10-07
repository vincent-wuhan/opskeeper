package store

import (
	"context"
	"errors"
	"testing"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// The PiG axis, as it reaches durable storage.
//
// The compatibility matrix reads this column and nothing else: it costs one
// query on purpose, so that opening the release page never turns into a
// per-node RPC fan-out. That makes the column the whole mechanism, and a
// mechanism this small fails in a way worth pinning down — the model
// struct, the AutoMigrate registration, and the column name in the UPDATE
// are three separate places that have to agree, and a typo in any of them
// surfaces as a runtime SQL error on a live node's heartbeat rather than as
// a failed build.

// Migrate must actually create the column, and it must be its own column.
//
// Asserting the schema directly rather than only round-tripping a value is
// what distinguishes "the column exists" from "the value came back". A
// missing column fails the round trip too, but with a SQL error that reads
// like a driver problem; naming the column here turns that into a message
// that says which declaration is missing. The second column is checked
// because pig_version and agent_version are semantically different values
// that upgrade on different cadences, and an accidental tag that aliased
// them would let an edge upgrade silently overwrite the agent version —
// the kind of bug that only shows up as a node that refuses a package it
// should accept.
func TestMigrateCreatesAPigVersionColumnDistinctFromTheAgentVersion(t *testing.T) {
	repo := newTestRepo(t)

	for _, column := range []string{"pig_version", "agent_version"} {
		var found int64
		if err := repo.db.Raw(
			"SELECT count(*) FROM pragma_table_info('edges') WHERE name = ?", column,
		).Scan(&found).Error; err != nil {
			t.Fatalf("inspect schema for %s: %v", column, err)
		}
		if found != 1 {
			t.Errorf("edges table has %d columns named %q, want exactly 1 — "+
				"check the gorm tag on model.Edge and that Migrate lists the model", found, column)
		}
	}
}

// The value round-trips, bumps, and refuses a missing row.
//
// Same shape as the agent-version test it sits beside, and deliberately
// the same shape: the two columns have the same contract, and a reader
// comparing them should find nothing to reconcile. ErrNotFound in
// particular is load-bearing rather than incidental — the heartbeat path
// writes after a liveness update, so a row that vanished underneath it
// must be distinguishable from a row that was never there.
func TestSQLiteSetPigVersion(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	e := &model.Edge{
		Name:          "edge-pig",
		AccessKeyID:   "ak-pppppppppppppppppppppp",
		SecretKeyHash: "$argon2id$v=19$m=65536,t=1,p=4$AAAA$BBBB",
		Status:        model.StatusOffline,
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, err := repo.GetByID(ctx, e.ID); err != nil {
		t.Fatalf("GetByID: %v", err)
	} else if got.PigVersion != "" {
		t.Errorf("initial PigVersion = %q, want empty", got.PigVersion)
	}

	if err := repo.SetPigVersion(ctx, e.ID, "0.3.0"); err != nil {
		t.Fatalf("SetPigVersion: %v", err)
	}
	got, err := repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("GetByID after SetPigVersion: %v", err)
	}
	if got.PigVersion != "0.3.0" {
		t.Errorf("PigVersion = %q, want 0.3.0", got.PigVersion)
	}
	// The two axes must not bleed into one another. An edge upgrade and a
	// pig upgrade are separate events, and a node commonly sits on
	// different builds of each for a long time.
	if got.AgentVersion != "" {
		t.Errorf("AgentVersion = %q after a pig-only write, want it untouched at empty", got.AgentVersion)
	}

	if err := repo.SetPigVersion(ctx, e.ID, "0.4.0"); err != nil {
		t.Fatalf("SetPigVersion bump: %v", err)
	}
	got, _ = repo.GetByID(ctx, e.ID)
	if got.PigVersion != "0.4.0" {
		t.Errorf("PigVersion = %q after bump, want 0.4.0", got.PigVersion)
	}

	if err := repo.SetPigVersion(ctx, 9999, "0.3.0"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("SetPigVersion missing id err = %v, want ErrNotFound", err)
	}
}
