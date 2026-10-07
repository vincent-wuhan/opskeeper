// Package pigmodel adapts OpsKeeper's settings-backed provider
// configuration onto PiG's provider and model types.
//
// The adapter exists to satisfy a mismatch in credential lifetime. OpsKeeper
// stores provider credentials in an admin-editable settings table and expects
// an edit to take effect on the next request, without a restart and without
// a process-wide credential store. PiG's own auth model is built around a
// per-user home directory (~/.pig). Rather than import that model, the
// registry resolves credentials per call through PiG's GetAPIKey hook and
// through StreamOptions.APIKey, so OpsKeeper never writes a credential to
// disk on PiG's behalf.
package pigmodel

import (
	"context"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ProviderConfig is one provider's persisted configuration.
//
// It mirrors the settings keys the control plane writes:
// `<provider>_api_key`, `<provider>_base_url`, `<provider>_models` (a JSON
// array of slugs), and `<provider>_default_model`.
type ProviderConfig struct {
	ID     domain.ProviderID
	APIKey string
	// BaseURL overrides the provider's default endpoint. Required for the
	// custom provider, optional for the rest.
	BaseURL string
	// Models is every slug the provider offers, in display order.
	Models []string
	// DefaultModel is the slug used when a caller does not pin one.
	DefaultModel string
}

// Configured reports whether the provider can serve a request: it has an API
// key (or a base URL for a local endpoint that needs no key) and a default
// model.
func (c ProviderConfig) Configured() bool {
	if c.DefaultModel == "" {
		return false
	}
	return c.APIKey != "" || c.BaseURL != ""
}

// HasModel reports whether the provider offers slug. A provider whose
// operator left the model list empty is treated as offering its default
// only, so a half-configured provider does not silently accept any slug.
func (c ProviderConfig) HasModel(slug string) bool {
	if slug == "" {
		return false
	}
	if slug == c.DefaultModel {
		return true
	}
	for _, m := range c.Models {
		if m == slug {
			return true
		}
	}
	return false
}

// SettingsSource supplies live provider configuration.
//
// The registry calls it on every resolution rather than caching results, so
// an operator's settings edit lands on the next request. Implementations
// are expected to serve from an in-memory cache refreshed by their own
// poller; the registry adds no TTL of its own, because a second TTL here
// would make the effective staleness a product of two intervals that nobody
// can reason about.
type SettingsSource interface {
	// ProviderConfig returns the configuration for id. The bool reports
	// whether the provider is present in settings at all; a provider with
	// no row is not the same as one with an empty row.
	ProviderConfig(ctx context.Context, id domain.ProviderID) (ProviderConfig, bool)
	// DefaultProvider is the cluster-wide fallback. It is consulted only
	// when a caller's selection is incomplete.
	DefaultProvider(ctx context.Context) (domain.ProviderID, bool)
}

// staticSource is an in-memory SettingsSource. It exists for tests and for
// a host that resolves settings once at startup; a host with live settings
// implements the interface directly.
type staticSource struct {
	providers map[domain.ProviderID]ProviderConfig
	fallback  domain.ProviderID
}

// NewStaticSource returns a SettingsSource backed by a fixed map. A nil or
// empty map yields a source that reports every provider unconfigured,
// which is the correct behaviour for a host with no settings loaded.
func NewStaticSource(providers map[domain.ProviderID]ProviderConfig, fallback domain.ProviderID) SettingsSource {
	cp := make(map[domain.ProviderID]ProviderConfig, len(providers))
	for k, v := range providers {
		cp[k] = v
	}
	return &staticSource{providers: cp, fallback: fallback}
}

func (s *staticSource) ProviderConfig(_ context.Context, id domain.ProviderID) (ProviderConfig, bool) {
	cfg, ok := s.providers[id]
	return cfg, ok
}

func (s *staticSource) DefaultProvider(_ context.Context) (domain.ProviderID, bool) {
	if s.fallback == "" {
		return "", false
	}
	return s.fallback, true
}

// normalizeBaseURL trims a trailing slash so endpoint joins do not produce a
// double slash. A blank URL returns blank, letting the provider apply its
// own default.
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// StaticSettings returns a SettingsSource over a fixed provider list.
//
// It exists for the processes that have no settings table to read: the eval
// command, a contract test, a one-shot CLI. The control plane does not use
// it — there, an operator's edit has to land on the next request, and a
// source that can only be rebuilt by hand cannot promise that.
//
// The slice is copied, so a caller that keeps mutating its own array after
// the call cannot change what the registry resolves against. defaultID may
// be empty, in which case DefaultProvider falls back to the first configured
// provider, matching what a catalog with no configured default means.
func StaticSettings(providers []ProviderConfig, defaultID domain.ProviderID) SettingsSource {
	snapshot := make(map[domain.ProviderID]ProviderConfig, len(providers))
	order := make([]domain.ProviderID, 0, len(providers))
	for _, p := range providers {
		if _, dup := snapshot[p.ID]; dup {
			continue
		}
		snapshot[p.ID] = p
		order = append(order, p.ID)
	}
	return &staticSettings{
		byID:  snapshot,
		order: order,
		deflt: defaultID,
	}
}

type staticSettings struct {
	byID  map[domain.ProviderID]ProviderConfig
	order []domain.ProviderID
	deflt domain.ProviderID
}

// DefaultProvider implements SettingsSource.
func (s *staticSettings) DefaultProvider(context.Context) (domain.ProviderID, bool) {
	if s.deflt != "" {
		if p, ok := s.byID[s.deflt]; ok && p.Configured() {
			return s.deflt, true
		}
	}
	for _, id := range s.order {
		if s.byID[id].Configured() {
			return id, true
		}
	}
	return "", false
}

// ProviderConfig implements SettingsSource.
func (s *staticSettings) ProviderConfig(_ context.Context, id domain.ProviderID) (ProviderConfig, bool) {
	p, ok := s.byID[id]
	return p, ok
}

var _ SettingsSource = (*staticSettings)(nil)
