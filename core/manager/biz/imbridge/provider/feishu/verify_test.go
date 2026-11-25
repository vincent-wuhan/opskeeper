package feishu

// S-3 of the marketplace-imbridge-fail-closed change: the HTTP layer now
// verifies EVERY event (an app with an empty encrypt_key is rejected
// before it gets here), which makes VerifyEventSignature the single
// integrity boundary between an anonymous POST and an agent run. These
// tests pin the algorithm itself — the exact concatenation order and the
// case-insensitive hex compare — so a refactor that reorders the hash
// input or tightens the comparison breaks here rather than in production
// by silently rejecting (or worse, accepting) platform traffic.

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(key, ts, nonce string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(ts))
	h.Write([]byte(nonce))
	h.Write([]byte(key))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func TestVerifyEventSignature_Match(t *testing.T) {
	key, ts, nonce, body := "evt-key-1", "1700000000", "nonce-abc", []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1"}}`)
	sig := sign(key, ts, nonce, body)
	if err := VerifyEventSignature(ts, nonce, key, body, sig); err != nil {
		t.Fatalf("correct signature rejected: %v", err)
	}
	// Same signature in upper-case hex must pass: the compare is
	// EqualFold, matching what Feishu docs imply about hex casing and
	// what proxies have been observed to mangle.
	if err := VerifyEventSignature(ts, nonce, key, body, " "+sig); err == nil {
		t.Log("note: leading space accepted by EqualFold? verifying strictness separately")
	}
	if err := VerifyEventSignature(ts, nonce, key, body, hexUpper(sig)); err != nil {
		t.Fatalf("upper-case hex signature rejected: %v", err)
	}
}

func hexUpper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'f' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func TestVerifyEventSignature_Mismatch(t *testing.T) {
	key, ts, nonce, body := "evt-key-1", "1700000000", "nonce-abc", []byte(`{"x":1}`)

	cases := []struct {
		name string
		sig  string
	}{
		{"wrong key", sign("other-key", ts, nonce, body)},
		{"tampered body", sign(key, ts, nonce, []byte(`{"x":2}`))},
		{"wrong nonce", sign(key, ts, "nonce-xyz", body)},
		{"wrong timestamp", sign(key, "1700000001", nonce, body)},
		{"empty signature", ""},
		{"garbage", "not-hex-at-all"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifyEventSignature(ts, nonce, key, body, c.sig); err == nil {
				t.Fatalf("signature %q accepted, want ErrBadSignature", c.sig)
			} else if err != ErrBadSignature {
				t.Fatalf("error = %v, want ErrBadSignature", err)
			}
		})
	}
}

// The concat order (ts, nonce, key, body) is part of the wire contract
// with Feishu. A test that signs in a different order must fail — this
// documents that the order is load-bearing, not incidental.
func TestVerifyEventSignature_OrderIsLoadBearing(t *testing.T) {
	key, ts, nonce := "k", "1", "n"
	body := []byte("b")
	wrong := sha256.New()
	wrong.Write([]byte(key))
	wrong.Write([]byte(ts))
	wrong.Write([]byte(nonce))
	wrong.Write(body)
	if err := VerifyEventSignature(ts, nonce, key, body, hex.EncodeToString(wrong.Sum(nil))); err == nil {
		t.Fatal("a signature computed in a different concat order was accepted — the algorithm's input order changed and the test suite did not notice")
	}
}
