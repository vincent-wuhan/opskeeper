package repairpreview

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type TargetIdentity struct {
	ScenarioID          string
	TargetFingerprint   string
	WorkloadFingerprint string
}

func ReadTargetIdentity(ctx context.Context, database *sql.DB) (TargetIdentity, error) {
	if database == nil {
		return TargetIdentity{}, errors.New("repair preview target identity: database is required")
	}
	row := database.QueryRowContext(ctx, `
		SELECT scenario_id, target_fingerprint, workload_fingerprint
		FROM repair_preview_target_identity
		WHERE singleton = TRUE
	`)
	var identity TargetIdentity
	if err := row.Scan(&identity.ScenarioID, &identity.TargetFingerprint, &identity.WorkloadFingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TargetIdentity{}, errors.New("repair preview target identity is missing")
		}
		return TargetIdentity{}, fmt.Errorf("read repair preview target identity: %w", err)
	}
	if identity.ScenarioID == "" || identity.TargetFingerprint == "" || identity.WorkloadFingerprint == "" {
		return TargetIdentity{}, errors.New("repair preview target identity is incomplete")
	}
	return identity, nil
}
