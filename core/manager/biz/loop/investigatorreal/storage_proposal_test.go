package investigatorreal

import (
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// A measured, nearly-full volume is the evidence for both remedies, and it
// is the only thing in this branch that can produce them: the restart-rate
// metric, the rollout probe and the node list all look healthy while a
// filesystem runs out of room.
func TestProposals_GrowAndCleanAVolumeTheMeasurementFoundFull(t *testing.T) {
	actions := proposedActions(t, "k8s", probeEvidence("k8s.pvc_usage",
		map[string]any{
			"name": "data-pvc", "namespace": "test",
			"measured": true, "used_percent": 99.0, "used_bytes": 1.0e10, "total_bytes": 1.01e10,
		},
	))
	resize, ok := actions["k8s.resize_pvc"]
	if !ok {
		t.Fatalf("a volume measured at 99%% must produce k8s.resize_pvc, got %v", actions)
	}
	if resize.Risk != "mutating" {
		t.Errorf("resize_pvc risk = %q, want mutating", resize.Risk)
	}
	cleanup, ok := actions["k8s.cleanup_logs"]
	if !ok {
		t.Fatalf("a full volume must also offer the immediate remedy, got %v", actions)
	}
	// Truncating logs cannot be undone by growing the volume afterwards, so
	// this one is graded above every other proposal in this branch.
	if cleanup.Risk != "dangerous" {
		t.Errorf("cleanup_logs risk = %q, want dangerous — truncating log content is not reversible", cleanup.Risk)
	}
}

// The measurement failing is not a measurement of zero.
//
// This is the assertion that keeps the probe honest: k8s.pvc_usage reports
// measured=false when it cannot find a pod, cannot find a mount point, or
// finds an image with no df. Every one of those volumes has an unknown
// used share, and "unknown" must not be turned into "fine" — otherwise
// every namespace where the adapter lacks permission to exec would produce a
// confident resize proposal for a disk nobody looked at.
func TestProposals_DoNotGrowAVolumeNobodyCouldMeasure(t *testing.T) {
	actions := proposedActions(t, "k8s", probeEvidence("k8s.pvc_usage",
		map[string]any{
			"name": "data-pvc", "namespace": "test",
			"measured": false, "unmeasured_reason": "no running pod in namespace test mounts this claim",
			"requested_storage": "100Gi",
		},
	))
	if _, ok := actions["k8s.resize_pvc"]; ok {
		t.Fatal("an unmeasurable volume must not produce a resize proposal; unknown usage is not healthy usage")
	}
	if _, ok := actions["k8s.cleanup_logs"]; ok {
		t.Fatal("an unmeasurable volume must not produce a log-cleanup proposal either")
	}
}

// A volume that was measured and came back healthy is a real observation of
// a healthy volume, and it is the opposite case from the one above: this one
// genuinely says the disk is fine.
func TestProposals_DoNotGrowAVolumeThatWasMeasuredAndIsFine(t *testing.T) {
	actions := proposedActions(t, "k8s", probeEvidence("k8s.pvc_usage",
		map[string]any{"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 41.0},
	))
	if _, ok := actions["k8s.resize_pvc"]; ok {
		t.Fatal("41%% full is not a reason to resize; the floor exists so proposals appear before the disk fails")
	}
}

// One full volume among several resolves. Two refuses — and the refusal has
// to name both, because "which of these two disks" is the question the
// approval step exists to answer.
func TestProposals_TwoFullVolumesOfferTheRemedyButTheResolverRefusesToPick(t *testing.T) {
	evidence := []loop.EvidenceItem{
		probeEvidence("k8s.pvc_usage",
			map[string]any{"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 99.0},
			map[string]any{"name": "logs-pvc", "namespace": "test", "measured": true, "used_percent": 95.0},
		),
	}
	actions := proposedActions(t, "k8s", evidence...)
	if _, ok := actions["k8s.resize_pvc"]; !ok {
		t.Fatalf("two full volumes must still produce the remedy, got %v", actions)
	}
	if rows := loop.FullClaimRows(evidence); len(rows) != 2 {
		t.Fatalf("FullClaimRows returned %d rows, want 2", len(rows))
	}
}

// The same claim name in two namespaces is two different volumes. A
// resolver that collapsed them onto the bare name would either call that
// agreement or call it ambiguity, and both would be wrong.
func TestFullClaimRows_TreatsNamespaceAndNameAsOneIdentity(t *testing.T) {
	evidence := []loop.EvidenceItem{
		probeEvidence("k8s.pvc_usage",
			map[string]any{"name": "data", "namespace": "team-a", "measured": true, "used_percent": 99.0},
			map[string]any{"name": "data", "namespace": "team-b", "measured": true, "used_percent": 99.0},
		),
	}
	if rows := loop.FullClaimRows(evidence); len(rows) != 2 {
		t.Fatalf("two claims both named data in different namespaces are two volumes; got %d rows", len(rows))
	}
}

// Rows from repeated calls to one probe are all still evidence. The chain
// measures each claim in its own call, and reading only the first would make
// the loop answer about whichever volume happened to be measured first.
func TestProbeRows_CollectsEveryCallOfTheSameProbe(t *testing.T) {
	evidence := []loop.EvidenceItem{
		probeEvidence("k8s.pvc_usage",
			map[string]any{"name": "a-pvc", "namespace": "test", "measured": true, "used_percent": 99.0},
		),
		probeEvidence("k8s.pvc_usage",
			map[string]any{"name": "b-pvc", "namespace": "test", "measured": true, "used_percent": 97.0},
		),
	}
	rows := loop.ProbeRows(evidence, "k8s.pvc_usage")
	if len(rows) != 2 {
		t.Fatalf("ProbeRows returned %d rows across two calls, want 2", len(rows))
	}
	if got := loop.FullClaimRows(evidence); len(got) != 2 {
		t.Fatalf("FullClaimRows saw %d of the two recorded volumes, want 2", len(got))
	}
}
