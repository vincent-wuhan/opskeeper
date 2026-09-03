package incident

import (
	"errors"
	"fmt"
	"time"
)

type Dataset struct {
	ID           string       `json:"id"`
	Title        string       `json:"title"`
	FaultFamily  string       `json:"fault_family"`
	SourceBasis  string       `json:"source_basis"`
	Assumptions  []string     `json:"assumptions"`
	Input        DatasetInput `json:"input"`
	RecoveryGate RecoveryGate `json:"recovery_gate"`
	Postmortem   Postmortem   `json:"postmortem"`
	CreatedAt    time.Time    `json:"created_at"`
}

type DatasetInput struct {
	Alert           Alert               `json:"alert"`
	MetricWindow    []MetricObservation `json:"metric_window"`
	SessionSnapshot SessionSnapshot     `json:"session_snapshot"`
	LockSnapshot    LockSnapshot        `json:"lock_snapshot"`
	Topology        Topology            `json:"topology"`
	Versions        Versions            `json:"versions"`
}

type Alert struct {
	IncidentID  string    `json:"incident_id"`
	TenantID    string    `json:"tenant_id"`
	Severity    string    `json:"severity"`
	Resource    string    `json:"resource"`
	Summary     string    `json:"summary"`
	FiredAt     time.Time `json:"fired_at"`
	EvidenceRef string    `json:"evidence_ref"`
	TraceID     string    `json:"trace_id"`
}

type MetricObservation struct {
	ObservedAt  time.Time         `json:"observed_at"`
	Name        string            `json:"name"`
	Value       float64           `json:"value"`
	Unit        string            `json:"unit"`
	Labels      map[string]string `json:"labels,omitempty"`
	EvidenceRef string            `json:"evidence_ref"`
}

type SessionSnapshot struct {
	CollectedAt time.Time          `json:"collected_at"`
	Summary     map[string]float64 `json:"summary"`
	Records     []SessionRecord    `json:"records"`
	EvidenceRef string             `json:"evidence_ref"`
}

type SessionRecord struct {
	PID             int    `json:"pid"`
	Role            string `json:"role"`
	State           string `json:"state"`
	WaitEventType   string `json:"wait_event_type"`
	WaitEvent       string `json:"wait_event"`
	ApplicationName string `json:"application_name"`
	TransactionAge  string `json:"transaction_age"`
}

type LockSnapshot struct {
	CollectedAt time.Time          `json:"collected_at"`
	Summary     map[string]float64 `json:"summary"`
	Records     []LockRecord       `json:"records"`
	EvidenceRef string             `json:"evidence_ref"`
}

type LockRecord struct {
	LockType    string `json:"lock_type"`
	Database    string `json:"database"`
	Relation    string `json:"relation"`
	Mode        string `json:"mode"`
	Granted     bool   `json:"granted"`
	BlockedPID  int    `json:"blocked_pid"`
	BlockingPID int    `json:"blocking_pid"`
	WaitTime    string `json:"wait_time"`
}

type Topology struct {
	Fingerprint string      `json:"fingerprint"`
	Components  []Component `json:"components"`
}

type Component struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	Parent   string `json:"parent,omitempty"`
	ReadOnly bool   `json:"read_only"`
}

type Versions struct {
	Database    string `json:"database"`
	Engine      string `json:"engine"`
	Major       int    `json:"major"`
	Minor       int    `json:"minor"`
	Patch       int    `json:"patch"`
	Agent       string `json:"agent"`
	Application string `json:"application"`
}

type RecoveryGate struct {
	ClosurePolicy        string             `json:"closure_policy"`
	RequiredObservations []string           `json:"required_observations"`
	Thresholds           map[string]float64 `json:"thresholds"`
	EvidenceRefs         []string           `json:"evidence_refs"`
}

