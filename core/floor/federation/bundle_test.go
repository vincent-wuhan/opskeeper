package federation

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Bundle tests.
//
// The properties here are all about a message whose fields are claims about a
// tree that is not in the message. Every one of them is a way a reader could be
// told something true about one thing and false about another.

func validBundle(t *testing.T, signer *pluginmanifest.Signer) (Bundle, pluginmanifest.Envelope) {
	t.Helper()
	_, env := newSignedTree(t, signer, "opskeeper-sre-readonly", "1.0.0")
	return bundleFor(t, ClusterID("prod-cn-north"), 1, env), env
}

func TestAWellFormedBundleValidates(t *testing.T) {
	signer := newSigner(t, "release-2026")
	b, env := validBundle(t, signer)

	got, err := b.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.TreeDigest != env.TreeDigest {
		t.Errorf("returned envelope digest = %q, want %q", got.TreeDigest, env.TreeDigest)
	}
}

// TestValidateCrossChecksTheEnvelope is the one that matters most. The bundle
// and the envelope are two copies of the same claim; if they are allowed to
// disagree, then a signature over package A can arrive attached to a receipt
// that says package B, and every downstream reader of the receipt is misled
// while the cryptography is perfectly happy.
func TestValidateCrossChecksTheEnvelope(t *testing.T) {
	signer := newSigner(t, "release-2026")
	base, _ := validBundle(t, signer)

	t.Run("name", func(t *testing.T) {
		b := base
		b.PackageName = "opskeeper-sre-repair"
		if _, err := b.Validate(); !errors.Is(err, ErrMalformedBundle) {
			t.Errorf("Validate = %v, want ErrMalformedBundle", err)
		} else if !strings.Contains(err.Error(), "envelope signs") {
			t.Errorf("refusal does not explain the disagreement: %v", err)
		}
	})

	t.Run("version", func(t *testing.T) {
		b := base
		b.PackageVersion = "2.0.0"
		if _, err := b.Validate(); !errors.Is(err, ErrMalformedBundle) {
			t.Errorf("Validate = %v, want ErrMalformedBundle", err)
		}
	})
}

func TestValidateRefusesTheMalformedShapes(t *testing.T) {
	signer := newSigner(t, "release-2026")
	base, _ := validBundle(t, signer)

	cases := map[string]func(b *Bundle){
		"no cluster":      func(b *Bundle) { b.ClusterID = "" },
		"bad cluster":     func(b *Bundle) { b.ClusterID = "Prod/North" },
		"version zero":    func(b *Bundle) { b.Version = 0 },
		"no name":         func(b *Bundle) { b.PackageName = "" },
		"no version":      func(b *Bundle) { b.PackageVersion = "" },
		"no envelope":     func(b *Bundle) { b.Envelope = "" },
		"not base64":      func(b *Bundle) { b.Envelope = "!!! not base64 !!!" },
		"not an envelope": func(b *Bundle) { b.Envelope = base64.StdEncoding.EncodeToString([]byte(`{"hello":"world"}`)) },
	}

	for name, mutate := range cases {
		b := base
		mutate(&b)
		if _, err := b.Validate(); !errors.Is(err, ErrMalformedBundle) {
			t.Errorf("[%s] Validate = %v, want ErrMalformedBundle", name, err)
		}
	}
}

// TestValidateRefusesAnUnreviewedAlgorithm: an envelope naming an algorithm
// this build has never reviewed is refused rather than guessed at. Guessing is
// how a signature scheme gets a second, worse implementation by accident.
func TestValidateRefusesAnUnreviewedAlgorithm(t *testing.T) {
	signer := newSigner(t, "release-2026")
	base, env := validBundle(t, signer)

	env.Algorithm = "rot13"
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b := base
	b.Envelope = base64.StdEncoding.EncodeToString(raw)

	if _, err := b.Validate(); !errors.Is(err, ErrMalformedBundle) {
		t.Fatalf("Validate = %v, want ErrMalformedBundle", err)
	} else if !strings.Contains(err.Error(), pluginmanifest.SignAlgorithm) {
		t.Errorf("refusal does not name the accepted algorithm: %v", err)
	}
}

// TestBundleStringOmitsTheSignature: the envelope is a couple of hundred bytes
// of base64, and a log line is for the person reading it at 3am.
func TestBundleStringOmitsTheSignature(t *testing.T) {
	signer := newSigner(t, "release-2026")
	b, _ := validBundle(t, signer)

	s := b.String()
	for _, want := range []string{"prod-cn-north", "version=1", "opskeeper-sre-readonly@1.0.0", "policy rollout"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	if strings.Contains(s, b.Envelope) {
		t.Errorf("String() leaked the signature envelope into a log line")
	}
}

func TestMinBundleVersionIsNotZero(t *testing.T) {
	// Version 0 means "never published" on both sides of the wire, which is
	// why the first publishable version is 1 and not 0.
	if MinBundleVersion != 1 {
		t.Errorf("MinBundleVersion = %d, want 1", MinBundleVersion)
	}
}
