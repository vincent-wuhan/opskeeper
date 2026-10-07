package pigagent

import (
	"testing"

	"github.com/MichaelKinsy/PiG/agent"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// Cache-write tokens are billable prompt work and the providers that report
// them (Anthropic, DeepSeek) report the largest ones. wire.UsageFrame had no
// column for them, so the live counter stopped at cache reads while the
// stored transcript — which does carry them — reported a larger number for
// the same turn. The two disagreed for as long as both existed.

func TestUsageFrameCarriesCacheWriteTokens(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()
	m.Map(agent.MessageEndEvent{Message: assistantMessage("", 1)})
	m.SetUsage(ports.TranscriptUsage{
		InputTokens:      1200,
		OutputTokens:     340,
		CacheReadTokens:  800,
		CacheWriteTokens: 4500,
		CostUSD:          0.0123,
	}, "test-model")

	frames := m.Map(agent.AgentEndEvent{})
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	usage := frames[0].Done.Usage
	if usage == nil {
		t.Fatal("usage missing")
	}
	if usage.CacheWriteTokens != 4500 {
		t.Errorf("usage.CacheWriteTokens = %d, want 4500 — a caching provider's turn "+
			"reads as cheaper while it runs than it does after a reload", usage.CacheWriteTokens)
	}
	// The pre-existing columns must be untouched by the addition.
	if usage.InputTokens != 1200 || usage.OutputTokens != 340 || usage.CacheReadTokens != 800 {
		t.Errorf("usage = %+v, want the other columns unchanged", *usage)
	}
}

func TestUsageFrameOmitsZeroCacheWrite(t *testing.T) {
	m := newTestMapper()
	m.TurnStarted()
	m.Map(agent.MessageEndEvent{Message: assistantMessage("", 1)})
	m.SetUsage(ports.TranscriptUsage{InputTokens: 10, OutputTokens: 5}, "test-model")

	frames := m.Map(agent.AgentEndEvent{})
	if frames[0].Done.Usage.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 for a provider that reports none",
			frames[0].Done.Usage.CacheWriteTokens)
	}
	if frames[0].Type != wire.StreamDone {
		t.Errorf("type = %q, want done", frames[0].Type)
	}
}