func (dataset Dataset) Validate() error {
	switch {
	case dataset.ID == "":
		return errors.New("incident dataset: id is required")
	case dataset.Title == "", dataset.FaultFamily == "", dataset.SourceBasis == "":
		return errors.New("incident dataset: title, fault family, and source basis are required")
	case len(dataset.Assumptions) == 0:
		return errors.New("incident dataset: assumptions are required")
	case dataset.CreatedAt.IsZero():
		return errors.New("incident dataset: created_at is required")
	}
	if err := dataset.Input.Validate(); err != nil {
		return fmt.Errorf("dataset %s: %w", dataset.ID, err)
	}
	if err := dataset.RecoveryGate.Validate(); err != nil {
		return fmt.Errorf("dataset %s recovery gate: %w", dataset.ID, err)
	}
	if err := dataset.Postmortem.Validate(); err != nil {
		return fmt.Errorf("dataset %s postmortem: %w", dataset.ID, err)
	}
	return nil
}

func (input DatasetInput) Validate() error {
	if err := input.Alert.Validate(); err != nil {
		return fmt.Errorf("alert: %w", err)
	}
	if len(input.MetricWindow) < 3 {
		return errors.New("metric window: at least three observations are required")
	}
	for index, observation := range input.MetricWindow {
		if err := observation.Validate(); err != nil {
			return fmt.Errorf("metric %d: %w", index, err)
		}
	}
	if err := input.SessionSnapshot.Validate(); err != nil {
		return fmt.Errorf("session snapshot: %w", err)
	}
	if err := input.LockSnapshot.Validate(); err != nil {
		return fmt.Errorf("lock snapshot: %w", err)
	}
	if err := input.Topology.Validate(); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	if err := input.Versions.Validate(); err != nil {
		return fmt.Errorf("versions: %w", err)
	}
	return nil
}

func (alert Alert) Validate() error {
	switch {
	case alert.IncidentID == "", alert.TenantID == "":
		return errors.New("incident and tenant ids are required")
	case alert.Severity == "", alert.Resource == "", alert.Summary == "":
		return errors.New("severity, resource, and summary are required")
	case alert.FiredAt.IsZero():
		return errors.New("fired_at is required")
	case alert.EvidenceRef == "", alert.TraceID == "":
		return errors.New("evidence_ref and trace_id are required")
	}
	return nil
}

func (observation MetricObservation) Validate() error {
	switch {
	case observation.ObservedAt.IsZero(), observation.Name == "", observation.Unit == "":
		return errors.New("observed_at, name, and unit are required")
	case observation.EvidenceRef == "":
		return errors.New("evidence_ref is required")
	}
	return nil
}

func (snapshot SessionSnapshot) Validate() error {
	switch {
	case snapshot.CollectedAt.IsZero():
		return errors.New("collected_at is required")
	case len(snapshot.Summary) == 0:
		return errors.New("summary is required")
	case snapshot.EvidenceRef == "":
		return errors.New("evidence_ref is required")
	}
	return nil
}

func (snapshot LockSnapshot) Validate() error {
	switch {
	case snapshot.CollectedAt.IsZero():
		return errors.New("collected_at is required")
	case len(snapshot.Summary) == 0:
		return errors.New("summary is required")
	case snapshot.EvidenceRef == "":
		return errors.New("evidence_ref is required")
	}
	return nil
}

func (topology Topology) Validate() error {
	switch {
	case topology.Fingerprint == "":
		return errors.New("fingerprint is required")
	case len(topology.Components) < 2:
		return errors.New("at least two components are required")
	}
	for index, component := range topology.Components {
		if component.Role == "" || component.Name == "" {
			return fmt.Errorf("component %d: role and name are required", index)
		}
	}
	return nil
}

func (versions Versions) Validate() error {
	switch {
	case versions.Database == "", versions.Engine == "":
		return errors.New("database and engine are required")
	case versions.Major == 0 || versions.Minor == 0:
		return errors.New("major and minor versions are required")
	case versions.Agent == "", versions.Application == "":
		return errors.New("agent and application versions are required")
	}
	return nil
}

func (gate RecoveryGate) Validate() error {
	switch {
	case gate.ClosurePolicy == "":
		return errors.New("closure policy is required")
	case len(gate.RequiredObservations) == 0:
		return errors.New("required observations are required")
	case len(gate.Thresholds) == 0:
		return errors.New("thresholds are required")
	case len(gate.EvidenceRefs) == 0:
		return errors.New("evidence refs are required")
	}
	return nil
}
