package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The fixture is one autonomy declaration, written out rather than built,
// because every field of it is a bound and a bound that is not visible is a
// bound nobody reads.
func manifest() domain.PluginManifest {
	return domain.PluginManifest{
		Metadata: domain.PluginMeta{Name: "opskeeper-sre-autonomy", Version: "0.1.0"},
		Spec: domain.PluginSpec{
			Targets:     domain.Targets{domain.TargetEdge},
			SafetyLevel: domain.SafetyL2,
			Autonomy: domain.AutonomyPolicy{
				OfflineAfter: domain.Duration(2 * time.Minute),
				Actions: []domain.AutonomyAction{{
					Name:           "restart-orders-on-disk-full",
					Tool:           "host_restart_service",
					Trigger:        domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
					Argv:           []string{"systemctl", "restart", "orders-api"},
					BlastRadius:    domain.RadiusPod,
					TTL:            domain.Duration(30 * time.Minute),
					IdempotencyKey: "restart-orders:{{target}}:{{window}}",
				}},
			},
		},
	}
}

type harness struct {
	arb    *Arbiter
	clock  *clock
	reach  *reach
	audit  *recordingAudit
	now    time.Time
	values map[string]float64
}

// metric is the node's own reading of a named metric. A node that has never
// scraped one has no entry, which is a different fact from a reading of zero
// and is reported differently.
func (h *harness) metric(name string) (float64, bool) {
	v, ok := h.values[name]
	return v, ok
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

type reach struct {
	mu     sync.Mutex
	online bool
	since  time.Time
}

func (r *reach) Reach() Reach {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Reach{Online: r.online, OfflineSince: r.since}
}

func (r *reach) goOffline(at time.Time) {
	r.mu.Lock()
	r.online = false
	r.since = at
	r.mu.Unlock()
}

func (r *reach) comeOnline() {
	r.mu.Lock()
	r.online = true
	r.mu.Unlock()
}

type recordingAudit struct {
	mu   sync.Mutex
	rows []Row
	err  error
}

func (a *recordingAudit) Record(_ context.Context, row Row) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.rows = append(a.rows, row)
	return nil
}

func (a *recordingAudit) all() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Row(nil), a.rows...)
}

var installedAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessFor(t, manifest())
}

