package loop

import (
	"fmt"
	"testing"
	"time"
)

func planWithEvidence(items ...EvidenceItem) Plan {
	return Plan{Meta: map[string]any{"evidence_chain": items}}
}

func subjectItem() EvidenceItem {
	return NewSubjectEvidenceItem("alert-17", "k8s:alert-17", map[string]string{"pod": "order-svc-7d9"})
}

// The load-bearing case: the LLM summarised the chain and left the label
// item out. The resolvers downstream read the pod name from that item, so a
// dropped subject would silently refuse every write action.
func TestStampSubject_RestoresTheSubjectTheLLMDropped(t *testing.T) {
	subject := subjectItem()
	rc := &RootCauseJSON{SchemaVersion: "v1", EvidenceChain: []EvidenceItem{
		{Tool: "query_promql", Value: `[{"value":"0.4"}]`},
		{Tool: "query_logql", Value: "12 entries"},
	}}
	StampSubject(planWithEvidence(EvidenceItem{Tool: "resource_alert"}, subject, EvidenceItem{Tool: "query_promql"}), rc)

	if len(rc.EvidenceChain) != 3 {
		t.Fatalf("chain = %d entries, want the subject restored", len(rc.EvidenceChain))
	}
	found := false
	for _, item := range rc.EvidenceChain {
		if item.Tool == SubjectEvidenceTool {
			found = true
		}
	}
	if !found {
		t.Fatal("the subject must be back in the chain")
	}
}

// If the chain is already at the cap, adding the subject would push it over
// the validator's limit and the contract would be rejected on write.
func TestStampSubject_StaysWithinTheContractCap(t *testing.T) {
	full := make([]EvidenceItem, 0, maxEvidenceChainEntries)
	for i := range maxEvidenceChainEntries {
		full = append(full, EvidenceItem{Tool: "query_promql", Query: fmt.Sprintf("q%d", i)})
	}
	rc := &RootCauseJSON{SchemaVersion: "v1", EvidenceChain: full}
	StampSubject(planWithEvidence(subjectItem()), rc)

	if len(rc.EvidenceChain) != maxEvidenceChainEntries {
		t.Fatalf("chain = %d entries, want it capped at %d", len(rc.EvidenceChain), maxEvidenceChainEntries)
	}
	if err := ValidateRootCauseJSON(minimalValidCause(rc)); err != nil {
		t.Fatalf("a stamped chain must still validate: %v", err)
	}
}

// Stamping must not duplicate a subject the LLM already carried.
func TestStampSubject_DoesNotDuplicateAnExistingSubject(t *testing.T) {
	rc := &RootCauseJSON{SchemaVersion: "v1", EvidenceChain: []EvidenceItem{
		{Tool: "query_promql"},
		{Tool: SubjectEvidenceTool, Value: `{"labels":{"pod":"from-llm"}}`},
	}}
	StampSubject(planWithEvidence(subjectItem()), rc)

	count := 0
	for _, item := range rc.EvidenceChain {
		if item.Tool == SubjectEvidenceTool {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("subject appears %d times, want exactly 1", count)
	}
}

// No toolset subject means the resolvers should keep refusing, so nothing is
// invented here.
func TestStampSubject_AddsNothingWhenTheToolsetRecordedNone(t *testing.T) {
	rc := &RootCauseJSON{SchemaVersion: "v1", EvidenceChain: []EvidenceItem{{Tool: "query_promql"}}}
	StampSubject(planWithEvidence(EvidenceItem{Tool: "query_promql"}), rc)
	if len(rc.EvidenceChain) != 1 {
		t.Fatalf("chain = %d entries, want the single metric untouched", len(rc.EvidenceChain))
	}
}

// minimalValidCause fills in the fields ValidateRootCauseJSON requires, so a
// test can assert on the evidence cap alone.
func minimalValidCause(rc *RootCauseJSON) *RootCauseJSON {
	rc.SchemaVersion = ContractSchemaV1
	rc.RootCauseObject = &RootCauseObject{Kind: "unknown", Summary: "s", Detail: map[string]any{}}
	rc.Confidence = 0.5
	rc.TimeWindow = TimeWindow{Start: nowStub(), End: nowStub().Add(time.Minute)}
	rc.RemediationOptions = []RemediationOption{{Action: "k8s.evict_pod", Target: "k8s:alert-17", Risk: "mutating"}}
	return rc
}

func nowStub() time.Time { return time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC) }
