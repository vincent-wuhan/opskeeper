package topology_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	biz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/topology"
	store "github.com/vincent-wuhan/opskeeper/internal/manager/data/topology/store"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/topology"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

func newUC(t *testing.T) *biz.Usecase {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return biz.NewUsecase(store.NewNodeRepo(db), store.NewRelationRepo(db), store.NewRelationTypeRepo(db), store.NewNodeTypeRepo(db), nil)
}

func TestCreateNodeValidation(t *testing.T) {
	uc := newUC(t)
	ctx := context.Background()

	if _, err := uc.CreateNode(ctx, "", "x", ""); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("missing type: want ErrInvalid, got %v", err)
	}
	if _, err := uc.CreateNode(ctx, "service", "", ""); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("missing name: want ErrInvalid, got %v", err)
	}
	if _, err := uc.CreateNode(ctx, "service", "x", "not-json"); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("invalid json: want ErrInvalid, got %v", err)
	}
	n, err := uc.CreateNode(ctx, "service", "order-api", `{"team":"pay"}`)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if n.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}
}

func TestCreateRelationValidates(t *testing.T) {
	uc := newUC(t)
	ctx := context.Background()

	a, _ := uc.CreateNode(ctx, "service", "a", "")
	b, _ := uc.CreateNode(ctx, "service", "b", "")

	// Endpoints must exist.
	if _, err := uc.CreateRelation(ctx, a.ID, 9999, model.RelDependsOn, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing dst: want ErrNotFound, got %v", err)
	}

	// Self-edge rejected.
	if _, err := uc.CreateRelation(ctx, a.ID, a.ID, model.RelDependsOn, ""); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("self-edge: want ErrInvalid, got %v", err)
	}

	// Unknown relation type rejected.
	if _, err := uc.CreateRelation(ctx, a.ID, b.ID, "made_up_thing", ""); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown type: want ErrNotFound (via RT lookup), got %v", err)
	}

	// Happy path.
	r, err := uc.CreateRelation(ctx, a.ID, b.ID, model.RelDependsOn, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == 0 || r.Type != model.RelDependsOn {
		t.Fatalf("bad relation: %+v", r)
	}
}

func TestRegisterRelationType(t *testing.T) {
	uc := newUC(t)
	ctx := context.Background()

	// Built-in collision.
	_, err := uc.RegisterRelationType(ctx, model.RelationType{
		Name:         model.RelDependsOn,
		Direction:    string(model.DirectionDstToSrc),
		SemanticsTag: string(model.SemanticsHardDep),
	})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("builtin collision: want ErrConflict, got %v", err)
	}

	// Bad direction.
	_, err = uc.RegisterRelationType(ctx, model.RelationType{
		Name:         "shares_storage_with",
		Direction:    "no_such_direction",
		SemanticsTag: string(model.SemanticsRedundancy),
	})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("bad direction: want ErrInvalid, got %v", err)
	}

	// Bad semantics tag.
	_, err = uc.RegisterRelationType(ctx, model.RelationType{
		Name:         "shares_storage_with",
		Direction:    string(model.DirectionBidirectional),
		SemanticsTag: "weird_bucket",
	})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("bad tag: want ErrInvalid, got %v", err)
	}

	// Happy path — custom type registered.
	rt, err := uc.RegisterRelationType(ctx, model.RelationType{
		Name:              "shares_storage_with",
		DisplayName:       "共享存储",
		PropagatesFailure: true,
		Direction:         string(model.DirectionBidirectional),
		SemanticsTag:      string(model.SemanticsRedundancy),
		Description:       "两节点挂同一块 NAS / SAN; 一方掉盘另一方也会受影响.",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rt.Builtin {
		t.Fatalf("expected Builtin=false for operator-registered")
	}
}

func TestDeleteRelationTypeGuards(t *testing.T) {
	uc := newUC(t)
	ctx := context.Background()

	// Built-in rejected.
	if err := uc.DeleteRelationType(ctx, model.RelMemberOf); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("delete builtin: want ErrConflict, got %v", err)
	}

	// Register + use + try delete (should refuse because referenced).
	if _, err := uc.RegisterRelationType(ctx, model.RelationType{
		Name: "owns", Direction: string(model.DirectionSrcToDst),
		SemanticsTag: string(model.SemanticsAnnotation),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	a, _ := uc.CreateNode(ctx, "service", "a", "")
	b, _ := uc.CreateNode(ctx, "service", "b", "")
	if _, err := uc.CreateRelation(ctx, a.ID, b.ID, "owns", ""); err != nil {
		t.Fatalf("create relation: %v", err)
	}
	if err := uc.DeleteRelationType(ctx, "owns"); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("delete with refs: want ErrConflict, got %v", err)
	}
}
