// pigsettings.go publishes the control plane's live provider configuration
// into the embedded PiG runtime.
//
// # The credential question
//
// PiG's credential model is a file: auth.json under the agent directory,
// written by an interactive `pig login`. OpsKeeper has no interactive login.
// Its keys live in a settings table an administrator edits from a web page,
// and a rotation has to take effect on the next request without a restart.
//
// So OpsKeeper does not use auth.json. It reads its own settings and
// registers them into PiG's ModelRegistry, which is where PiG resolves a
// request credential from. A rotation is therefore a re-registration: no
// cache to expire, no restart, and no second copy of a secret on disk.
//
// # The caching question
//
// This type holds no cache of its own. The underlying setting.Service
// already carries a 60s TTL, and a second TTL here would make the
// effective staleness a product of two intervals that nobody can reason
// about when an operator asks why their new key is not working yet.
package llmpig

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// ProviderCatalog is the reader side of the control plane's provider
// configuration: what the SPA model picker already consumes.
//
// It is declared here rather than taking the concrete settings resolver so
// the wiring can be tested without a database, and so a future catalog
// source does not have to reshape this file.
type ProviderCatalog interface {
	ResolveProviders(ctx context.Context) (providers []llm.ProviderConfig, defaultProvider string, err error)
}

// Sync publishes a ProviderCatalog into a pigcoding.Runtime.
type Sync struct {
	runtime *pigcoding.Runtime
	catalog ProviderCatalog
	log     *slog.Logger

	// mu guards default, which is the one piece of published state a
	// request handler reads. The catalogue itself lives in the runtime and
	// is read there under the runtime's own lock.
	mu sync.RWMutex
	// fallback is the provider an unpinned turn resolves to. It is named
	// for what it is rather than default because "default" is the feature
	// the callers already have a word for, and shadowing it inside this
	// type made the Sync and defaultProvider() read as the same thing.
	fallback domain.ProviderID
	// syncedAt is the last successful publication. It exists for the log
	// line, not for gating: a failed sync must leave the previous
	// credentials in place rather than blanking a working deployment.
	syncedAt time.Time
}

// NewSync wires a runtime to a catalog. A nil runtime or catalog yields a
// Sync that reports every provider unconfigured, which is the fail-closed
// behaviour for a control plane that was assembled without model settings:
// the first turn fails with "not configured" rather than dialling a host
// nobody named.
func NewSync(runtime *pigcoding.Runtime, catalog ProviderCatalog, log *slog.Logger) *Sync {
	if log == nil {
		log = slog.Default()
	}
	return &Sync{runtime: runtime, catalog: catalog, log: log}
}

// Publish reads the catalog and registers it.
//
// A read failure is returned and nothing is registered. That is deliberate:
// unregistering on failure would take a working deployment offline because
// a database was briefly unreachable, and the operator's model would go
// dark for a reason that has nothing to do with their configuration.
func (s *Sync) Publish(ctx context.Context) error {
	if s == nil || s.runtime == nil || s.catalog == nil {
		return nil
	}
	providers, def, err := s.catalog.ResolveProviders(ctx)
	if err != nil {
		return fmt.Errorf("llmpig: read provider catalog: %w", err)
	}

	specs := make([]pigcoding.ProviderSpec, 0, len(providers))
	for _, p := range providers {
		if !p.Configured() {
			// An unconfigured row is skipped rather than registered empty.
			// Registering it would leave a provider that resolves but
			// cannot authenticate, so every request for it fails with a
			// 401 instead of the "not configured" that tells the operator
			// what to actually do.
			continue
		}
		specs = append(specs, pigcoding.ProviderSpec{
			ID:           p.ID,
			APIKey:       p.APIKey,
			BaseURL:      p.BaseURL,
			API:          apiFor(p.ID),
			DefaultModel: p.Model,
			Models:       append([]string(nil), p.Models...),
		})
	}
	if err := pigcoding.RegisterProviders(s.runtime, specs); err != nil {
		return fmt.Errorf("llmpig: register providers: %w", err)
	}

	s.mu.Lock()
	s.fallback = defaultProvider(providers, def)
	s.syncedAt = time.Now()
	s.mu.Unlock()
	return nil
}

// Default reports the provider an unpinned turn resolves to.
func (s *Sync) Default() domain.ProviderID {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fallback
}

