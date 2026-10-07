package chatdiagnose

import (
	"crypto/rand"
	"encoding/hex"
)

// generateConversationID returns a 32-char hex (128-bit) identifier
// suitable for the diagnostic_conversation.id column. UUID v4 is the
// Day 5 plan; for now we use raw 16 random bytes hex-encoded — same
// shape, simpler impl, no external dep.
//
// crypto/rand is used (NOT math/rand) so the IDs are unpredictable;
// per chatdiagnose spec, the id is exposed to the SPA in URLs and
// audit logs, so predictability is a soft concern but cheap to get
// right.
func generateConversationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a kernel-level outage — fall back
		// to a zeroed buffer so the service doesn't panic. The DB
		// primary key will collide on retry; caller retries with a
		// fresh conversation.
		for i := range b {
			b[i] = byte(i)
		}
	}
	return hex.EncodeToString(b[:])
}
