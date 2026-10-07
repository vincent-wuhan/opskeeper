package main

// Cost-crystallisation wiring (plan item 7, §4.96).
//
// The learner is the object the ledger was missing: it holds the cross-run
// streaks and is told by the orchestrator about every recovery that verified
// on the first try, so a fault that keeps coming back stops paying a model to
// be re-diagnosed three times before anything is written down as a runbook.
//
// It is a function rather than an inline block in main() for the reason every
// other wiring in cmd/opskeeper is (see loop_adapters.go): the interesting
// properties of this wiring are decisions, and a decision buried in a
// 2,500-line main() is one no test can reach. Three of them, in order of how
// expensive they are to get wrong:
//
//  1. The crystalliser is off exactly when there is no tool to learn from.
//     Without a registry a recovery cannot be graded, and the learner would
//     refuse every run for want of a class — a permanently-off feature that
//     looks like a healthy one, because "no errors" is what it reports.
//  2. The console and the loop read the *same* ledger. Two ledgers, or a
//     console wired to a different object than the orchestrator tells, is an
//     approval surface that shows an empty history while runs are being
//     counted somewhere nobody can see.
//  3. The class on a crystallised declaration comes from the registry's risk
//     grade, never from the action's name. This is the one that decides
//     whether a runbook may be dispatched with no model in the path.
//
// The trigger source is built here too, so "the crystalliser is on" and "a
// trigger can be named" cannot be true at different times.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	managersvcplugin "github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
	managerbizcrystallizehook "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallizehook"
	manberalbizalert "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	managerserveraiops "github.com/vincent-wuhan/opskeeper/core/manager/server/aiops"
)

// crystallizeBlastRadius is the reach a pattern proven on this deployment is
// allowed to carry. It is policy, not evidence: the loop's approval carries a
// target, not a radius, so the operator's default is stamped here. A runbook
// that fixes one pod is not the same claim as one that may act on a namespace.
const crystallizeBlastRadius = domain.RadiusSingleNS

// crystallizeTTL is how long a crystallised action may stay live before an
// operator has to look at it again. The ledger caps it further.
const crystallizeTTL = 15 * time.Minute

// crystallizeStatePath is where the ledger's promotion progress is kept.
//
// It has a default for the same reason the federation ledger does, and the
// reason is the one that decides this whole feature. A default of "" would be
// a technically-correct, practically-empty fix: the environment variable
// would be read by deployments that set it, which after this change is none
// of them, and every default deployment would keep re-arming a three-run
// streak on every restart. That is the failure this path exists to remove, so
// the path is a default and the empty string is the opt-out.
//
// A deployment that wants an in-memory ledger sets it to the empty string.
// One that cannot write the default path gets a warning at boot and an
// in-memory ledger, which is what it had before this file existed.
func crystallizeStatePath() string {
	return firstNonEmpty(os.Getenv("OPSKEEPER_CRYSTALLIZE_STATE"),
		"/var/lib/opskeeper/crystallize/ledger.json")
}

// crystallizedReviewSurface is the console half of the crystalliser: the
// handler that lists promoted patterns and renders the draft package each
// one would install.
//
// It is an interface rather than *managerserveraiops.Handler because the
// wiring must be reachable from a test, and a test cannot boot the control
// plane to check a setter was called. The code sits on the aiops handler
// because the crystalliser is in the aiops domain (aiops -> loop is the
// declared direction, decision 117; a loop-side endpoint on an aiops package
// would close a cycle). The route paths keep the /v1/loops prefix — these
// patterns are the loop's own history — but the code sits where the edge
// points.
type crystallizedReviewSurface interface {
	SetPatterns(managerserveraiops.PatternReader)
	SetDraftRoot(dir string)
	SetDraftReleaser(managerserveraiops.DraftReleaser)
}

// crystallizedReleaser adapts the plugin release manager to the one call the
// crystallised review surface makes.
//
// It exists so the surface does not depend on the release service, and so the
// release service does not grow a second entry point for the same Start.
// Every field of the answer is the release manager's own status: this adapter
// translates shapes and invents nothing, because a release route that
// reported a plan of its own would be a second answer to "what is happening
// to the fleet right now".
type crystallizedReleaser struct {
	mgr *managersvcplugin.Manager
}

