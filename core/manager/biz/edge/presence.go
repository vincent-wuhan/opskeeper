package edge

import (
	"context"
	"errors"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// The port is satisfied or the tree does not build. That is the whole guard:
// `alert` and `systemhealth` hold a domain.EdgeQuery, and the only thing that
// keeps this method from quietly disappearing is that something in another
// package is asking for it by name.
var _ domain.EdgeQuery = (*Usecase)(nil)

// ListPresence implements domain.EdgeQuery, the port the alert pipeline's
// staleness gauge and the system-health edge probe hold.
//
// It is the only thing this package exposes to those two domains, and it is a
// projection rather than a pass-through on purpose: both callers read a
// presence question ("is this node registered, when was it last seen, is it
// online") and neither reads a node's identity or its deletion state. Handing
// them `*model.Edge` and letting them pick fields is how a credential column
// ends up reachable from a gauge that has no use for it.
//
// The limit arrives as a plain int because that is all either caller has ever
// passed. A filter struct with a single legal value is a type whose second
// value will be wrong, and this port has no need for one yet.
func (u *Usecase) ListPresence(ctx context.Context, limit int) ([]domain.EdgePresence, error) {
	edges, err := u.List(ctx, ListFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]domain.EdgePresence, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			// A slice of pointers from a query that soft-deletes can
			// carry a nil if a row is scanned into a nil pointer; the
			// two callers both range over the result and would
			// dereference it. Dropping it here keeps that impossible
			// rather than leaving it to every future caller.
			continue
		}
		out = append(out, presenceOf(e))
	}
	return out, nil
}

// var _ domain.EdgeStatusQuery = (*Usecase)(nil) is the guard for the second
// port, and it is the same guard as the one above for the same reason: a
// method that nothing in another package asks for by name is a method that
// can be deleted by a later reader who sees no caller in this file.
var _ domain.EdgeStatusQuery = (*Usecase)(nil)

// PresenceStatus implements domain.EdgeStatusQuery, the port the webshell
// holds to decide whether it may open a shell onto a node.
//
// It reads one column of the row and hands back one string, because that is
// the whole question: the handler compares it against domain.EdgeStatusOnline
// and puts it in an error message when it does not match. Everything else the
// row carries — the name, the device link, the last-seen stamp, and the six
// credential and bookkeeping columns — stays inside this domain.
//
// The error path is the row being absent. `u.Get` already maps a missing row
// to errs.ErrNotFound, so there is no "gone but not an error" state to
// represent, which is why this returns a string and not a pointer: the caller
// used to hold `*model.Edge` and carry a nil check for a `(nil, nil)` that no
// implementation in this tree can return.
func (u *Usecase) PresenceStatus(ctx context.Context, id uint64) (string, error) {
	edge, err := u.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if edge == nil {
		// Unreachable through u.Get, which is the point: it is here so
		// that a future repo implementation returning (nil, nil) fails
		// here rather than handing a caller a bare string that reads as
		// a presence state nobody wrote.
		return "", errs.ErrNotFound
	}
	return edge.Status, nil
}

// var _ domain.EdgeCatalog = (*Usecase)(nil) is the guard for the third port,
// and the same one: the RCA tools hold it by name from another package, so a
// reader who finds no caller in this file cannot conclude the methods are dead.
var _ domain.EdgeCatalog = (*Usecase)(nil)

// ListCatalog implements domain.EdgeCatalog.
//
// The filter is applied here rather than in each caller because two of the six
// call sites used to fetch every row and drop it themselves: the query tool
// filtered on a last-seen window, and both the window and the name match were
// post-filters over a full table load. Moving them in means the edge domain
// decides what "last seen within an hour" means — which is a question about
// the column, not about the tool that happens to ask.
//
// NameContains is matched here rather than pushed into ListFilter.Name, which
// is an exact match. That is a behavioural difference and it is the intended
// one: the tools asked for "contains", and expressing it as an exact match
// would have silently answered a different question.
func (u *Usecase) ListCatalog(ctx context.Context, f domain.EdgeFilter) ([]domain.EdgePresence, error) {
	edges, err := u.List(ctx, ListFilter{Status: f.Status, Limit: f.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]domain.EdgePresence, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		if f.NameContains != "" && !strings.Contains(e.Name, f.NameContains) {
			continue
		}
		if f.SeenAfter != nil {
			// A node that has never reported in has no LastSeenAt, and both
			// callers used to drop it: their rule was `LastSeenAt == nil ||
			// LastSeenAt.Before(cutoff)` skips, so "never seen" counted as
			// "not seen recently" rather than as unknown. That is the
			// reading an operator means by `last_seen_within_minutes` — a
			// node that has never phoned home is not a node that was around
			// an hour ago. Keeping nil here would answer a different
			// question under the same parameter name, and the difference is
			// invisible until an RCA concludes that a freshly registered
			// node was active during the incident.
			if e.LastSeenAt == nil || e.LastSeenAt.Before(*f.SeenAfter) {
				continue
			}
		}
		out = append(out, presenceOf(e))
	}
	return out, nil
}

// Presence implements domain.EdgeCatalog.
//
// The missing-node case is answered rather than returned: u.Get reports an
// absent row as errs.ErrNotFound, and a caller asking "is node 7 registered"
// does not want a failure, it wants an answer. Translating here means the six
// RCA tools stop each carrying their own translation of that error, which is
// six chances to spell it slightly differently.
func (u *Usecase) Presence(ctx context.Context, id uint64) (domain.EdgePresence, bool, error) {
	e, err := u.Get(ctx, id)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.EdgePresence{}, false, nil
		}
		return domain.EdgePresence{}, false, err
	}
	if e == nil {
		// Same reasoning as PresenceStatus above: unreachable through
		// u.Get today, and here so that a future repo returning (nil, nil)
		// fails here rather than handing back a zero node that reads as a
		// real one.
		return domain.EdgePresence{}, false, nil
	}
	return presenceOf(e), true, nil
}

// PluginHealth is the third method of domain.EdgeCatalog and it is not
// redeclared here: plugin_health.go already has one, and it already returns a
// core/domain type because decision 281 moved PluginHealth out of this
// package for the tunnel handler. So this method needed no work at all —
// which is the best possible outcome for a port method, and worth noting
// because it means the compile-time guard at the top of this file
// (var _ domain.EdgeCatalog = (*Usecase)(nil)) is doing real work: it is what
// proves the third method is satisfied from the other file rather than from
// one written to satisfy the interface.

// PresenceByName implements domain.EdgeCatalog. It resolves the same question
// as Presence with the other key an operator has, and it maps an absent row
// the same way — found=false with a nil error — so the three tools that use
// it stop each carrying their own translation of errs.ErrNotFound.
func (u *Usecase) PresenceByName(ctx context.Context, name string) (domain.EdgePresence, bool, error) {
	e, err := u.GetByName(ctx, name)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.EdgePresence{}, false, nil
		}
		return domain.EdgePresence{}, false, err
	}
	if e == nil {
		return domain.EdgePresence{}, false, nil
	}
	return presenceOf(e), true, nil
}

// presenceOf is the one place a stored row becomes a projection. Three
// methods above funnel through it, and they did not used to share anything —
// each spelled out its own field copy — so a column added to EdgePresence had
// to be added in three places and a compiler could not help.
func presenceOf(e *model.Edge) domain.EdgePresence {
	return domain.EdgePresence{
		ID:         e.ID,
		Name:       e.Name,
		Status:     e.Status,
		DeviceID:   e.DeviceID,
		LastSeenAt: e.LastSeenAt,
		CreatedAt:  e.CreatedAt,
	}
}