// newHarnessFor builds the same node over a different declaration, so a test
// about one field does not have to restate the other eight.
func newHarnessFor(t *testing.T, m domain.PluginManifest) *harness {
	t.Helper()
	reg, err := NewRegistry([]domain.PluginManifest{m}, installedAt)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h := &harness{
		clock:  &clock{at: installedAt},
		reach:  &reach{online: true},
		audit:  &recordingAudit{},
		now:    installedAt,
		values: map[string]float64{"node_disk_used_ratio": 0.97},
	}
	arb, err := New(Options{
		Registry: reg,
		Link:     h.reach,
		Audit:    h.audit,
		Now:      h.clock.now,
		Detector: ValueDetector{Value: h.metric},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.arb = arb
	return h
}

func goodClaim() Claim {
	return Claim{
		Action:  "restart-orders-on-disk-full",
		Trigger: domain.AutonomyTrigger{Kind: domain.TriggerMetricAbove, Metric: "node_disk_used_ratio", Threshold: 0.92},
		Argv:    []string{"systemctl", "restart", "orders-api"},
		Target:  "orders-api-7d9",
		Window:  "2026-03-01T12",
	}
}

// goDark puts the node past the offline threshold.
func (h *harness) goDark() {
	h.reach.goOffline(h.clock.now())
	h.clock.advance(3 * time.Minute)
}

// --- the regression: nothing changes while the center is there ----------

// TestAConnectedNodeDefersEverything is the test the whole package exists
// to pass.
//
// Every other case in this file would be worthless if a node with autonomy
// installed behaved differently from one without it while the tunnel was up.
// So this asserts the strong form: a well-formed claim, on a node whose
// declarations match it exactly, on a fully connected link, is deferred
// with no row written and no key spent.
func TestAConnectedNodeDefersEverything(t *testing.T) {
	h := newHarness(t)
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	if d.Verdict != Defer {
		t.Fatalf("verdict = %s, want defer: a human can be asked, so a human is asked", d.Verdict)
	}
	if n := len(h.audit.all()); n != 0 {
		t.Errorf("audit rows = %d, want 0: nothing happened that needs recording", n)
	}
	if got := h.arb.Snapshot(); got.Run != 0 || got.Refused != 0 || got.Deferred != 1 {
		t.Errorf("stats = %+v, want one deferral and nothing else", got)
	}

	// And the key was not spent, so the outage that follows is not
	// greeted by a self-heal that has already been used up by a call that
	// never ran.
	h.goDark()
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Run {
		t.Errorf("after the outage the same claim = %s, want run: the connected call spent nothing", d.Verdict)
	}
}

// TestAClaimForSomethingNotDeclaredDefers is the second half of the
// regression.
//
// An unknown action name is not a refusal. It is a call the arbiter has no
// opinion on, and the authority for a call it has no opinion on is the
// approval gate. Refusing here would make this package a second policy
// engine, and two policy engines disagree at the worst possible moment.
func TestAClaimForSomethingNotDeclaredDefers(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	claim := goodClaim()
	claim.Action = "delete-the-cluster"
	d := h.arb.Adjudicate(context.Background(), claim)
	if d.Verdict != Defer {
		t.Fatalf("verdict = %s, want defer", d.Verdict)
	}
	if n := len(h.audit.all()); n != 0 {
		t.Errorf("audit rows = %d, want 0: a deferred call is somebody else's to record", n)
	}
}

// TestAShortOutageStillDefers is the threshold, and it is the case that
// makes the threshold worth having.
//
// A tunnel reconnect cycle is a stream of seconds-long losses. A node that
// started self-healing on those would run its whole autonomy list every
// time the center blinked, which is a way of turning a healthy fleet into
// one that restarts things for no reason.
func TestAShortOutageStillDefers(t *testing.T) {
	h := newHarness(t)
	h.reach.goOffline(h.clock.now())
	h.clock.advance(30 * time.Second) // under the declared 2m
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Defer {
		t.Fatalf("verdict = %s after 30s offline, want defer", d.Verdict)
	}
	// Just past it.
	h.clock.advance(91 * time.Second)
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Run {
		t.Errorf("verdict = %s after 121s offline, want run", d.Verdict)
	}
}

// TestTheLinkComingBackEndsAutonomyImmediately is the other half of the
// switch. A node that has been self-healing for twenty minutes and gets its
// tunnel back must stop, on the next call, without waiting for a TTL.
func TestTheLinkComingBackEndsAutonomyImmediately(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	claim := goodClaim()
	if d := h.arb.Adjudicate(context.Background(), claim); d.Verdict != Run {
		t.Fatalf("verdict = %s, want run", d.Verdict)
	}
	claim.Window = "2026-03-01T13"
	h.reach.comeOnline()
	d := h.arb.Adjudicate(context.Background(), claim)
	if d.Verdict != Defer {
		t.Fatalf("verdict = %s after reconnect, want defer", d.Verdict)
	}
	if d.Reason == "" {
		t.Error("the deferral gave no reason: the row an operator reads at 09:00 has to say why")
	}
}

// TestALinkThatNeverReportedGoingDownGrantsNothing covers the degenerate
// case that a "how long has it been down" question always has. A reach
// report with no OfflineSince is ambiguous, and the ambiguous reading must
// be the one that runs nothing.
func TestALinkThatNeverReportedGoingDownGrantsNothing(t *testing.T) {
	h := newHarness(t)
	h.reach.goOffline(time.Time{})
	h.clock.advance(time.Hour)
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Defer {
		t.Fatalf("verdict = %s, want defer: a link that never said when it went down has not established an outage", d.Verdict)
	}
}

// --- the refusals, which are the security tests -------------------------

// TestTheAutonomyClaimEscapes are the three escapes the plan names, and
// each is attempted the way an attacker would: not by declaring a wider
// action, which the loader refuses, but by claiming a narrow one and
// changing what it carries.
func TestTheAutonomyClaimEscapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Claim)
		want   string
	}{
		{
			name:   "an extra argument appended to the declared argv",
			mutate: func(c *Claim) { c.Argv = append(c.Argv, "--now") },
			want:   "not the declared",
		},
		{
			name:   "one argument swapped for another",
			mutate: func(c *Claim) { c.Argv[2] = "postgres" },
			want:   "not the declared",
		},
		{
			name:   "a shell in front of the declared argv",
			mutate: func(c *Claim) { c.Argv = []string{"/bin/sh", "-c", "systemctl restart orders-api"} },
			want:   "not the declared",
		},
		{
			name:   "the declared argv as a prefix of something longer",
			mutate: func(c *Claim) { c.Argv = []string{"systemctl", "restart", "orders-api", "&&", "rm", "-rf", "/var/log"} },
			want:   "not the declared",
		},
		{
			name:   "a different trigger, same action",
			mutate: func(c *Claim) { c.Trigger.Metric = "node_load_average" },
			want:   "is not the one",
		},
		{
			name: "the same metric with a lower threshold, so it fires earlier",
			mutate: func(c *Claim) {
				c.Trigger.Threshold = 0.5
			},
			want: "is not the one",
		},
		{
			name:   "an occurrence the caller forgot to name",
			mutate: func(c *Claim) { c.Window = "" },
			want:   "no occurrence",
		},
		{
			name:   "no resolved target for a key that is derived from one",
			mutate: func(c *Claim) { c.Target = "" },
			want:   "no target",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.goDark()
			claim := goodClaim()
			tc.mutate(&claim)
			d := h.arb.Adjudicate(context.Background(), claim)
			if d.Verdict != Refuse {
				t.Fatalf("verdict = %s, want refuse: %s", d.Verdict, tc.name)
			}
			if !strings.Contains(d.Reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", d.Reason, tc.want)
			}
			// A refusal is recorded, because the next question anybody asks
			// about a node acting oddly is "what did it try".
			rows := h.audit.all()
			if len(rows) != 1 || rows[0].Verdict != Refuse.String() {
				t.Errorf("audit rows = %+v, want one recorded refusal", rows)
			}
			// And nothing ran, which the key is the witness of. A refusal
			// that spent the key would deny the legitimate self-heal too,
			// so the honest claim is submitted afterwards and must still
			// be allowed: a tampered argv does not get to veto the real
			// outage, and neither does the attempt itself.
			honest := goodClaim()
			if d := h.arb.Adjudicate(context.Background(), honest); d.Verdict != Run {
				t.Errorf("the honest claim after a refused one = %s, want run: the escape attempt consumed the key", d.Verdict)
			}
		})
	}
}

