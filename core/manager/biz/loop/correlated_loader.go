package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type ContractDetectionEventLoader struct {
	contracts ContractRepo
}

func NewContractDetectionEventLoader(contracts ContractRepo) ContractDetectionEventLoader {
	return ContractDetectionEventLoader{contracts: contracts}
}

func (l ContractDetectionEventLoader) Load(ctx context.Context, in PlanInput) (DetectionEvent, error) {
	if l.contracts == nil {
		return DetectionEvent{}, errors.New("loop: detection event contract repo is required")
	}
	contract, err := l.contracts.ReadContract(ctx, in.TenantID, in.IncidentID, PhaseDetected, "DetectedEvent")
	if err != nil {
		return DetectionEvent{}, fmt.Errorf("loop: read DetectedEvent contract: %w", err)
	}
	if contract == nil {
		return DetectionEvent{}, errors.New("loop: DetectedEvent contract is missing")
	}
	var event DetectionEvent
	if err := json.Unmarshal([]byte(contract.Payload), &event); err != nil {
		return DetectionEvent{}, fmt.Errorf("loop: decode DetectedEvent contract: %w", err)
	}
	if event.LabelSetKey == "" {
		return DetectionEvent{}, errors.New("loop: DetectedEvent contract missing labelsetkey")
	}
	return event, nil
}

var _ CurrentDetectionEventLoader = ContractDetectionEventLoader{}
