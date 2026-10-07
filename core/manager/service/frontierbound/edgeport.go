package frontierbound

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// This file is the answer to a measurement, and it is the third of the shape
// decision 218 established: service/frontierbound is the tunnel's manager-side
// handler, it calls seven methods on the edge domain, and every signature it
// needed already said "built-in types only" except for three domain rows.
//
//   - EdgeAuthn and EdgeUC were named as *concrete* struct pointers in Wiring.
//     That is the worst of both: not a port, and not a value the handler owns.
//   - the three row types (plugin health, plugin config, change events) all
//     came from the same package, so a handler that wanted to record a health
//     row had to import the domain it was recording into.
//
// So: three interfaces, four moved types, and the handler now names the edge
// domain nowhere. The edges are still real — *edgebiz.Usecase satisfies
// EdgeLifecycle structurally, and the assembly root still passes it — but
// which methods exist, and which types cross, are stated here rather than
// being whatever the concrete package happens to export today.

// EdgeAuthenticator turns a node's tunnel credential into a session.
// *edgebiz.AccessKeyAuthenticator satisfies it.
type EdgeAuthenticator interface {
	Authenticate(ctx context.Context, accessKey, secretKey string) (tunnel.Session, error)
}

// EdgeLifecycle is the edge domain's tunnel-facing half: one call per lifecycle
// event the gateway observes, plus the piggybacked health snapshot a heartbeat
// carries. It is five methods, and the handler makes one call per event —
// there is no loop, no optional argument, and no caller that uses a subset.
//
// The parameter types are tunnel's and the standard library's on purpose.
// tunnel.HostInfo is the same struct the wire message carries, so declaring a
// local copy would have been a second declaration of the same JSON, and that
// is the mistake core/domain exists to stop.
type EdgeLifecycle interface {
	HandleOffline(ctx context.Context, edgeID uint64, at time.Time) error
	HandleRegister(ctx context.Context, edgeID uint64, info tunnel.HostInfo, agentVersion string) error
	HandleHeartbeat(ctx context.Context, edgeID uint64, ts time.Time, pigVersion string) error
	// RecordPluginHealth is not an error return on purpose: the heartbeat
	// handler calls it best-effort and never fails a heartbeat on a health
	// snapshot it could not store.
	RecordPluginHealth(edgeID uint64, items []domain.PluginHealth)
}

// ChangeEventIngestor accepts the change events a node pushes, already
// filtered by the handler. *changeevent.Usecase satisfies it.
//
// The method is Ingest, not BatchInsert, and that difference is the cut. The
// handler used to build edgemodel.ChangeEventRow values itself — a GORM model,
// with the labels map JSON-encoded and the absent sequence number written as
// NULL so it would not collide with every other event on the node under the
// (edge_id, seq) unique index. Both of those are facts about the edge domain's
// schema, and the handler was the one enforcing them from three packages away.
type ChangeEventIngestor interface {
	Ingest(ctx context.Context, events []domain.ChangeEventInput) (int, error)
}
