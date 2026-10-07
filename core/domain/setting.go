package domain

import "context"

// This file is the port and the vocabulary for reading operator-supplied
// configuration out of the settings table, and it exists because the Grafana
// service was reaching into the setting domain's packages to do it.
//
// The shape of that dependency was not a port at all: `biz/grafana` held a
// `*setting.Service` by name and called two methods on it. Declaring an
// interface would have been theatre — the field type would still have been
// the producer's struct. What makes this a real boundary is that the port's
// signature names nothing but builtins, so a test can satisfy it in five
// lines and the Grafana service can be exercised without a settings table,
// a database, or the setting domain in the build graph.
//
// The vocabulary moved with it, and that is the part worth arguing about. The
// categories and keys are column values in one table, so the setting domain
// owns them; but a constant that only the setting domain reads can stay there,
// while one that a second domain has to name to ask its own question cannot.
// The line drawn here is that test: prom and grafana crossed, so they moved;
// llm, loki, tempo, websearch and agent did not, and are still declared once
// each in `model/setting`. The setting model keeps aliases to everything that
// moved, so its own code and its other callers read the same words as before
// and there is still exactly one declaration of each string.
//
// The names carry their owner — `SettingCategoryGrafana`, not `CategoryGrafana`
// — for the reason decision 233 gave: a bare `CategoryProm` is a word three
// packages could each declare, and the day one does, a reader merging the two
// has no way to tell.

// SettingStore is the port a domain holds to read and write operator-supplied
// configuration.
//
// Two methods, because two is what the only cross-domain consumer calls, and
// `SetIfAbsent` is absent on purpose: it is the bootstrap path's compare-and-set
// and the Grafana service never asks for it. A port that carried it would be a
// port another caller could reach through to make a decision this one has no
// reason to make.
//
// The parameters are plain strings rather than a typed key, and that is the
// second decision here. A `SettingKey` struct would put the setting domain's
// type back into every caller's signature, which is the dependency this port
// exists to remove. The price is that a typo is a runtime miss rather than a
// compile error — which is the trade the constants below exist to blunt, and
// the reason `Get` returns `found` rather than leaving a caller to guess
// between an empty value and an absent one.
type SettingStore interface {
	// Get returns the stored value, whether the key was present, and an
	// error. The three are separate because an empty string is a legal
	// value and an absent key is a legal answer.
	Get(ctx context.Context, category, key string) (value string, found bool, err error)
	// Set writes a value, marking it sensitive when the third call is true so
	// the settings surface redacts it on the way out.
	Set(ctx context.Context, category, key, value string, sensitive bool) error
}

// Categories and keys that more than one domain has to name.
//
// Only the two feature areas that a second domain reads are here. A category
// or key that stays inside the setting domain stays in `model/setting`, and
// moving one of those would be a change with no boundary behind it.
const (
	SettingCategoryProm    = "prom"    // external Prometheus / VictoriaMetrics / Mimir / Thanos
	SettingCategoryGrafana = "grafana" // external Grafana root URL + service-account token
)

// Well-known keys under SettingCategoryProm. `core/base/pkg/promauth` reads
// bearer/basic on every request through its Resolver; URLs are read at
// startup (env seed to DB) and a change requires a manager restart.
const (
	SettingKeyPromQueryURL       = "query_url"
	SettingKeyPromRemoteWriteURL = "remote_write_url"
	SettingKeyPromBearerToken    = "bearer_token" // sensitive
	SettingKeyPromBasicUser      = "basic_user"
	SettingKeyPromBasicPassword  = "basic_password" // sensitive
	SettingKeyPromTLSInsecure    = "tls_insecure"   // "true" / "false"
	SettingKeyPromTLSCAPEM       = "tls_ca_pem"     // PEM text
)

// Well-known keys under SettingCategoryGrafana.
//
// SAToken is the bearer credential minted at bootstrap for the embedded
// Grafana. APIKey is the operator-pasted equivalent for an external Grafana —
// semantically identical, since both feed the Authorization: Bearer header,
// and exposed as a separate field because an operator running their own Grafana
// usually cannot mint a fresh service-account token.
//
// OrgID mirrors the per-browser observability store value into the backend so
// the dashboard-fetch proxy can default it without the SPA passing it on every
// call.
const (
	SettingKeyGrafanaRootURL = "root_url"
	SettingKeyGrafanaSAToken = "sa_token" // sensitive — service-account token
	SettingKeyGrafanaAPIKey  = "api_key"  // sensitive — alternative bearer
	SettingKeyGrafanaOrgID   = "org_id"
)
