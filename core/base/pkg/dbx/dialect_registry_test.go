package dbx

import (
	"log/slog"
	"testing"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

func TestRegisteredDialectsCoversBuiltins(t *testing.T) {
	got := RegisteredDialects()
	want := map[string]bool{"mysql": true, "sqlite": true, "postgresql": true}
	for _, name := range got {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("these built-in dialects have no factory: %v", want)
	}
}

func TestRegisterDialectRefusesDuplicate(t *testing.T) {
	err := RegisterDialect("sqlite", func(config.DBConfig, *slog.Logger) (*gorm.DB, error) { return nil, nil })
	if err == nil {
		t.Fatal("registering over a built-in dialect must be refused")
	}
}

func TestRegisterDialectRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterDialect("", nil); err == nil {
		t.Fatal("an empty dialect name must be refused")
	}
	if err := RegisterDialect("brand-new", nil); err == nil {
		t.Fatal("a nil factory must be refused")
	}
}

func TestOpenDispatchesThroughTheRegistry(t *testing.T) {
	// A dialect registered by a package opens through the same path a
	// built-in does. The factory returns a nil db on purpose: this test is
	// about dispatch, not about a live connection.
	name := "third-party-dialect-test"
	cfg := config.DBConfig{Dialect: name}
	if _, err := Open(cfg, nil); err == nil {
		t.Skip("dialect already registered by another test")
	}
	if err := RegisterDialect(name, func(config.DBConfig, *slog.Logger) (*gorm.DB, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	db, err := Open(cfg, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if db != nil {
		t.Fatal("the factory's return value must be passed through unchanged")
	}
}

func TestResolveDialectAliases(t *testing.T) {
	// The spellings the operator config file may use must all reach the
	// same canonical factory — this is the behaviour the switch used to
	// carry, and it is what the existing Open tests assert.
	for _, spelling := range []string{"sqlite", "", "SQLite"} {
		f, ok := resolveDialect(config.DBConfig{Dialect: spelling})
		if !ok || f == nil {
			t.Fatalf("dialect %q must resolve", spelling)
		}
	}
	for _, spelling := range []string{"pg", "postgres", "postgresql"} {
		if _, ok := resolveDialect(config.DBConfig{Dialect: spelling}); !ok {
			t.Fatalf("dialect %q must resolve", spelling)
		}
	}
	if _, ok := resolveDialect(config.DBConfig{Dialect: "does-not-exist"}); ok {
		t.Fatal("an unknown dialect must not resolve")
	}
}
