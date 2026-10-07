package main

import (
	"context"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	internaldataguardlabel "github.com/vincent-wuhan/opskeeper/core/manager/dataguard/label"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
)

// sensitivityEscalator raises an approval's risk class from the label on
// whatever the action is about to touch.
//
// This is the approval half of the promise the read half started in decision
// 361: a resource labelled Restricted now costs a writer two signatures, not
// just a reader a tier. Before this file the vocabulary said TopSecret and
// Restricted "raise a call to dangerous" and the only implementation of that
// sentence lived in a PausePolicyImpl that production never constructed.
//
// It sits at the composition root for the same reason every other adapter
// here does: the rows are approval's and the labels are dataguard's, and
// neither domain may import the other.

// sensitiveLabeler is the narrow slice of the label store this needs.
type sensitiveLabeler interface {
	StrictestForResourceID(ctx context.Context, resourceID string) (dataguard.Sensitivity, bool, error)
}

type sensitivityEscalator struct {
	labels sensitiveLabeler
	log    *slog.Logger
}

// ClassFor implements approval.Escalator.
//
// The whole set is read and the strictest label wins. A call that reaches
// twelve devices is only as safe as the most sensitive one of them, and an
// implementation that stopped at the first would be undone by nothing more
// than reordering an argument list.
func (e *sensitivityEscalator) ClassFor(ctx context.Context, targets []string) (domain.ToolClass, bool, error) {
	if e == nil || e.labels == nil {
		return "", false, nil
	}
	worst := dataguard.Sensitivity("")
	found := false
	for _, target := range targets {
		sensitivity, labelled, err := e.labels.StrictestForResourceID(ctx, target)
		if err != nil {
			// Propagated rather than swallowed. The caller refuses to create
			// the row, and refusing to create a row is the correct answer to
			// "we could not find out how sensitive this is" — a proposal is
			// cheap to retry, and a wrongly-classified one is not.
			return "", false, err
		}
		if !labelled {
			continue
		}
		if !found || sensitivity.Compare(worst) > 0 {
			worst, found = sensitivity, true
		}
	}
	if !found {
		return "", false, nil
	}
	class, _ := dataguard.RequiredClass(worst)
	if e.log != nil {
		e.log.Info("approval targets carry a sensitivity label",
			slog.Int("targets", len(targets)),
			slog.String("sensitivity", string(worst)))
	}
	return class, class != "", nil
}

func newSensitivityEscalator(labels sensitiveLabeler, log *slog.Logger) *sensitivityEscalator {
	if labels == nil {
		return nil
	}
	return &sensitivityEscalator{labels: labels, log: log}
}

var _ managerbizapproval.Escalator = (*sensitivityEscalator)(nil)
var _ sensitiveLabeler = (*internaldataguardlabel.LabelManager)(nil)