// TestAClaimWithNoArgvRunsTheDeclaration covers the shape the host tool
// actually submits.
//
// The tool's parameters are an action name, a target and an occurrence, and
// that is the point of the tool: there is no field a model can put a command
// into. So the claim that arrives here has no argv at all, and requiring one
// would refuse every legitimate self-heal while protecting nothing — the
// vector that executes is the declaration's either way, which is why Perform
// runs d.Action.Argv and not the claim's.
//
// The companion case is in the table above: a claim that *does* carry an argv
// has to be carrying the declared one.
func TestAClaimWithNoArgvRunsTheDeclaration(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	claim := goodClaim()
	claim.Argv = nil

	d := h.arb.Adjudicate(context.Background(), claim)
	if d.Verdict != Run {
		t.Fatalf("verdict = %s (%s), want run: the host tool cannot express an argv and must not be punished for it", d.Verdict, d.Reason)
	}
	if len(d.Action.Argv) != 3 || d.Action.Argv[0] != "systemctl" {
		t.Fatalf("the decision carries argv %q, want the declaration's", d.Action.Argv)
	}
	if d.Key != "restart-orders:orders-api-7d9:2026-03-01T12" {
		t.Errorf("key = %q, want the key derived from the target and the occurrence", d.Key)
	}
}

