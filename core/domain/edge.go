package domain

import (
	"context"
	"time"
)

// This file is the answer to a measurement, not a design sketch.
//
// The edge domain's Usecase exposes twenty methods. Four bounded contexts call
// into it, and between them they call nine. Two of those four — the alert
// pipeline's staleness gauge and the system-health edge probe — call exactly
// one method, `List`, and both pass the same argument: a limit of 1000 and
// nothing else. They never touch a filter field.
//
// So the seam between them and the edge domain is one method returning six
// columns, and both of them already had a local interface saying so. What they
// could not do was name the types, because the signature said `edgebiz.Edge`
// and `edgemodel.Edge` — a package-shaped boundary, not an interface one
// (decision 218). Moving the six columns here is what turns it into an
// interface boundary, and it is why this file carries no GORM tag and no
// entity: a value the two callers read is not an entity, and importing one
// would put a database row in the contract layer.

// Edge presence states. These are the values the edges table's status column
// is constrained to, repeated here as plain constants so a consumer that only
// counts them does not have to import the model package to name them.
//
// They are declared as untyped string constants rather than a named type on
// purpose: `EdgePresence.Status` stays a `string`, so the edge domain's
// existing `Edge.Status` assigns to it with no conversion and no second
// vocabulary to keep in step. A named type here would be a third spelling of
// the same two values, and this repository has already been bitten by that
// shape twice (decisions 229, 232).
const (
	EdgeStatusOnline  = "online"
	EdgeStatusOffline = "offline"
)

// EdgePresence is the part of a registered node that a consumer outside the
// edge domain is allowed to see.
//
// It is six fields out of fifteen, and the nine it leaves behind are not
// arbitrary: what is left is credentials (`AccessKeyID`, `SecretKeyHash`),
// the soft-delete mechanism (`DeleteMarker`, `DeletedAt`), version self-report
// (`AgentVersion`, `PigVersion`) and bookkeeping (`Description`, `UpdatedAt`,
// `CreatedBy`). A consumer that could reach any of those would be able to
// assert things about a node's identity or its deletion, and neither of the
// two callers here needs either.
//
// `LastSeenAt` is a pointer because the column is nullable and a node that
// has never been seen has no value to report — the gauge falls back to
// `CreatedAt` in that case, and that fallback is a fact about the data, not
// something this type should smooth over.
type EdgePresence struct {
	ID         uint64
	Name       string
	Status     string
	DeviceID   *uint64
	LastSeenAt *time.Time
	CreatedAt  time.Time
}

// EdgeQuery is the port the alert pipeline and the system-health probe hold.
//
// One method, deliberately. A wider port would be a port every future caller
// could reach through, and the reason this edge was worth cutting is that
// neither caller touches anything else. If a second consumer later needs the
// node's plugin health, it gets a second method here with its own
// justification — not a widening of this one by accretion.
type EdgeQuery interface {
	// ListPresence returns up to limit registered nodes, most useful first
	// as the edge domain orders them. The limit is passed as a plain int
	// rather than a filter struct because both callers pass the same
	// constant: a filter type here would be a shape with one legal value,
	// which is a type that exists to be wrong later.
	ListPresence(ctx context.Context, limit int) ([]EdgePresence, error)
}

// EdgeStatusQuery is the port the webshell holds, and it is a second
// interface rather than a second method on EdgeQuery on purpose.
//
// The file above says a wider port is a port every future caller can reach
// through, and it offers the remedy as "a second method here with its own
// justification". Read strictly, that remedy is a second *port*: alert and
// systemhealth hold EdgeQuery to count stale nodes, and handing them a point
// lookup they never asked for is the same accretion one method later. The
// three consumers ask three different questions, and a question that has one
// holder has no business sharing a type with a question that has two.
//
// The answer is one string, not an EdgePresence. This consumer reads exactly
// one column of the fifteen the model carries — the presence state — and the
// projection principle the rest of this file follows is "move the columns the
// caller reads", not "move a row that contains them". A record whose only use
// is to be compared against EdgeStatusOnline is a struct with one legal
// comparison, and the two untyped constants above already exist so that
// naming a state costs no import.
//
// The missing row is an error rather than a zero value, and that is what
// removes a branch from the caller: a `*model.Edge` port can answer
// `(nil, nil)`, so its holder has to carry a "the row is gone" check that no
// implementation in this tree can produce. A string cannot, so the question
// stops being askable.
type EdgeStatusQuery interface {
	// PresenceStatus returns the node's presence state — one of
	// EdgeStatusOnline or EdgeStatusOffline — or an error if the node is
	// not registered. It does not fall back to any other value, because
	// "unknown" is a claim about a node and this port is only allowed to
	// make claims the edges table already makes.
	PresenceStatus(ctx context.Context, id uint64) (string, error)
}

