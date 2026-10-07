package domain

// This file is the answer to a second measurement. `domaincheck -edges` priced
// the `agentteams -> alert` edge at three types and two methods, and the three
// types were a twenty-five-column GORM entity, a six-field filter struct and a
// status constant.
//
// The consumer is `core/manager/server/agentteams`, and what it does with an
// alert incident is this: when an AgentTeams run reports that an incident has
// recovered, find the still-open alert incident carrying that incident's id
// in its labels, and close it. That is the whole relationship.
//
// So of twenty-five columns it reads two — DedupeKey, because that is what
// the resolve call is keyed by, and LabelsJSON, because that is where the
// incident id lives. The other twenty-three are not read at all, and three
// groups of them could not be: Rule / Severity / Scope / ScopeType / Summary
// are what an operator reads on a page this consumer never renders,
// EventCount and the three timestamps are how the alert domain counts and
// ages its own rows, and ResolvedAt / ResolvedBy / AcknowledgedAt are
// bookkeeping on a row whose whole point here is that it is still open.
//
// The status constant is a stronger deletion than it looks. The consumer
// only ever asks for open incidents, so "open" was not a filter it set —
// it was a precondition of the question. Folding it into the method name
// means a future caller cannot ask for resolved ones by accident, and it
// removes the one field that would have let a caller believe the projection
// was a general-purpose view of incidents.

// OpenAlert is one open alert incident as a consumer of "close the alert this
// run recovered" needs to see it.
//
// Deliberately not the entity: no gorm tags, no timestamps, no notification
// bookkeeping. A projection that carries a row's bookkeeping will be carried
// forever — the next person to add a column to the entity adds it here too,
// and the boundary widens back to twenty-five without anybody deciding to.
type OpenAlert struct {
	// DedupeKey identifies the incident. It is the key the resolve call
	// takes, so it is the one field that cannot be projected away.
	DedupeKey string
	// LabelsJSON is the raw label document. It stays raw rather than
	// becoming a map for the same reason DedupeKey is not a parsed id: this
	// consumer asks one question of it ("does any label pair equal this
	// incident id"), and a parsed map would be the alert domain's schema
	// appearing on this side of a boundary it has no opinion about.
	LabelsJSON string
}