// TestAReplayIsRefused is the third escape, and the one with the most
// damage: a self-heal that runs twice because a tunnel reconnected or an
// agent retried is an action taken twice on the strength of one trigger.
func TestAReplayIsRefused(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	claim := goodClaim()
	first := h.arb.Adjudicate(context.Background(), claim)
	if first.Verdict != Run {
		t.Fatalf("the first claim = %s, want run", first.Verdict)
	}
	if first.Key != "restart-orders:orders-api-7d9:2026-03-01T12" {
		t.Errorf("key = %q, want the host's own derivation", first.Key)
	}
	h.arb.Complete(context.Background(), first, ResultOK, 0)

	second := h.arb.Adjudicate(context.Background(), claim)
	if second.Verdict != Refuse {
		t.Fatalf("the replay = %s, want refuse", second.Verdict)
	}
	if !strings.Contains(second.Reason, "already ran") {
		t.Errorf("reason = %q, want it to name the earlier run", second.Reason)
	}
	if got := h.arb.Snapshot(); got.Replays != 1 {
		t.Errorf("replays = %d, want 1: this is the number an operator reads first", got.Replays)
	}

	// A different occurrence is a different key, and therefore a different
	// question — the refusal is about this self-heal, not about this
	// service for ever.
	later := claim
	later.Window = "2026-03-01T18"
	if d := h.arb.Adjudicate(context.Background(), later); d.Verdict != Run {
		t.Errorf("a later occurrence = %s, want run: the key is per incident, not per service", d.Verdict)
	}
}

// TestAnExpiredDeclarationRunsNothing is the TTL, and it is measured from
// when the package was admitted rather than from the first run, so a
// declaration cannot be kept alive by the outage it exists for.
func TestAnExpiredDeclarationRunsNothing(t *testing.T) {
	h := newHarness(t)
	h.goDark()                        // +3m
	h.clock.advance(28 * time.Minute) // total 31m, past the declared 30m
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	if d.Verdict != Refuse {
		t.Fatalf("verdict = %s after the TTL, want refuse", d.Verdict)
	}
	if !strings.Contains(d.Reason, "expired") {
		t.Errorf("reason = %q, want it to say the declaration expired", d.Reason)
	}
}

// TestAnUnreliableSpoolDoesNotStopTheNode is a judgement, and it is
// recorded as one.
//
// A full disk is an availability problem on a node that is already in
// trouble, and turning it into a correctness problem — refusing a legal
// self-heal because the evidence could not be written — means the node gets
// worse exactly when it is needed. The row is attempted, the failure is
// logged, and the verdict stands.
func TestAnUnreliableSpoolDoesNotStopTheNode(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	h.audit.err = errors.New("no space left on device")
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Run {
		t.Fatalf("verdict = %s, want run: a full disk is not a policy decision", d.Verdict)
	}
}

// TestTwoPackagesCannotClaimOneActionName is the collision the registry
// refuses at boot. Resolving it either way would run an argv the operator
// did not read for the name they were shown.
func TestTwoPackagesCannotClaimOneActionName(t *testing.T) {
	other := manifest()
	other.Metadata.Name = "opskeeper-sre-rogue"
	if _, err := NewRegistry([]domain.PluginManifest{manifest(), other}, installedAt); err == nil {
		t.Fatal("two packages declaring one action name were admitted")
	} else if !strings.Contains(err.Error(), "declared by both") {
		t.Errorf("error = %v, want it to name both packages", err)
	}
}

// TestTheStrictestThresholdWins is a small thing with a real failure behind
// it: a package that wants two minutes of human presence should not get one
// because another package asked for thirty seconds of it.
func TestTheStrictestThresholdWins(t *testing.T) {
	eager := manifest()
	eager.Metadata.Name = "opskeeper-sre-eager"
	eager.Spec.Autonomy.OfflineAfter = domain.Duration(30 * time.Second)
	eager.Spec.Autonomy.Actions[0].Name = "restart-cache-on-memory-pressure"
	eager.Spec.Autonomy.Actions[0].Tool = "host_restart_cache"
	reg, err := NewRegistry([]domain.PluginManifest{manifest(), eager}, installedAt)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got, want := reg.OfflineAfter(), domain.MinAutonomyOfflineAfter; got != want {
		t.Errorf("offline threshold = %s, want the strictest asked-for, floored at %s", got, want)
	}
}

