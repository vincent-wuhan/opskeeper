package vecstore

import (
	"log/slog"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
)

func nopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// nopFactory satisfies Factory without building a store; the registry tests
// care about registration semantics, not about a live backend.
func nopFactory(Config, *slog.Logger) (qdrantx.Store, error) { return nil, nil }

func TestRegisteredBackendsCoversBuiltins(t *testing.T) {
	got := RegisteredBackends()
	want := map[string]bool{"qdrant": true, "chromem": true}
	for _, name := range got {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("these built-in backends have no factory: %v", want)
	}
}

func TestRegisterBackendRefusesDuplicate(t *testing.T) {
	err := RegisterBackend("chromem", nopFactory)
	if err == nil {
		t.Fatal("registering over a built-in backend must be refused")
	}
	// The built-in must survive the refused takeover.
	if _, ok := func() (Factory, bool) {
		mu.RLock()
		defer mu.RUnlock()
		f, ok := backends["chromem"]
		return f, ok
	}(); !ok {
		t.Fatal("the built-in chromem factory must survive a refused registration")
	}
}

func TestRegisterBackendRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterBackend("", nopFactory); err == nil {
		t.Fatal("an empty backend name must be refused")
	}
	if err := RegisterBackend("brand-new", nil); err == nil {
		t.Fatal("a nil factory must be refused")
	}
}

func TestBuildEmptyNameMeansDefaultQdrant(t *testing.T) {
	s, err := Build("", Config{QdrantURL: "http://example.test:6333"}, nopLogger())
	if err != nil {
		t.Fatalf("build default: %v", err)
	}
	if s == nil {
		t.Fatal("the default backend must build")
	}
}

func TestBuildUnknownErrors(t *testing.T) {
	_, err := Build("does-not-exist", Config{}, nopLogger())
	if err == nil {
		t.Fatal("an unknown backend must be refused")
	}
}

func TestBuildChromem(t *testing.T) {
	s, err := Build("chromem", Config{ChromemDir: t.TempDir()}, nopLogger())
	if err != nil {
		t.Fatalf("build chromem: %v", err)
	}
	if s == nil {
		t.Fatal("the chromem backend must build")
	}
}

func TestRegisterThenBuildThirdParty(t *testing.T) {
	name := "third-party-vector-test"
	if _, err := Build(name, Config{}, nopLogger()); err == nil {
		t.Skip("backend already registered by another test")
	}
	// A third-party backend is a registration, not an edit to the assembly:
	// pgvector, Milvus and Weaviate would land here the same way.
	if err := RegisterBackend(name, nopFactory); err != nil {
		t.Fatalf("register: %v", err)
	}
	s, err := Build(name, Config{}, nopLogger())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_ = s
}
