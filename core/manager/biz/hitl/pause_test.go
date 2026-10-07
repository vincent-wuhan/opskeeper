package hitl

import (
	"testing"
	"time"
)

func TestResumeToken_SerializeDeserialize(t *testing.T) {
	original := &ResumeToken{
		ProposalID: 42,
		LLMMessages: []LLMMessage{
			{Role: "user", Content: "restart prod-nginx-3"},
			{Role: "assistant", Content: "checking..."},
		},
		ToolCallStack: []ToolCall{
			{Tool: "host_load", Args: map[string]interface{}{"host": "edge-1"}},
		},
		DBRefs: []DBRowRef{
			{Table: "flow_runs", ID: "run-1", Version: 1},
		},
		DBRowVersion: 1,
		CreatedAt:    time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}

	data, err := original.Serialize()
	if err != nil {
		t.Fatalf("Serialize error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty serialized data")
	}

	got, err := DeserializeResumeToken(data)
	if err != nil {
		t.Fatalf("Deserialize error: %v", err)
	}

	if got.ProposalID != original.ProposalID {
		t.Errorf("ProposalID = %d, want %d", got.ProposalID, original.ProposalID)
	}
	if len(got.LLMMessages) != 2 {
		t.Errorf("LLMMessages count = %d, want 2", len(got.LLMMessages))
	}
	if got.LLMMessages[0].Content != "restart prod-nginx-3" {
		t.Errorf("LLMMessages[0].Content = %q, want %q", got.LLMMessages[0].Content, "restart prod-nginx-3")
	}
	if got.DBRowVersion != 1 {
		t.Errorf("DBRowVersion = %d, want 1", got.DBRowVersion)
	}
}

func TestResumeToken_ValidateVersion(t *testing.T) {
	tok := &ResumeToken{DBRowVersion: 5}

	if err := tok.ValidateVersion(5); err != nil {
		t.Errorf("ValidateVersion(5) error = %v, want nil", err)
	}

	if err := tok.ValidateVersion(6); err == nil {
		t.Error("ValidateVersion(6) should error on mismatch")
	}
}

func TestResumeToken_NilSafety(t *testing.T) {
	var tok *ResumeToken
	if _, err := tok.Serialize(); err == nil {
		t.Error("nil Serialize should error")
	}
	if err := tok.ValidateVersion(0); err == nil {
		t.Error("nil ValidateVersion should error")
	}
}

func TestDeserializeResumeToken_Empty(t *testing.T) {
	if _, err := DeserializeResumeToken(nil); err == nil {
		t.Error("DeserializeResumeToken(nil) should error")
	}
	if _, err := DeserializeResumeToken([]byte{}); err == nil {
		t.Error("DeserializeResumeToken([]byte{}) should error")
	}
}

func TestDeserializeResumeToken_Invalid(t *testing.T) {
	_, err := DeserializeResumeToken([]byte("not json"))
	if err == nil {
		t.Error("DeserializeResumeToken(invalid) should error")
	}
}