// TestANodeWithNoAutonomyDefersEverything is the shape of a fleet that has
// never heard of this package: a registry with nothing in it must cost a
// mutex acquisition and nothing else.
func TestANodeWithNoAutonomyDefersEverything(t *testing.T) {
	plain := manifest()
	plain.Spec.Autonomy = domain.AutonomyPolicy{}
	reg, err := NewRegistry([]domain.PluginManifest{plain}, installedAt)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if !reg.Empty() {
		t.Fatal("a package with no autonomy block compiled into actions")
	}
	arb, err := New(Options{Registry: reg, Link: &reach{online: false, since: installedAt}, Now: func() time.Time { return installedAt }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claim := goodClaim()
	if d := arb.Adjudicate(context.Background(), claim); d.Verdict != Defer {
		t.Errorf("verdict = %s, want defer", d.Verdict)
	}
}

// TestTheRecorderGetsBothHalves pins the two-phase audit. A self-heal that
// began and never reported is the row that matters most, and it only exists
// if the decision is written before the work starts.
func TestTheRecorderGetsBothHalves(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	h.arb.Complete(context.Background(), d, ResultFailed, 1)
	rows := h.audit.all()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: the decision and then its outcome", len(rows))
	}
	if rows[0].Phase != PhaseDecided || rows[0].Verdict != Run.String() {
		t.Errorf("first row = %+v, want the decision", rows[0])
	}
	if rows[0].Result != "" {
		t.Error("the decision row carried a result: it was written before the action ran")
	}
	if rows[1].Phase != PhaseCompleted || rows[1].Result != ResultFailed || rows[1].ExitCode != 1 {
		t.Errorf("second row = %+v, want the failure", rows[1])
	}
	if rows[0].Package != "opskeeper-sre-autonomy" || rows[0].Tool != "host_restart_service" {
		t.Errorf("the row does not say which package ran what: %+v", rows[0])
	}
	if rows[0].Key != rows[1].Key {
		t.Error("the two halves name different keys, so a replay of the pair cannot be seen as one run")
	}
}

// --- the trigger is a condition, not a comment -------------------------

// TestTheTriggerHasToActuallyHold is the test that makes the field mean
// something. Every other bound would check out on a node whose disk is at
// 4% and the restart would happen anyway, with a manifest line that reads
// as if something had decided it should.
func TestTheTriggerHasToActuallyHold(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	h.values["node_disk_used_ratio"] = 0.41
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	if d.Verdict != Refuse {
		t.Fatalf("verdict = %s with the metric below its threshold, want refuse", d.Verdict)
	}
	// The reason has to be the one an operator can act on: this is the
	// difference between a node that was right to wait and a node whose
	// metric pipeline is dead.
	if !strings.Contains(d.Reason, "0.41") || !strings.Contains(d.Reason, "0.92") {
		t.Errorf("reason = %q, want it to report the reading and the threshold", d.Reason)
	}
	h.values["node_disk_used_ratio"] = 0.93
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Run {
		t.Errorf("verdict = %s with the metric past its threshold, want run", d.Verdict)
	}
}

// TestAMetricTheNodeHasNeverSeenGrantsNothing: a missing reading is not a
// reading of zero, and treating it as one would make the whole feature a
// coin flip on the health of the metric pipeline.
func TestAMetricTheNodeHasNeverSeenGrantsNothing(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	delete(h.values, "node_disk_used_ratio")
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	if d.Verdict != Refuse {
		t.Fatalf("verdict = %s, want refuse", d.Verdict)
	}
	if !strings.Contains(d.Reason, "never seen a sample") {
		t.Errorf("reason = %q, want it to say the metric is missing", d.Reason)
	}
}

// TestANodeThatCannotMeasureRunsNothingAtAll is the fail-closed default. A
// node with no detector could otherwise grant autonomy on declarations it
// cannot check, which is the one configuration where the trigger field is
// pure decoration.
func TestANodeThatCannotMeasureRunsNothingAtAll(t *testing.T) {
	reg, err := NewRegistry([]domain.PluginManifest{manifest()}, installedAt)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	arb, err := New(Options{Registry: reg, Link: &reach{}, Now: time.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Online, so the link is not what stops it.
	if d := arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Defer {
		t.Fatalf("verdict = %s on a linked node, want defer", d.Verdict)
	}
}
