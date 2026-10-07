package chatdiagnose

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/chatdiagnose"
)

type fakePatternSaver struct {
	saved []*model.IncidentPattern
	err   error
}

func (f *fakePatternSaver) Save(_ context.Context, p *model.IncidentPattern) error {
	f.saved = append(f.saved, p)
	return f.err
}

// TestPatternLearnerDerivesThePinnedRow pins the fingerprint and the
// signature as literals.
//
// Fingerprint is half of this table's UNIQUE (tenant_id, fingerprint) index.
// Before decision 114 the loop derived it — and nothing tested that
// derivation, so it was free to drift with no failing test. If one of these
// two literals changes, a postmortem stops deduplicating against the rows
// already in the table and the knowledge base grows a duplicate per
// postmortem. That is a data migration, not a code fix, so it has to be a
// deliberate, visible change.
func TestPatternLearnerDerivesThePinnedRow(t *testing.T) {
	saver := &fakePatternSaver{}
	learner := NewPatternLearner(saver)
	now := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)

	if err := learner.LearnFromPostmortem(context.Background(), loop.PostmortemDigest{
		IncidentID:     "inc-1",
		CommitSHA:      "commit-abc",
		RootCause:      "connection pool exhausted under sustained load",
		Summary:        "P1 saturation of the pg pool",
		LessonsLearned: "size the pool against peak concurrency",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("LearnFromPostmortem: %v", err)
	}
	if len(saver.saved) != 1 {
		t.Fatalf("saved rows = %d; want 1", len(saver.saved))
	}
	got := saver.saved[0]
	if got.Signature != "incident:connection pool exhausted under sustained load:high" {
		t.Errorf("signature = %q", got.Signature)
	}
	if got.Fingerprint != "284e7262a76b03f9" {
		t.Errorf("fingerprint = %q; want 284e7262a76b03f9 — this is the dedup key", got.Fingerprint)
	}
	if got.Severity != "high" {
		t.Errorf("severity = %q; want high (P1 in the summary)", got.Severity)
	}
	if got.ResourceType != "incident" {
		t.Errorf("resource type = %q; want incident", got.ResourceType)
	}
	if got.RootCauseObject != "connection pool exhausted under sustained load" {
		t.Errorf("root cause object = %q", got.RootCauseObject)
	}
	if got.SourcePostmortemID != "commit-abc" {
		t.Errorf("source postmortem = %q; want the commit sha", got.SourcePostmortemID)
	}
	if got.Confidence != 0.5 {
		t.Errorf("confidence = %v; want the 0.5 the loop used to write", got.Confidence)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v / %v; want the digest's clock %v", got.CreatedAt, got.UpdatedAt, now)
	}
}

func TestPatternLearnerSeverityBranches(t *testing.T) {
	cases := []struct {
		name             string
		digest           loop.PostmortemDigest
		wantSeverity     string
		wantRootCauseLen int
		wantFingerprint  string
	}{
		{
			name:             "critical wins over high",
			digest:           loop.PostmortemDigest{RootCause: "node 7 disk full", Summary: "critical: no space left on device"},
			wantSeverity:     "critical",
			wantRootCauseLen: len("node 7 disk full"),
			wantFingerprint:  "f7289014b8c9d40e",
		},
		{
			name:             "empty root cause becomes unknown",
			digest:           loop.PostmortemDigest{RootCause: "   ", Summary: "a thing happened"},
			wantSeverity:     "medium",
			wantRootCauseLen: len("unknown"),
			wantFingerprint:  "3f256dbe80d083e5",
		},
		{
			name:             "root cause truncated at 64",
			digest:           loop.PostmortemDigest{RootCause: strings.Repeat("X", 90), Summary: "nothing alarming"},
			wantSeverity:     "medium",
			wantRootCauseLen: 64,
			wantFingerprint:  "60df481ccbeec78c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saver := &fakePatternSaver{}
			if err := NewPatternLearner(saver).LearnFromPostmortem(context.Background(), tc.digest); err != nil {
				t.Fatalf("LearnFromPostmortem: %v", err)
			}
			got := saver.saved[0]
			if got.Severity != tc.wantSeverity {
				t.Errorf("severity = %q; want %q", got.Severity, tc.wantSeverity)
			}
			if len(got.RootCauseObject) != tc.wantRootCauseLen {
				t.Errorf("root cause object length = %d; want %d", len(got.RootCauseObject), tc.wantRootCauseLen)
			}
			if got.Fingerprint != tc.wantFingerprint {
				t.Errorf("fingerprint = %q; want %q", got.Fingerprint, tc.wantFingerprint)
			}
		})
	}
}

// TestPatternLearnerTenantIsThePlaceholder documents the defect rather than
// hiding it. tenant_id is NOT NULL, is half of the unique index above, and is
// documented as enforcing cross-tenant isolation — yet every pattern written
// from a postmortem has always carried "". Carried over unchanged in
// decision 114 on purpose: populating it changes which rows deduplicate
// against each other, so it is a migration with its own decision, not a
// side effect of a refactor.
func TestPatternLearnerTenantIsThePlaceholder(t *testing.T) {
	saver := &fakePatternSaver{}
	if err := NewPatternLearner(saver).LearnFromPostmortem(context.Background(), loop.PostmortemDigest{
		CommitSHA: "commit-abc", Summary: "anything",
	}); err != nil {
		t.Fatalf("LearnFromPostmortem: %v", err)
	}
	if got := saver.saved[0].TenantID; got != "" {
		t.Fatalf("tenant id = %q; want \"\" — §4.52.2 explains why this must not change silently", got)
	}
}

func TestPatternLearnerNilSaverIsNoOp(t *testing.T) {
	// A platform with the knowledge base off must still run investigations.
	// An unwired learner has to be a no-op, not an error and not a panic.
	if err := NewPatternLearner(nil).LearnFromPostmortem(context.Background(), loop.PostmortemDigest{
		CommitSHA: "commit-abc", Summary: "anything",
	}); err != nil {
		t.Fatalf("nil saver err = %v; want nil", err)
	}
	var nilLearner *PatternLearner
	if err := nilLearner.LearnFromPostmortem(context.Background(), loop.PostmortemDigest{}); err != nil {
		t.Fatalf("nil receiver err = %v; want nil", err)
	}
}

func TestPatternLearnerPropagatesSaveError(t *testing.T) {
	wantErr := errors.New("qdrant unreachable")
	saver := &fakePatternSaver{err: wantErr}
	// The loop logs and continues on failure, so the error has to arrive —
	// a swallowed one would be reported as a successful write-back.
	if err := NewPatternLearner(saver).LearnFromPostmortem(context.Background(), loop.PostmortemDigest{
		CommitSHA: "commit-abc", Summary: "anything",
	}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v; want %v", err, wantErr)
	}
}

func TestPatternLearnerZeroCreatedAtFallsBackToNow(t *testing.T) {
	saver := &fakePatternSaver{}
	if err := NewPatternLearner(saver).LearnFromPostmortem(context.Background(), loop.PostmortemDigest{
		CommitSHA: "commit-abc", Summary: "anything",
	}); err != nil {
		t.Fatalf("LearnFromPostmortem: %v", err)
	}
	got := saver.saved[0]
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %v / %v; want non-zero", got.CreatedAt, got.UpdatedAt)
	}
}
