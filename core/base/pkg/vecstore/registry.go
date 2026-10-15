package vecstore

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
)

// defaultQdrantURL is the docker-internal address the compose stack's
// qdrant service listens on. Used when the operator left the URL empty.
const defaultQdrantURL = "http://qdrant:6333"

// Config carries the material any backend might need. A backend that
// ignores a field ignores it explicitly; the struct is uniform so the
// registry can hold every factory behind one signature.
type Config struct {
	// QdrantURL is the external qdrant endpoint. Empty means the compose
	// default.
	QdrantURL string
	// ChromemDir is the on-disk directory the embedded store persists to.
	ChromemDir string
}

// Factory builds one backend.
type Factory func(cfg Config, log *slog.Logger) (qdrantx.Store, error)

var (
	mu       sync.RWMutex
	backends = map[string]Factory{
		// qdrant is the default: an external deployment that several
		// manager replicas can share.
		"qdrant": func(cfg Config, log *slog.Logger) (qdrantx.Store, error) {
			url := cfg.QdrantURL
			if url == "" {
				url = defaultQdrantURL
			}
			return qdrantx.New(url, log), nil
		},
		// chromem is the embedded default for a fresh checkout: no external
		// service, state on local disk.
		"chromem": func(cfg Config, log *slog.Logger) (qdrantx.Store, error) {
			return NewChromem(cfg.ChromemDir, log)
		},
	}
)

// RegisterBackend registers a vector store backend by name.
//
// It refuses a duplicate rather than replacing, and an empty name, for the
// same reason the microkernel registry does: a silent replace is how one
// backend takes over another's name, and a deployment that keeps the
// backend name it always used would never see the implementation change.
func RegisterBackend(name string, f Factory) error {
	if name == "" {
		return fmt.Errorf("vecstore: backend name is empty")
	}
	if f == nil {
		return fmt.Errorf("vecstore: backend %q has no implementation", name)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := backends[name]; ok {
		return fmt.Errorf("vecstore: backend %q is already registered", name)
	}
	backends[name] = f
	return nil
}

// Build constructs the named backend. An empty name means the default
// (external qdrant), which is what a deployment that never set
// OPSKEEPER_VECTOR_BACKEND gets.
func Build(name string, cfg Config, log *slog.Logger) (qdrantx.Store, error) {
	if name == "" {
		name = "qdrant"
	}
	mu.RLock()
	f, ok := backends[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("vecstore: unknown backend %q (registered: %v)", name, RegisteredBackends())
	}
	return f(cfg, log)
}

// RegisteredBackends returns the backend names a factory is registered for,
// sorted. It is what a diagnostics page lists, derived from the registry so
// it cannot drift from what Build will actually construct.
func RegisteredBackends() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(backends))
	for name := range backends {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
