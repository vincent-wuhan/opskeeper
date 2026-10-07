package main

// Closed-loop remediation adapter wiring.
//
// The approved phase of the closed loop dispatches a RemediationOption by
// looking its Action up in a tool registry and calling it. Until the
// adapters existed the registry was assembled from exactly one of them
// (PostgreSQL, behind OPSKEEPER_LOOP_PG_DSN), so every other action the
// investigator can write into a contract — k8s.rollout_undo,
// mq.drain_queue, host.restart_service — resolved to a name with nothing
// behind it. The gate that reports this (cmd/opskeeper-eval vocabulary,
// "Loop action executability") went green on names once the adapters
// existed; this file is what makes the names true in a running deployment
// rather than only in the report.
//
// Which adapters those are, which environment variable each reads, and how
// each is constructed are not written here. They are toolset.Sources(), the
// same catalog the manifest generator and the capability gate read, so a
// family that is wired into a running control plane is by construction a
// family the gate counts. This file's remaining job is the part that is
// genuinely the boot path's: read the environment, connect what is
// configured, skip what is not, and hand back the closers.
//
// Wiring is opt-in per adapter, and each one is independent: a deployment
// that only points at PostgreSQL behaves exactly as before, and one that
// adds a Kubernetes DSN gets those tools too without a rebuild. A DSN that
// fails to connect is logged and skipped rather than fatal, because the
// alternative — refusing to boot because one of five adapters is unreachable
// — would take the control plane down over a remediation convenience.

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	middlewareadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/toolset"
)

// riskLevelToToolClass maps a registry risk grade to the class a crystallised
// declaration is graded on.
//
// It lives at the assembly root because it is the one place both the registry
// (which states a risk) and the domain (which states a class) are visible.
// The mapping is deliberately coarser than the five-level ladder: the
// crystalliser's safety question is "may a node run this with no model in
// the path", and it answers that from the class. An unknown grade returns
// false so the learner refuses the run rather than defaulting a safety level.
func riskLevelToToolClass(risk string) (domain.ToolClass, bool) {
	switch middlewareadapter.RiskLevel(strings.TrimSpace(risk)) {
	case middlewareadapter.RiskL0ReadOnly, middlewareadapter.RiskL1Diagnostic:
		return domain.ClassRead, true
	case middlewareadapter.RiskL2SoftWrite:
		return domain.ClassWrite, true
	case middlewareadapter.RiskL3HardWrite, middlewareadapter.RiskL4Destructive:
		return domain.ClassDestructive, true
	default:
		return domain.ClassUnknown, false
	}
}

// wireLoopRemediationAdapters connects every catalog entry whose DSN is
// configured, registers its tools into reg, and returns the closers to run
// at shutdown.
//
// The returned slice is empty when nothing is configured, which is the same
// observable state as before any adapter existed: an empty registry and an
// invoker that refuses with a reason instead of advancing the run.
func wireLoopRemediationAdapters(ctx context.Context, log *slog.Logger, reg *middlewareregistry.Registry) []func() {
	var closers []func()
	for _, src := range toolset.Sources() {
		dsn := strings.TrimSpace(os.Getenv(src.Env))
		if dsn == "" {
			continue
		}
		connectCtx, cancel := context.WithTimeout(ctx, toolset.ConnectTimeout)
		closeFn, err := src.Wire(connectCtx, reg, dsn)
		cancel()
		if err != nil {
			// The env var names the adapter in full rather than a
			// credential, so quoting it back is safe and is the difference
			// between "something failed" and "this deployment's k8s DSN is
			// wrong".
			log.Error("loop: remediation adapter failed to connect; its tools will not be dispatchable",
				slog.String("adapter", string(src.Name)), slog.String("env", src.Env), slog.Any("err", err))
			continue
		}
		closers = append(closers, closeFn)
		log.Info("loop: remediation adapter wired", slog.String("adapter", string(src.Name)))
	}
	return closers
}
