package pigwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The cache-write loss happened twice on this path: PiG's ai.Usage has a
// CacheWrite field, the record this package decodes it into had no column for
// it, and neither did the wire frame. So a caching provider's turn reported
// only its cache reads. The payload below is written out rather than built
// from Go types, for the reason the rest of this package's fixtures are: the
// shape under test is the one PiG's rpc_events.go emits.

const messageEndCachedJSON = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"cached"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-5","usage":{"input":0,"output":90,"cacheRead":12000,"cacheWrite":45000,"totalTokens":57090,"cost":{"input":0,"output":0.0018,"cacheRead":0.0036,"cacheWrite":0.0675,"total":0.0729}},"stopReason":"stop","timestamp":1759200000000}}`

func TestCacheWriteTokensReachTheDoneFrame(t *testing.T) {
	tr := newTranslator()
	tr.Translate("turn_start", []byte(turnStartJSON))
	tr.Translate("message_end", []byte(messageEndCachedJSON))

	frames := tr.Translate("agent_end", []byte(agentEndJSON))
	if len(frames) != 1 {
		t.Fatalf("produced %d frames, want 1", len(frames))
	}
	usage := frames[0].Done.Usage
	if usage == nil {
		t.Fatal("usage missing: the console must not have to sum assistant frames for a cost")
	}
	if usage.CacheWriteTokens != 45000 {
		t.Errorf("usage.CacheWriteTokens = %d, want 45000 — 45k cache-write tokens are the "+
			"largest single line in this turn and the frame cannot express them", usage.CacheWriteTokens)
	}
	if usage.CacheReadTokens != 12000 {
		t.Errorf("usage.CacheReadTokens = %d, want 12000", usage.CacheReadTokens)
	}
	if usage.OutputTokens != 90 {
		t.Errorf("usage.OutputTokens = %d, want 90", usage.OutputTokens)
	}
}

// The column is omitempty on purpose: a provider that reports no cache
// writes must not make every other provider's frames carry a zero, and the
// console reads the frame by key. Dropping omitempty is a one-character
// change that no other test would notice, so the key's presence is pinned
// here in both directions.
func TestCacheWriteKeyIsOmittedWhenZero(t *testing.T) {
	tr := newTranslator()
	tr.Translate("turn_start", []byte(turnStartJSON))
	tr.Translate("message_end", []byte(messageEndTextJSON)) // cacheWrite: 0
	frames := tr.Translate("agent_end", []byte(agentEndJSON))

	raw, err := json.Marshal(frames[0])
	if err != nil {
		t.Fatalf("marshal done frame: %v", err)
	}
	if strings.Contains(string(raw), "cache_write_tokens") {
		t.Errorf("done frame carries cache_write_tokens for a provider that reported none: %s", raw)
	}

	cached := newTranslator()
	cached.Translate("turn_start", []byte(turnStartJSON))
	cached.Translate("message_end", []byte(messageEndCachedJSON))
	cachedFrames := cached.Translate("agent_end", []byte(agentEndJSON))
	raw, err = json.Marshal(cachedFrames[0])
	if err != nil {
		t.Fatalf("marshal done frame: %v", err)
	}
	if !strings.Contains(string(raw), `"cache_write_tokens":45000`) {
		t.Errorf("done frame does not carry the cache-write count under its wire key: %s", raw)
	}
}