// EdgeFilter is what the RCA tools are allowed to ask for when they list the
// node estate.
//
// It is a projection of the edge domain's own five-field ListFilter, and the
// three columns it drops are the three nothing on this side sets: CreatedBy
// (every RCA tool lists every node, not one operator's), and Offset (every
// caller here takes the whole result and cuts it itself, because each of them
// has a different notion of "enough" — 500 for the tools that join against
// metrics, 5000 for the one that draws a topology, and a caller's own limit
// for the query tool).
//
// SeenAfter is the one field that did not exist in ListFilter. Two of the
// callers used to fetch every row and then drop the ones whose LastSeenAt was
// older than a cutoff, which means the edge domain was loading the whole table
// to answer a question about a window. It is a pointer because "no window" and
// "the window that includes everything" are different requests, and only the
// caller knows which one it meant.
type EdgeFilter struct {
	// Status is EdgeStatusOnline or EdgeStatusOffline, or empty for both.
	Status string
	// NameContains is a substring match on the node's display name.
	NameContains string
	// SeenAfter keeps only nodes last seen at or after this instant. A
	// node that has never reported in has no stamp and is dropped, which
	// is the rule the post-filter had before it became a filter.
	SeenAfter *time.Time
	// Limit caps the result. Zero means the edge domain's own default.
	Limit int
}

// EdgeCatalog is the port the RCA tool set holds, and it is a second port
// rather than a wider EdgeQuery for the reason the file above gives: alert and
// systemhealth count stale nodes, and this consumer enumerates the estate to
// join it against metrics, logs and alerts. Handing the first pair a
// point lookup, or this consumer a method that only counts, would be the same
// accretion one method later.
//
// It is one port with four methods rather than four one-method ports
// because it has one holder. The file above's own precedent runs both ways —
// EdgeStatusQuery is a single method for a single holder, and that is right
// because nobody else would ever want it — but a consumer that asks three
// questions about the same subject should hold one thing, not three, or every
// tool struct in the package has to carry three fields that are never used
// independently.
//
// Every type in these three signatures is declared in this package or the
// standard library, which is what makes them expressible at all: before
// decision 283 the tools held *edgebiz.Usecase outright, and the reason is
// that the answer to "list the estate" was a []*model.Edge, so a tool that
// wanted six columns had no way to ask for six.
type EdgeCatalog interface {
	// ListCatalog returns the nodes matching f, most useful first as the
	// edge domain orders them.
	ListCatalog(ctx context.Context, f EdgeFilter) ([]EdgePresence, error)
	// Presence returns one node. A node that is not registered is reported
	// as found=false with a nil error, not as a nil row: the projection
	// principle in this file says the port may not make a claim the table
	// does not make, and "no such node" is an answer rather than a failure.
	Presence(ctx context.Context, id uint64) (EdgePresence, bool, error)
	// PresenceByName is Presence with a different key, and it is a fourth
	// method rather than a filter on the first because three of the tools
	// are handed a node's name by an operator and have nothing else to look
	// it up by. It was a fifth method on the concrete type before, named
	// GetByName, and folding it into ListCatalog would have meant every
	// name lookup loading the whole estate to find one row.
	PresenceByName(ctx context.Context, name string) (EdgePresence, bool, error)
	// PluginHealth returns the last health snapshot a node reported for its
	// plugins, or nil if none has arrived yet.
	PluginHealth(edgeID uint64) []PluginHealth
}
