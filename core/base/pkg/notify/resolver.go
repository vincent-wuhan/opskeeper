package notify

import (
	"log/slog"
	"sort"
	"sync"
)

// ChannelResolver is the startup-time view of the notification channels a
// host admitted.
//
// The interface is declared here, in the consumer, and implemented by the
// manager-side glue that reads the kernel's notifier extension point. This
// package imports neither the kernel nor the governance vocabulary, and the
// implementation knows nothing about this package beyond the interface —
// which is the whole point of declaring it on this side.
//
// The view is installed once, at startup, and read on every delivery. It is
// a snapshot: a package deactivated at run time does not change what an
// installed view answers, because a restart is how a deployment changes the
// set of packages it hosts.
type ChannelResolver interface {
	// Types returns the channel types the view holds, sorted.
	Types() []string
	// Factory returns the factory the view holds for a channel type, and
	// whether it holds that type at all.
	Factory(typ string) (ChannelFactory, bool)
	// Logger is where the view wants a fallback warning to go.
	//
	// It is not BuildChannel's own logger: the production caller
	// (BuildSenderFromChannel) passes nil there, and a warning that only
	// appears when some other caller happens to pass a logger is a warning
	// that does not appear at all.
	Logger() *slog.Logger
}

// The installed view, and what installing it displaced.
//
// Everything here lives under channelMu, the registry's own lock: the
// registry map is read by BuildChannel under it, and installing a view
// rewrites that map.
var (
	installedView   ChannelResolver
	displacedByView = map[string]ChannelFactory{}
	addedByView     = map[string]bool{}
)

// InstallChannelResolver installs the startup-time channel view, or removes it
// when r is nil.
//
// Installing rewrites the registry map: a type the view holds resolves to the
// view's factory, and a type it does not hold keeps the registry's own
// factory behind a wrapper that says so — once per type. A type the view
// holds and the registry never had is added, because a channel type a
// deployment admits is a channel type the deployment must be able to build.
//
// Passing nil puts the registry back exactly as it was. That is the rollback
// lever the migration needs: the assembly layer can stop calling into the
// kernel without leaving a half-installed view behind.
//
// The registry map is where the view is applied rather than BuildChannel
// itself: the registry is the one thing the old path and the new path already
// agree on, and the migration keeps registry.go byte-identical — a change
// there would move the read order out of the file every existing test in
// this package is written against.
func InstallChannelResolver(r ChannelResolver) {
	channelMu.Lock()
	defer channelMu.Unlock()

	restoreRegistryLocked()
	if r == nil {
		return
	}
	installedView = r

	log := r.Logger()
	types := make([]string, 0, len(channelFactories))
	for typ := range channelFactories {
		types = append(types, typ)
	}
	sort.Strings(types)
	for _, typ := range types {
		legacy := channelFactories[typ]
		displacedByView[typ] = legacy
		if f, ok := r.Factory(typ); ok {
			channelFactories[typ] = f
			continue
		}
		channelFactories[typ] = fallbackFactory(typ, legacy, log)
	}

	for _, typ := range r.Types() {
		if _, ok := channelFactories[typ]; ok {
			continue
		}
		f, ok := r.Factory(typ)
		if !ok {
			continue
		}
		addedByView[typ] = true
		channelFactories[typ] = f
	}
}

// restoreRegistryLocked puts the registry map back the way it was before the
// installed view rewrote it.
//
// It is called by every install, so installing a second view does not stack
// on top of the first one.
func restoreRegistryLocked() {
	if installedView == nil {
		return
	}
	for typ, legacy := range displacedByView {
		channelFactories[typ] = legacy
	}
	for typ := range addedByView {
		delete(channelFactories, typ)
	}
	displacedByView = map[string]ChannelFactory{}
	addedByView = map[string]bool{}
	installedView = nil
}

// fallbackFactory wraps a registry factory for a channel type the installed
// view does not hold.
//
// It answers from the registry and says so — once per channel type, because
// a deployment that logs the same fallback on every delivery produces a log
// nobody reads. The warning goes to the view's logger rather than to the
// logger BuildChannel was handed, which may be nil.
func fallbackFactory(typ string, legacy ChannelFactory, log *slog.Logger) ChannelFactory {
	var once sync.Once
	return func(spec ChannelSpec, senderLog *slog.Logger) (Sender, error) {
		once.Do(func() {
			if log == nil {
				return
			}
			log.Warn("notification channel resolved from the legacy registry",
				slog.String("type", typ),
				slog.String("hint", "no admitted plugin holds this channel type; the legacy channel registry is removed in the next notifier migration slice"))
		})
		return legacy(spec, senderLog)
	}
}
