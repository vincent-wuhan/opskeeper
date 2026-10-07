package edge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// The plan's security suite names this case directly: a node's token must not
// carry another node's inference. The gateway above it is where that promise
// is cashed out, but the promise is made here, and here is the only place in
// the repository that can break it in a way the gateway cannot see.
//
// Two nodes are enrolled, and each holds a pair: an access key that names it
// and a secret key that proves it. The access key is not a secret — it is
// written to the manager's own log line on creation and travels in the
// Authorization header of every request a node makes — so the entire weight
// of the scheme rests on the secret half never being satisfied by anything
// but itself. That is what these tests hold: A's access key plus B's secret
// is not a near miss, it is a forgery, and it is refused in both orders.
//
// The cache is the part that makes this worth a second test. Verifying an
// argon2id hash costs 64 MiB of memory per call, so successful pairs are
// cached for 60 seconds, and a cache that keyed on the access key alone would
// turn that access key into a bearer token: the moment A authenticated once,
// anyone holding A's access key — which is the half that is logged — would be
// answered with A's identity and A's budget, for the length of the TTL. The
// key is the digest of both halves for exactly that reason, and a test is the
// only thing that keeps it that way through the next optimisation pass.

func enroll(t *testing.T, uc *Usecase, ctx context.Context, name string) *CreateResult {
	t.Helper()
	res, err := uc.Create(ctx, name, nil)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	if res.Edge == nil || res.Edge.ID == 0 {
		t.Fatalf("%s was enrolled without an id", name)
	}
	return res
}

func TestAMixedPairIsRefusedInBothOrders(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	alpha := enroll(t, uc, ctx, "cross-alpha")
	beta := enroll(t, uc, ctx, "cross-beta")
	auth := NewAccessKeyAuthenticator(repo, nil)

	for _, attempt := range []struct {
		name              string
		accessKey, secret string
	}{
		{"alpha's access key with beta's secret", alpha.AccessKey, beta.SecretKey},
		{"beta's access key with alpha's secret", beta.AccessKey, alpha.SecretKey},
	} {
		session, err := auth.Authenticate(ctx, attempt.accessKey, attempt.secret)
		if !errors.Is(err, errs.ErrUnauthorized) {
			t.Errorf("%s: err = %v, want ErrUnauthorized", attempt.name, err)
		}
		if session.EdgeID != 0 {
			t.Errorf("%s: resolved to edge %d; a refused credential must resolve to nobody, "+
				"because a non-zero id here is the id the caller would be trusted as",
				attempt.name, session.EdgeID)
		}
	}

	// Refusing a forgery is only half of it: neither node may have been
	// disturbed by the attempt, and each must still answer as itself.
	if session, err := auth.Authenticate(ctx, alpha.AccessKey, alpha.SecretKey); err != nil {
		t.Errorf("alpha after the forgeries: %v", err)
	} else if session.EdgeID != alpha.Edge.ID {
		t.Errorf("alpha resolved to %d, want %d", session.EdgeID, alpha.Edge.ID)
	}
	if session, err := auth.Authenticate(ctx, beta.AccessKey, beta.SecretKey); err != nil {
		t.Errorf("beta after the forgeries: %v", err)
	} else if session.EdgeID != beta.Edge.ID {
		t.Errorf("beta resolved to %d, want %d", session.EdgeID, beta.Edge.ID)
	}
}

