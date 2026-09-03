package judge

import (
	"context"
	"errors"
	"testing"
)

func TestScore_IsValid(t *testing.T) {
	tests := []struct {
		name  string
		score Score
		want  bool
	}{
		{"valid", Score{Overall: 0.5, Dimensions: map[string]float64{"a": 0.5}}, true},
		{"negative", Score{Overall: -0.1}, false},
		{"over_one", Score{Overall: 1.1}, false},
		{"zero_ok", Score{Overall: 0}, true},
		{"one_ok", Score{Overall: 1}, true},
		{"dim_negative", Score{Overall: 0.5, Dimensions: map[string]float64{"a": -0.1}}, false},
		{"dim_over", Score{Overall: 0.5, Dimensions: map[string]float64{"a": 1.5}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.score.IsValid()
			if (err == nil) != tc.want {
				t.Errorf("IsValid err = %v, wantValid=%v", err, tc.want)
			}
		})
	}
}

func TestMeanScore_BothAgree_NoFlag(t *testing.T) {
	a := &Score{Overall: 0.8, Dimensions: map[string]float64{"x": 0.8}, JudgesUsed: []string{"a"}}
	b := &Score{Overall: 0.82, Dimensions: map[string]float64{"x": 0.82}, JudgesUsed: []string{"b"}}
	merged := MeanScore(a, b)
	if merged.Flagged {
		t.Errorf("diff 0.02 < 0.2 should not flag")
	}
	if merged.Overall != 0.81 {
		t.Errorf("Overall = %f, want 0.81", merged.Overall)
	}
	if len(merged.JudgesUsed) != 2 {
		t.Errorf("JudgesUsed = %v, want 2", merged.JudgesUsed)
	}
}

func TestMeanScore_LargeDiff_Flag(t *testing.T) {
	a := &Score{Overall: 0.9, JudgesUsed: []string{"a"}}
	b := &Score{Overall: 0.5, JudgesUsed: []string{"b"}}
	merged := MeanScore(a, b)
	if !merged.Flagged {
		t.Errorf("diff 0.4 > 0.2 should flag")
	}
	if merged.FlagReason == "" {
		t.Errorf("FlagReason should be set on flag")
	}
}

func TestMeanScore_NilHandling(t *testing.T) {
	if MeanScore(nil, nil) != nil {
		t.Errorf("both nil should return nil")
	}
	a := &Score{Overall: 0.5, JudgesUsed: []string{"a"}}
	if MeanScore(a, nil) != a {
		t.Errorf("one nil should return the other")
	}
}

func TestMeanScore_MergesDimensions(t *testing.T) {
	a := &Score{Overall: 0.5, Dimensions: map[string]float64{"x": 0.4, "y": 0.6}}
	b := &Score{Overall: 0.5, Dimensions: map[string]float64{"x": 0.6, "z": 0.8}}
	merged := MeanScore(a, b)
	if merged.Dimensions["x"] != 0.5 {
		t.Errorf("x = %f, want 0.5 (mean of 0.4 and 0.6)", merged.Dimensions["x"])
	}
	if merged.Dimensions["y"] != 0.6 {
		t.Errorf("y = %f, want 0.6 (only in a)", merged.Dimensions["y"])
	}
	if merged.Dimensions["z"] != 0.8 {
		t.Errorf("z = %f, want 0.8 (only in b)", merged.Dimensions["z"])
	}
}

func TestCache_PutGet(t *testing.T) {
	c := NewCache()
	s := &Score{Overall: 0.7}
	c.Put("case1", "hash1", s)
	got, ok := c.Get("case1", "hash1")
	if !ok {
		t.Errorf("expected cache hit")
	}
	if got != s {
		t.Errorf("got different *Score pointer")
	}
}

func TestCache_Miss(t *testing.T) {
	c := NewCache()
	_, ok := c.Get("never-put", "x")
	if ok {
		t.Errorf("expected cache miss")
	}
}

func TestCache_KeyIsolation(t *testing.T) {
	c := NewCache()
	c.Put("case1", "h1", &Score{Overall: 0.7})
	c.Put("case1", "h2", &Score{Overall: 0.8})
	c.Put("case2", "h1", &Score{Overall: 0.9})
	if c.Size() != 3 {
		t.Errorf("Size = %d, want 3", c.Size())
	}
	if s, _ := c.Get("case1", "h1"); s.Overall != 0.7 {
		t.Errorf("isolation broken: case1/h1 = %f", s.Overall)
	}
}

func TestComputeResponseHash_Deterministic(t *testing.T) {
	r := &AgentResponse{
		ToolCalls:    []ToolCall{{Name: "pg.lock_waits", Args: []byte(`{}`)}},
		RootCause:    []string{"pg.lock_waits"},
		Remediations: []string{"pg.kill_session"},
	}
	h1 := ComputeResponseHash(r)
	h2 := ComputeResponseHash(r)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("hash length = %d, want 16 (truncated sha256)", len(h1))
	}
}

func TestComputeResponseHash_DifferentInputs(t *testing.T) {
	r1 := &AgentResponse{RootCause: []string{"a"}}
	r2 := &AgentResponse{RootCause: []string{"b"}}
	if ComputeResponseHash(r1) == ComputeResponseHash(r2) {
		t.Errorf("different inputs should produce different hashes")
	}
}

func TestConsistencyThreshold(t *testing.T) {
	if ConsistencyThreshold != 0.2 {
		t.Errorf("ConsistencyThreshold = %f, want 0.2 (per ADR-001)", ConsistencyThreshold)
	}
}

// Ensure Judge interface is implemented by both implementations
var (
	_ Judge = (*LLMJudge)(nil)
	_ Judge = (*HeuristicJudge)(nil)
	_       = context.Background
	_       = errors.New
)
