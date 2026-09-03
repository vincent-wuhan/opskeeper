package agentteams

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
)

type recordIncidentEventReq struct {
	IncidentID        string     `json:"incident_id"`
	OccurredAt        *time.Time `json:"occurred_at,omitempty"`
	ActionFingerprint string     `json:"action_fingerprint,omitempty"`
	EvidenceRef       string     `json:"evidence_ref"`
	RecoverySignal    bool       `json:"recovery_signal,omitempty"`
}

type incidentEventSpec struct {
	Phase     string
	EventType string
	Previous  string
}

var incidentEventByRole = map[string]incidentEventSpec{
	"alerter":      {Phase: "detect", EventType: incidentcontrol.EventAlertReceived},
	"investigator": {Phase: "diagnose", EventType: incidentcontrol.EventRootCause, Previous: incidentcontrol.EventAlertReceived},
	"reviewer":     {Phase: "approve", EventType: incidentcontrol.EventApproved, Previous: incidentcontrol.EventRootCause},
	"repairer":     {Phase: "act", EventType: incidentcontrol.EventAction, Previous: incidentcontrol.EventApproved},
	"verifier":     {Phase: "verify", EventType: incidentcontrol.EventRecovery, Previous: incidentcontrol.EventAction},
	"reporter":     {Phase: "report", EventType: incidentcontrol.EventClosed, Previous: incidentcontrol.EventRecovery},
}

func (h *Handler) recordIncidentEvent(w http.ResponseWriter, r *http.Request) {
	identity, ok := mcpauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "no resolved identity")
		return
	}
	spec, ok := incidentEventByRole[identity.Role]
	if !ok || !auth.AgentTeamsRoleAllows(identity.Role, "incident.record") {
		writeJSONError(w, http.StatusForbidden, "role not allowed to record this incident event")
		return
	}
	if identity.TenantID == "" {
		writeJSONError(w, http.StatusForbidden, "tenant could not be derived")
		return
	}
	trace, hasTrace := mcpauth.TraceFromContext(r.Context())
	if !hasTrace || !trace.HasTrace() {
		writeJSONError(w, http.StatusBadRequest, "trace context is required")
		return
	}
	if h.incident == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "incident recorder unavailable")
		return
	}

	var req recordIncidentEventReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.IncidentID = strings.TrimSpace(req.IncidentID)
	req.EvidenceRef = strings.TrimSpace(req.EvidenceRef)
	req.ActionFingerprint = strings.TrimSpace(req.ActionFingerprint)
	if req.IncidentID == "" || len(req.IncidentID) > 128 {
		writeJSONError(w, http.StatusBadRequest, "incident_id is required and must be at most 128 bytes")
		return
	}
	if req.EvidenceRef == "" || len(req.EvidenceRef) > 512 {
		writeJSONError(w, http.StatusBadRequest, "evidence_ref is required and must be at most 512 bytes")
		return
	}
	if spec.EventType == incidentcontrol.EventAction && (req.ActionFingerprint == "" || len(req.ActionFingerprint) > 256) {
		writeJSONError(w, http.StatusBadRequest, "action_fingerprint is required for action.executed and must be at most 256 bytes")
		return
	}
	if spec.EventType == incidentcontrol.EventRecovery && !req.RecoverySignal {
		writeJSONError(w, http.StatusBadRequest, "recovery_signal=true is required for recovery_signal.observed")
		return
	}
	if spec.EventType != incidentcontrol.EventRecovery && req.RecoverySignal {
		writeJSONError(w, http.StatusBadRequest, "recovery_signal=true is only allowed for recovery_signal.observed")
		return
	}

	occurredAt := time.Now().UTC()
	if req.OccurredAt != nil {
		occurredAt = req.OccurredAt.UTC()
	}
	existing, err := h.incident.ListIncident(r.Context(), identity.TenantID, req.IncidentID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load incident timeline failed")
		return
	}
	if err := validateIncidentProgress(spec, existing, occurredAt); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}

	actor := identity.ConsumerName
	if actor == "" {
		actor = auth.AgentTeamsWorkerForRole(identity.Role)
	}
	event := incidentcontrol.Event{
		ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(
			"opskeeper:incident-event:"+identity.TenantID+"/"+req.IncidentID+"/"+spec.EventType+"/"+req.EvidenceRef,
		)).String(),
		TenantID:          identity.TenantID,
		IncidentID:        req.IncidentID,
		OccurredAt:        occurredAt,
		Phase:             spec.Phase,
		EventType:         spec.EventType,
		ActorType:         "agent",
		Actor:             actor,
		Status:            "completed",
		ActionFingerprint: req.ActionFingerprint,
		EvidenceRef:       req.EvidenceRef,
		TraceID:           trace.TraceID,
		RecoverySignal:    req.RecoverySignal,
	}
	if err := h.incident.Append(r.Context(), event); err != nil {
		if errors.Is(err, incidentcontrol.ErrDuplicateEvent) {
			writeJSONError(w, http.StatusConflict, "incident event already exists")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "append incident event failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(event)
}

func validateIncidentProgress(spec incidentEventSpec, existing []incidentcontrol.Event, occurredAt time.Time) error {
	if spec.Previous == "" {
		return nil
	}
	for index := range existing {
		previous := existing[index]
		if previous.EventType != spec.Previous || !previous.OccurredAt.Before(occurredAt) {
			continue
		}
		if spec.EventType == incidentcontrol.EventClosed && !previous.RecoverySignal {
			break
		}
		return nil
	}
	return errors.New("incident event prerequisite is missing or out of order")
}
