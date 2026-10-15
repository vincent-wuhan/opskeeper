package kernel

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrDuplicate is returned when a name is already registered for a kind.
//
// It is an error and not a replace because a silent replace is how one
// plugin takes over another's tool: the caller keeps the name it always
// used and the implementation underneath changes. A replace has to be
// asked for.
var ErrDuplicate = errors.New("kernel: name already registered for kind")

// ErrUnknownKind is returned for a kind the kernel does not define.
var ErrUnknownKind = errors.New("kernel: unknown extension point")

// ErrEmptyName is returned when a registration carries no name.
var ErrEmptyName = errors.New("kernel: registration has no name")

// Entry is one registered capability.
//
// Plugin records which package registered it. That field is the reason the
// registry can be rolled back by name, and the reason an operator reading a
// listing can tell a first-party entry from a third-party one.
type Entry struct {
	Kind   Kind
	Name   string
	Plugin string
	Impl   any
}

// Registry is the microkernel's extension table.
//
// It is safe for concurrent use. Reads take a read lock and never block
// each other; a registration takes the write lock and either commits whole
// or leaves the table untouched.
type Registry struct {
	mu       sync.RWMutex
	entries  map[entryKey]Entry
	byPlugin map[string][]entryKey
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		entries:  make(map[entryKey]Entry),
		byPlugin: make(map[string][]entryKey),
	}
}

// Register adds one capability.
//
// It refuses an empty name, an unknown kind, and a name already taken for
// that kind. It does not check authority: authority is the host's, and the
// host is the only thing that should be calling Register — a plugin reaches
// the registry through the host, not around it.
func (r *Registry) Register(e Entry) error {
	if e.Name == "" {
		return fmt.Errorf("%w: kind=%s plugin=%q", ErrEmptyName, e.Kind, e.Plugin)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownKind, e.Kind)
	}
	key := entryKey{kind: e.Kind, name: e.Name}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.entries[key]; ok {
		return fmt.Errorf("%w: %s (held by plugin %q, %q tried to take it)",
			ErrDuplicate, key, existing.Plugin, e.Plugin)
	}
	r.entries[key] = e
	r.byPlugin[e.Plugin] = append(r.byPlugin[e.Plugin], key)
	return nil
}

// Lookup returns the entry registered for a kind and name.
func (r *Registry) Lookup(kind Kind, name string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[entryKey{kind: kind, name: name}]
	return e, ok
}

// All returns every entry of a kind, ordered by name.
//
// The order is stable so that a listing is a diff between two runs rather
// than a reshuffle: an operator comparing two boot logs wants to see what
// changed, not where it moved.
func (r *Registry) All(kind Kind) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Entry
	for key, e := range r.entries {
		if key.kind == kind {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ByPlugin returns every entry a plugin registered, ordered by kind then
// name. It is what a deactivation walks to roll a package back.
func (r *Registry) ByPlugin(plugin string) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := r.byPlugin[plugin]
	out := make([]Entry, 0, len(keys))
	for _, key := range keys {
		out = append(out, r.entries[key])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RemovePlugin removes every entry a plugin registered and reports how many
// were removed.
//
// Removing a plugin that registered nothing is not an error: a rollback that
// is already done should succeed, for the same reason ports.PluginInstaller
// treats removing an absent package as success.
func (r *Registry) RemovePlugin(plugin string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := r.byPlugin[plugin]
	for _, key := range keys {
		delete(r.entries, key)
	}
	delete(r.byPlugin, plugin)
	return len(keys)
}

// Len reports how many entries the registry holds.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
