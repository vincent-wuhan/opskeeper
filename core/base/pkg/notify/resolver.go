package notify

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
//
// Since the legacy channel map was removed, this view is the one source of
// channel factories in the process. Installing it is not one of two ways to
// make a channel type buildable — it is the only one, and a process that
// never installs it has no channels at all.
type ChannelResolver interface {
	// Factory returns the factory the view holds for a channel type, and
	// whether it holds that type at all.
	Factory(typ string) (ChannelFactory, bool)
}

// installedView is the view the assembly root installed, or nil.
//
// Installing is one pointer write, made once at startup before any delivery
// path runs; reading is one interface call on every build. There is nothing
// to lock because there is nothing to rewrite: the registry map this once
// shadowed is gone, and with it the install-time reshuffle and the
// once-per-type fallback warning that existed only to describe it.
var installedView ChannelResolver

// InstallChannelResolver installs the startup-time channel view, or removes
// it when r is nil.
//
// The view answers every BuildChannel call from the moment it is installed.
// Passing nil leaves the process with no resolvable channel types — which
// is the state of a process that never wired the notifier extension point,
// not a restored "pre-migration" registry: that registry no longer exists,
// and a rollback of this behaviour is a git revert of the slice that
// removed it.
//
// The nil check is explicit rather than left to a method call on a nil
// interface: Set implements this interface as a value type, so a typed nil
// cannot occur today, but one line here retires the whole class of bug a
// future implementation might reintroduce.
func InstallChannelResolver(r ChannelResolver) {
	if r == nil {
		installedView = nil
		return
	}
	installedView = r
}

// factoryFor reads the installed view.
//
// The name exists so BuildChannel reads as policy — "where factories come
// from" — rather than as plumbing.
func factoryFor(typ string) (ChannelFactory, bool) {
	if installedView == nil {
		return nil, false
	}
	return installedView.Factory(typ)
}