// Selection builds the model selection for one turn.
//
// provider and model are the caller's pins; either may be empty. An empty
// provider falls back to the published default, and an empty model is left
// for the runtime to resolve against that provider's default — resolving
// it here would mean reading the catalogue a second time on a hot path, and
// the two reads could disagree across a settings edit.
func (s *Sync) Selection(provider, model string, thinking string) domain.ModelSelection {
	if provider == "" {
		provider = string(s.Default())
	}
	return domain.ModelSelection{
		Provider:      domain.ProviderID(provider),
		Model:         model,
		ThinkingLevel: thinking,
	}
}

// Spec renders one catalog entry as a "provider/model" reference, which is
// how a turn pins a model against the runtime's allowlist.
func Spec(provider, model string) string {
	return pigcoding.NormalizeModelSpec(provider + "/" + model)
}

// SettingsSource adapts the same catalog onto pigmodel.SettingsSource, for
// the one-shot completion path.
//
// The two paths deliberately read the same rows. If they read different
// ones, an operator would find that a background judge and an interactive
// turn disagree about which models exist, and there would be no way to tell
// which one is lying.
func SettingsSource(catalog ProviderCatalog) pigmodel.SettingsSource {
	return &catalogSettings{catalog: catalog}
}

type catalogSettings struct {
	catalog ProviderCatalog
}

func (s *catalogSettings) read(ctx context.Context) ([]llm.ProviderConfig, domain.ProviderID, bool) {
	if s.catalog == nil {
		return nil, "", false
	}
	providers, def, err := s.catalog.ResolveProviders(ctx)
	if err != nil {
		// A transient settings read failure must not look like "this
		// provider was removed": that would make the registry report an
		// unknown provider and stop retrying. Reporting "not present" is
		// the fail-closed choice — the caller gets a clear not-configured
		// error instead of a dial to stale coordinates.
		return nil, "", false
	}
	return providers, domain.ProviderID(strings.TrimSpace(def)), true
}

// ProviderConfig implements pigmodel.SettingsSource.
//
// A provider the catalog omits reports ok=false, which the registry treats
// as "not configured" rather than "misconfigured". The distinction matters:
// the first is not retried, the second is.
func (s *catalogSettings) ProviderConfig(ctx context.Context, id domain.ProviderID) (pigmodel.ProviderConfig, bool) {
	providers, _, ok := s.read(ctx)
	if !ok {
		return pigmodel.ProviderConfig{}, false
	}
	for _, p := range providers {
		if domain.ProviderID(p.ID) != id {
			continue
		}
		return pigmodel.ProviderConfig{
			ID:           id,
			APIKey:       p.APIKey,
			BaseURL:      p.BaseURL,
			Models:       append([]string(nil), p.Models...),
			DefaultModel: p.Model,
		}, true
	}
	return pigmodel.ProviderConfig{}, false
}

// DefaultProvider implements pigmodel.SettingsSource.
//
// It returns the catalog's default when that provider is actually
// configured. A default naming an unconfigured provider would make every
// unpinned call fail with "default x is not configured" — an error that
// reads as a bug in OpsKeeper rather than as the missing key it is — so an
// unusable default falls through to the first configured provider, which is
// the same tie-break the SPA picker applies.
func (s *catalogSettings) DefaultProvider(ctx context.Context) (domain.ProviderID, bool) {
	providers, def, ok := s.read(ctx)
	if !ok {
		return "", false
	}
	return defaultProvider(providers, string(def)), true
}

// defaultProvider picks the provider an unpinned turn uses.
func defaultProvider(providers []llm.ProviderConfig, def string) domain.ProviderID {
	for _, p := range providers {
		if p.ID == def && p.Configured() {
			return domain.ProviderID(def)
		}
	}
	for _, p := range providers {
		if p.Configured() {
			return domain.ProviderID(p.ID)
		}
	}
	return ""
}

// apiFor maps an OpsKeeper provider id onto a wire format.
//
// The default is OpenAI-compatible because that is what every provider in
// OpsKeeper's settings table actually speaks, including the ones with
// their own brand names. The one exception is the one provider that is not,
// and it has been the exception since before this function existed. A
// second exception is the signal to add a case here rather than to start
// guessing per request.
func apiFor(id string) pigai.API {
	if id == "anthropic" {
		return pigai.APIAnthropicMessages
	}
	return pigai.APIOpenAICompletions
}

// Compile-time proof the adapter satisfies the contract it bridges to.
var _ pigmodel.SettingsSource = (*catalogSettings)(nil)