func (c crystallizedReleaser) StartRelease(
	_ context.Context, req managerserveraiops.ReleaseRequest, strategy string,
) (managerserveraiops.ReleaseHandle, error) {
	status, err := c.mgr.Start(context.Background(), managersvcplugin.StartRequest{
		Name:      req.Name,
		Version:   req.Version,
		URL:       req.URL,
		SHA256:    req.SHA256,
		Signature: req.Signature,
		KeyID:     req.KeyID,
		Strategy:  strategy,
		Nodes:     req.Nodes,
	})
	if err != nil {
		return managerserveraiops.ReleaseHandle{}, err
	}
	return managerserveraiops.ReleaseHandle{
		Plugin:   status.Plugin,
		Version:  status.Version,
		Strategy: strategy,
		Wave:     status.Wave,
		Waves:    status.Waves,
		Progress: status.Summary,
	}, nil
}

// loopCrystallization is what boot hands the orchestrator, plus the learner
// behind it so a caller can tell "off" from "wired but silent".
//
// The zero value is the off state, and it is what a caller gets when there is
// nothing to learn from or when the learner refuses to construct. Both are
// ordinary: cost crystallisation is a saving, not a service, and a
// deployment that cannot do it is still a working deployment.
type loopCrystallization struct {
	// crystallizer is nil when crystallisation is off, and the
	// orchestrator treats nil as "learn as a producer would without one".
	crystallizer managerbizloop.RecoveryCrystallizer
	// triggers names the detection signal for an incident, and is nil on
	// the same condition as crystallizer.
	triggers managerbizloop.AutonomyTriggerSource
	// learner is the object that owns the ledger, kept so a caller can
	// reach the ledger itself rather than the interface it satisfies.
	learner *managerbizcrystallizehook.Learner
	// tools is how many tools the registry held when the decision was
	// made — the number an operator needs to understand why it is on.
	tools int
}

// enabled reports whether cost crystallisation is wired. It is the single
// answer to "is this feature on", so a caller does not have to infer it from
// a non-nil interface.
func (c loopCrystallization) enabled() bool { return c.crystallizer != nil }

// newLoopCrystallization decides whether cost crystallisation is on and, if it
// is, builds the learner, the trigger source, and the console's view of the
// same ledger.
//
// tools is the registry the loop dispatches remediations through; the switch
// is derived from it here rather than passed in, because a caller that
// computed the count itself would be able to disagree with the source the
// learner grades against — and that disagreement is silent.
func newLoopCrystallization(
	tools *middlewareregistry.Registry,
	alerts manberalbizalert.Repo,
	review crystallizedReviewSurface,
	log *slog.Logger,
) (loopCrystallization, error) {
	var out loopCrystallization
	if tools != nil {
		out.tools = len(tools.ListTools(""))
	}
	if out.tools == 0 {
		// Not an error and not worth a warning: a deployment with no
		// remediation adapter is a deployment whose loop can diagnose but
		// not fix, so there is nothing for a runbook to be made of.
		return out, nil
	}

	if alerts == nil {
		// NewAlertTriggerAdapter panics on a nil repo, and a panic during
		// boot over a cost-saving side feature is a bad trade. The loop
		// builds its trigger from the incident's own rule, so without the
		// repo there is no "when" half of a runbook and the feature has
		// nothing to offer.
		return loopCrystallization{}, errors.New("loop: cost crystallisation needs the alert repo to name a trigger, and there is none")
	}

	learner, err := managerbizcrystallizehook.New(tools, managerbizcrystallizehook.Config{
		// The class is a property the registry states about the tool, so
		// the mapping is the one place a risk level becomes a class.
		ToolClass:   riskLevelToToolClass,
		BlastRadius: crystallizeBlastRadius,
		TTL:         crystallizeTTL,
		StatePath:   crystallizeStatePath(),
	}, log.With(slog.String("comp", "crystallize")))
	if err != nil {
		return loopCrystallization{}, err
	}

	out.learner = learner
	out.crystallizer = learner
	out.triggers = managerbizloop.NewAlertTriggerAdapter(loopAlertReader{repo: alerts}, log)
	// The review surface reads from the same ledger the loop writes to.
	review.SetPatterns(learner.Ledger())
	review.SetDraftRoot(os.Getenv("OPSKEEPER_PLUGIN_IMPORT_DIR"))
	return out, nil
}
