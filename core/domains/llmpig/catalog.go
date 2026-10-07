// catalog.go is the console's read/write surface over the same settings
// rows the agent runtime resolves models from.
//
// # Why this is a separate type from Sync
//
// Sync already has a Default(), and it returns a domain.ProviderID because
// that is what a turn's model selection is built from. The console's model
// picker wants Default() (string, string) — a provider and a model slug, the
// two values it puts in a <select>. One Go type cannot carry both signatures
// under one name, and the two answers are not even the same question: Sync
// answers "which provider does an unpinned turn use", the picker answers
// "what do I show the operator as preselected". So the picker gets its own
// view over the same catalog rather than a renamed copy of Sync's method.
//
// # Why the picker reads live, not the published snapshot
//
// Sync keeps a fallback provider from its last successful Publish. This type
// does not, and reads the catalog on every call instead. The difference
// matters in exactly one direction: after an admin deletes the default
// provider, the picker must stop offering it immediately, while a turn that
// is already in flight keeps resolving against the provider it started with.
// A picker reading a snapshot would keep advertising a provider the next
// request cannot use.
package llmpig

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
)

// catalogPublishTimeout bounds the re-publish an admin save triggers.
//
// The request that triggers it is an interactive settings save, so waiting
// a few seconds for the new credentials to land is the right trade. It is
// bounded anyway: a settings read that hangs must not hold an admin's
// browser open, and the underlying setting.Service carries its own TTL, so
// a publish that gives up here self-heals within a minute without it.
const catalogPublishTimeout = 5 * time.Second

// CatalogView adapts a ProviderCatalog to the two interfaces the HTTP layer
// declares: the model picker (Providers, Default) and the settings-save
// invalidation hook (Invalidate).
//
// It is a view, not a cache. Every call reads the catalog.
type CatalogView struct {
	sync    *Sync
	catalog ProviderCatalog
}

// NewCatalogView wires a view. A nil Sync is allowed so the picker still
// works in an assembly that published nothing; Invalidate then reports that
// there was nothing to refresh instead of panicking on a nil receiver.
func NewCatalogView(s *Sync, catalog ProviderCatalog) *CatalogView {
	return &CatalogView{sync: s, catalog: catalog}
}

// Providers lists the configured providers for the model picker, ordered by
// id so the list does not reshuffle between two requests that read the same
// rows.
//
// An unconfigured provider is omitted rather than listed greyed out. The
// picker has no disabled state, and a provider the deployment cannot
// authenticate against is one whose every selection ends in a 401 — worse
// than not offering it.
func (v *CatalogView) Providers() []llm.ProviderInfo {
	if v == nil || v.catalog == nil {
		return nil
	}
	providers, _, err := v.catalog.ResolveProviders(context.Background())
	if err != nil || len(providers) == 0 {
		// An unreadable catalog is reported as an empty one. The picker's
		// contract already covers this: no providers means the selector
		// is hidden, which is the truth from the browser's side.
		return nil
	}
	out := make([]llm.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		if !p.Configured() {
			continue
		}
		out = append(out, p.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Default reports the provider and model the picker preselects.
//
// It resolves through the same defaultProvider tie-break an unpinned turn
// uses, so the model an operator sees preselected is the model they get when
// they do not choose. A picker that disagreed with the router was the single
// most confusing report this surface ever produced.
func (v *CatalogView) Default() (string, string) {
	if v == nil || v.catalog == nil {
		return "", ""
	}
	providers, def, err := v.catalog.ResolveProviders(context.Background())
	if err != nil {
		return "", ""
	}
	id := defaultProvider(providers, def)
	if id == "" {
		return "", ""
	}
	for _, p := range providers {
		if string(id) == p.ID {
			return p.ID, p.Model
		}
	}
	return string(id), ""
}

// Invalidate re-publishes the catalog after an admin edits a provider row.
//
// The one-shot completion path needs nothing here: pigmodel.Registry reads
// the catalog on every request and only caches the transport, so the next
// completion already sees the new key. The agent path is the one that has
// to be told, because it resolves against a ModelRegistry that was
// populated once at boot — a provider added to the settings table would
// otherwise stay invisible to every session until a restart.
//
// A publish failure is logged, not returned. The caller is an HTTP handler
// whose contract returns nothing, and turning a failed re-publish into an
// error would report the *save* as failed when the save itself succeeded and
// the deployment will pick the change up on its own within the settings TTL.
func (v *CatalogView) Invalidate() {
	if v == nil || v.sync == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogPublishTimeout)
	defer cancel()
	if err := v.sync.Publish(ctx); err != nil {
		v.sync.log.Warn("llm: re-publish provider catalog after settings edit failed;"+
			" the change will be picked up when the settings cache expires",
			slog.String("error", err.Error()))
	}
}

// Compile-time proof of the two contracts the HTTP layer declares. Both are
// structural, so this is the only place the mismatch would be caught.
var (
	_ interface {
		Providers() []llm.ProviderInfo
		Default() (string, string)
	} = (*CatalogView)(nil)
	_ interface{ Invalidate() } = (*CatalogView)(nil)
)
