package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
	dglabel "github.com/vincent-wuhan/opskeeper/core/manager/dataguard/label"
	iambizauthz "github.com/vincent-wuhan/opskeeper/core/manager/iam/biz/authz"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// sensitivityGate is the answer to ports.SensitivityGate, assembled from the
// two halves that already exist and had never been joined: the label store
// knows how sensitive a resource is, and the iam enforcer knows whether this
// caller may read at that level.
//
// It lives at the assembly root for the same reason every other cross-boundary
// adapter does: the tool chain asks a question, iam answers it, and neither
// side may import the other.

// sensitivityAuthorizer is the narrow slice of the iam enforcer this gate
// needs. It is named rather than taken as *authz.Enforcer so the gate can be
// tested against a double, and so a future move of the tier table does not
// reach into this file.
type sensitivityAuthorizer interface {
	AllowWithSensitivity(ctx context.Context, userID, orgID uint64, obj, act string,
		sens dataguard.Sensitivity, tierRepo iambizauthz.SensitivityTierRepo) (bool, error)
	UserOrgs(ctx context.Context, userID uint64) ([]uint64, error)
}

// sensitivityLabeler is the narrow slice of the label store.
type sensitivityLabeler interface {
	ResolveEffective(ctx context.Context, resourceType, resourceID string) (dataguard.Sensitivity, float64, bool, error)
}

type sensitivityGate struct {
	labels   sensitivityLabeler
	authz    sensitivityAuthorizer
	tierRepo iambizauthz.SensitivityTierRepo
	log      *slog.Logger
}

// Check answers whether the caller on ctx may act on this resource.
//
// The order is a decision rather than an accident of how the lines came out:
// the label is read first, because a resource nobody has labelled needs no
// tier at all, and asking the authorization stack about an unlabeled resource
// is a database round trip that can only ever answer "yes".
func (g *sensitivityGate) Check(ctx context.Context, resourceType, resourceID string) error {
	if g == nil || g.labels == nil || g.authz == nil {
		// Not wired is not a denial. A deployment without Data-Guard keeps
		// running the way it did before this gate existed.
		return nil
	}
	if resourceType == "" || resourceID == "" {
		// The call names no resource the labels are keyed by. See
		// ports.SensitivityGate for why this is an allow and not a refusal.
		return nil
	}
	sensitivity, _, _, err := g.labels.ResolveEffective(ctx, resourceType, resourceID)
	if err != nil {
		// A lookup that fails is not an unlabeled resource: treating it as
		// one would turn a database outage into a silent authorization
		// downgrade. The tool fails instead.
		return fmt.Errorf("sensitivity: label lookup for %s/%s failed: %w", resourceType, resourceID, err)
	}
	if sensitivity == "" || sensitivity == dataguard.Public || sensitivity == dataguard.Internal {
		return nil
	}
	tenant, ok := tenantctx.From(ctx)
	if !ok || tenant.UserID == 0 {
		return nil
	}
	orgs, err := g.authz.UserOrgs(ctx, tenant.UserID)
	if err != nil {
		return fmt.Errorf("sensitivity: resolve the caller's orgs: %w", err)
	}
	// Any org the caller belongs to is an org they may act within, so the
	// tier is satisfied by the first org that grants it. Requiring it in
	// every org would make membership in a second org a downgrade.
	for _, orgID := range orgs {
		allowed, err := g.authz.AllowWithSensitivity(ctx, tenant.UserID, orgID,
			resourceType, "read", sensitivity, g.tierRepo)
		if err != nil {
			return fmt.Errorf("sensitivity: authorize %s/%s: %w", resourceType, resourceID, err)
		}
		if allowed {
			return nil
		}
	}
	if g.log != nil {
		g.log.Warn("sensitivity: the caller's tier does not meet the resource's label",
			slog.Uint64("user_id", tenant.UserID),
			slog.String("resource", resourceType+"/"+resourceID),
			slog.String("sensitivity", string(sensitivity)))
	}
	return fmt.Errorf("%s/%s is labelled %s and your reader tier does not reach it",
		resourceType, resourceID, sensitivity)
}

// CheckAll answers about a whole set in one pass.
//
// The set is walked rather than answered by its first member, and the caller
// is authorized once for the strictest label in the set: a device list where
// one device is Restricted and the rest are Internal is a call that needs a
// Restricted reader, and asking per id would both round-trip the caller for
// every device and refuse a caller the tier table would in fact allow.
func (g *sensitivityGate) CheckAll(ctx context.Context, resourceType string, resourceIDs []string) error {
	if g == nil || g.labels == nil || g.authz == nil {
		return nil
	}
	if resourceType == "" || len(resourceIDs) == 0 {
		return g.Check(ctx, resourceType, "")
	}
	// An unlabeled resource ranks lowest, so a list of them leaves worstID
	// empty and is allowed without ever asking iam.
	worst := dataguard.Sensitivity("")
	worstID := ""
	for _, id := range resourceIDs {
		if id == "" {
			continue
		}
		sensitivity, _, _, err := g.labels.ResolveEffective(ctx, resourceType, id)
		if err != nil {
			// Same rule as the single case: a lookup that fails is not an
			// unlabeled resource, and a partially resolved list is not a
			// partially authorized one.
			return fmt.Errorf("sensitivity: label lookup for %s/%s failed: %w", resourceType, id, err)
		}
		// The level ordering is the dataguard module's own; a second copy of
		// the ranking table here would be a second place to forget TopSecret.
		if sensitivity.Compare(worst) > 0 {
			worst, worstID = sensitivity, id
		}
	}
	if worstID == "" {
		return nil
	}
	return g.Check(ctx, resourceType, worstID)
}

// newSensitivityGate assembles the gate, or nil when Data-Guard is off.
func newSensitivityGate(
	labels sensitivityLabeler,
	authzEnforcer sensitivityAuthorizer,
	tierRepo iambizauthz.SensitivityTierRepo,
	log *slog.Logger,
) ports.SensitivityGate {
	if labels == nil || authzEnforcer == nil {
		return nil
	}
	return &sensitivityGate{labels: labels, authz: authzEnforcer, tierRepo: tierRepo, log: log}
}

var _ ports.SensitivityGate = (*sensitivityGate)(nil)
var _ ports.MultiResourceGate = (*sensitivityGate)(nil)
var _ sensitivityLabeler = (*dglabel.LabelManager)(nil)
