package alert

import (
	"context"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// FiringCorrelator is asked, for every incoming firing, whether that firing
// belongs to a story somebody else already opened.
//
// The question used to be asked the other way round: the alert repository
// imported the demo model and asked "is this the demo scenario?", so the
// production alert store knew the demo existed, could name its state
// machine, and advanced it inside its own transaction (decision 113). That
// made alert → demo a live import while demo → alert was already structural
// (the scenario seeds real incidents), i.e. a cycle with a production store
// on one end of it.
//
// Inverting the question fixes the direction without changing a single
// behaviour: alert states a need, the owner of the story answers it, and the
// composition root connects them. The only implementation today is the
// pg-pool-exhaustion demo scenario, so the answer set is identical to what
// the hardcoded branch produced — but the next correlator is added by
// implementing one method, not by editing the alert store.
//
// The signature deliberately returns plain types rather than a named result
// struct, so an implementation only has to import model/alert (which every
// correlator needs anyway) and never biz/alert.
type FiringCorrelator interface {
	// CorrelateFiring reports the pre-opened Incident this firing belongs
	// to, advancing the story's own state. matched=false means the firing
	// is an ordinary one and must be ingested by the normal dedupe path.
	//
	// It runs on the ingest path, so it must not block on anything slow.
	CorrelateFiring(ctx context.Context, fingerprint string, labels map[string]string) (*model.Incident, bool, error)
}

// correlateFiring is the nil-safe call site. A platform with no correlator
// wired ingests every firing the ordinary way, which is what happened before
// the demo was ever built — so "not wired" and "nothing matched" are the same
// answer, and neither is an error.
func (u *Usecase) correlateFiring(ctx context.Context, fingerprint string, labels map[string]string) (*model.Incident, bool, error) {
	if u == nil || u.correlator == nil {
		return nil, false, nil
	}
	return u.correlator.CorrelateFiring(ctx, fingerprint, labels)
}
