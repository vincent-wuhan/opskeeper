package domain

import "context"

// This file is the second of two ports that were already written and wired in
// the wrong place. `server/integration` declared a local `GrafanaService`
// interface with exactly these three methods and satisfied it structurally
// with `*biz/grafana.Service` — the seam existed, and it still compiled the
// consumer against the producer's package because the one return type in the
// signature was named there.
//
// The return type is all this edge was. Three methods, one three-field struct,
// no entity, no ORM tag, no behaviour. That is the whole of it, and it is why
// the seam was worth closing rather than documenting.

// GrafanaSyncResult is what one bootstrap run did: the folder it created, the
// datasource it pointed at the platform's Prometheus, and the dashboard titles
// it pushed.
//
// It carries json tags because it *is* the wire shape — `POST
// /v1/integrations/grafana/sync` writes this struct straight into the response
// body, and the frontend reads those three keys. Moving the type does not move
// the contract, which is why the tags travelled with it rather than being
// re-declared on a DTO that could then drift from the type the service
// returns.
//
// The name carries its owner. It was `SyncResult`, which says nothing about
// what synchronised, and decision 233's whole subject is names like that: three
// packages can each have a `SyncResult` and every one of them reads as the same
// vocabulary until a reader merges them. This one is declared exactly once in
// the tree today, and it is named so that the day a second one appears the two
// are visibly different words.
type GrafanaSyncResult struct {
	Folder     string   `json:"folder"`
	Datasource string   `json:"datasource"`
	Dashboards []string `json:"dashboards"` // titles synced
}

// GrafanaQuery is the port the integration HTTP surface holds.
//
// Three methods, and the split between them is the surface's own: `Test` is a
// connectivity probe the operator triggers by hand, `Sync` is the bootstrap an
// operator triggers by hand, and `FetchDashboardJSON` is the one the sync flow
// calls on itself. Nothing else in the tree needs any of the three, and the
// consumer already said so by writing this interface itself.
type GrafanaQuery interface {
	// Test verifies that the configured Grafana is reachable with the
	// configured service-account token.
	Test(ctx context.Context) error
	// Sync runs the full bootstrap: folder, datasource, dashboards.
	Sync(ctx context.Context) (*GrafanaSyncResult, error)
	// FetchDashboardJSON returns one provisioned dashboard's JSON by uid.
	FetchDashboardJSON(ctx context.Context, uid string) ([]byte, error)
}