// The cached path is a different code path with the same promise, and it is
// the one that can fail open. Alpha authenticates for real, which warms the
// entry; every later call for that access key is answered from memory and
// never reaches argon2id again. If the cache key carried only the access key,
// the mixed pair would now be admitted — and it would be admitted as alpha,
// so the forgery would not look like a forgery anywhere: the gateway would
// serve it, charge it to alpha, and log alpha asking a question it never asked.
func TestAWarmedCacheDoesNotTurnAnAccessKeyIntoABearerToken(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	alpha := enroll(t, uc, ctx, "cache-alpha")
	beta := enroll(t, uc, ctx, "cache-beta")
	auth := NewAccessKeyAuthenticator(repo, nil)

	warm, err := auth.Authenticate(ctx, alpha.AccessKey, alpha.SecretKey)
	if err != nil {
		t.Fatalf("alpha's own credential: %v", err)
	}
	if warm.EdgeID != alpha.Edge.ID {
		t.Fatalf("alpha resolved to %d, want %d", warm.EdgeID, alpha.Edge.ID)
	}
	// The status flip runs in a goroutine; give it a moment so the warmed
	// state is fully settled before the forgery is attempted.
	time.Sleep(50 * time.Millisecond)

	for _, attempt := range []struct {
		name              string
		accessKey, secret string
	}{
		{"alpha's key with beta's secret", alpha.AccessKey, beta.SecretKey},
		{"beta's key with alpha's secret", beta.AccessKey, alpha.SecretKey},
		{"alpha's key with no secret at all", alpha.AccessKey, ""},
		{"alpha's key with alpha's own secret truncated", alpha.AccessKey, alpha.SecretKey[:len(alpha.SecretKey)-1]},
		{"alpha's key with alpha's own secret extended", alpha.AccessKey, alpha.SecretKey + "x"},
	} {
		session, err := auth.Authenticate(ctx, attempt.accessKey, attempt.secret)
		if !errors.Is(err, errs.ErrUnauthorized) {
			t.Errorf("%s: err = %v, want ErrUnauthorized", attempt.name, err)
		}
		if session.EdgeID != 0 {
			t.Errorf("%s: resolved to edge %d while the cache was warm", attempt.name, session.EdgeID)
		}
	}

	// The warm entry survived the attempts: alpha is still alpha, and beta is
	// still reachable on its own key.
	if session, err := auth.Authenticate(ctx, alpha.AccessKey, alpha.SecretKey); err != nil {
		t.Errorf("alpha from the warm cache: %v", err)
	} else if session.EdgeID != alpha.Edge.ID {
		t.Errorf("alpha from the warm cache resolved to %d, want %d", session.EdgeID, alpha.Edge.ID)
	}
	if session, err := auth.Authenticate(ctx, beta.AccessKey, beta.SecretKey); err != nil {
		t.Errorf("beta after the forgeries: %v", err)
	} else if session.EdgeID != beta.Edge.ID {
		t.Errorf("beta resolved to %d, want %d", session.EdgeID, beta.Edge.ID)
	}
}

// Rotating a secret is the operator's way of cutting a node off, and the cache
// is what decides whether that works on the same schedule everywhere else in
// the system. The access key is untouched by a rotation, so the only thing
// standing between a rotated node and its old identity is the secret half —
// which means the documented 60 second window has to hold for the rotated
// node, or "revoke this node" means something different on the data plane
// than it means in the console.
//
// The window is a real cost: a node keeps answering for up to authCacheTTL
// after its secret is rotated. That cost is only acceptable if it is bounded
// and known, so it is asserted rather than left as a comment.
func TestARotatedSecretStopsWorkingAtTheCacheTTLAndNotBefore(t *testing.T) {
	if authCacheTTL != 60*time.Second {
		t.Fatalf("authCacheTTL = %s, want 60s: the documented revocation window and the code "+
			"have to be the same number, because this is the window in which a revoked node "+
			"still reaches the cluster's model budget", authCacheTTL)
	}

	cache := newAuthCache()
	cache.store("ak", "sk", 7)

	// A stored entry that has not expired is served from memory, which is the
	// window itself. Nothing here should consult a database.
	if id, ok := cache.lookup("ak", "sk"); !ok || id != 7 {
		t.Errorf("lookup of the stored pair = (%d, %v), want (7, true)", id, ok)
	}
	// The secret half is inside the key, so a different secret is a different
	// entry rather than a near miss on the same one.
	if id, ok := cache.lookup("ak", "other"); ok {
		t.Errorf("lookup with a different secret hit edge %d; the cache key has to be the digest "+
			"of the whole pair, or the entry for a real credential is a bearer token for the "+
			"access key alone", id)
	}
	if id, ok := cache.lookup("other", "sk"); ok {
		t.Errorf("lookup with a different access key hit edge %d", id)
	}

	// And an entry past its expiry is not served at all.
	expired := newAuthCache()
	expired.entries[expired.key("ak", "sk")] = authCacheEntry{
		edgeID:    7,
		expiresAt: time.Now().Add(-time.Millisecond),
	}
	if id, ok := expired.lookup("ak", "sk"); ok {
		t.Errorf("an expired entry was served as edge %d", id)
	}
}
