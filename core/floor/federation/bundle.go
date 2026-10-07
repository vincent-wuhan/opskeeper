package federation

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// MinBundleVersion is the first version a root may publish.
//
// Version 0 exists in the wire so that "unset" and "the first decision" are
// different values on both sides of the wire. A root that has never published
// anything reports 0, and a child that has never accepted anything reports 0,
// and neither has to carry a second boolean to say so.
const MinBundleVersion uint64 = 1

// Bundle is one published policy decision, addressed to one cluster.
//
// It names a package and carries the detached signature over that package's
// tree. It does not carry the bytes: a policy tree is tens of kilobytes and the
// tunnel is already carrying telemetry, so the tree travels the same
// out-of-band path a plugin package already takes and this is the receipt.
//
// Which means every field here is a claim about a tree that is not in the
// message. That is why Validate cross-checks the bundle's own PackageName and
// PackageVersion against the signature envelope: the envelope is the part that
// is authenticated, so the fields a reader would otherwise trust have to be
// derived from it rather than believed alongside it.
type Bundle struct {
	// ClusterID scopes the bundle. A child refuses a bundle addressed to a
	// different cluster rather than applying it and reporting a
	// diagnostic — a misrouted policy is an operator error, and applying
	// it is how one cluster's policy ends up on another.
	ClusterID ClusterID `json:"cluster_id"`
	// Version is the sequence number of the decision. It only ever goes
	// up; see the package doc for why a rollback is version N+1 carrying
	// old content rather than a message naming an old version.
	Version uint64 `json:"version"`
	// PackageName and PackageVersion name the tree the signature covers.
	// Both are re-checked against the envelope and, after verification,
	// against the manifest on disk.
	PackageName    string `json:"package_name"`
	PackageVersion string `json:"package_version"`
	// Envelope is pluginmanifest.Envelope.Transport() — the same envelope
	// the sidecar in the tree carries, read only so that a mismatch is an
	// answer an operator can act on rather than a bare signature failure.
	Envelope string `json:"envelope"`
	// Reason is why the root published this version. It is free text and
	// it is not signed over: it is for the human reading the child's log
	// at 3am, and making it authenticated would mean a signature covers a
	// field whose only reader is a person.
	Reason string `json:"reason,omitempty"`
	// IssuedAt is when the root published it. Advisory, and deliberately
	// not signed: a clock is not a security property.
	IssuedAt time.Time `json:"issued_at"`
}

// ErrMalformedBundle is returned for a bundle whose own fields disagree.
//
// It is a distinct error from a policy refusal so a reader can tell "this
// message is broken" from "I understood this message and I am declining it".
var ErrMalformedBundle = errors.New("federation: malformed bundle")

// Validate checks that the bundle is a well-formed claim, and returns the
// envelope it carries.
//
// It does not verify the signature — that needs the tree, and the tree is not
// here. What it does is refuse the shapes that would make a later verification
// ambiguous.
func (b Bundle) Validate() (pluginmanifest.Envelope, error) {
	var env pluginmanifest.Envelope

	if !b.ClusterID.Valid() {
		return env, fmt.Errorf("%w: cluster_id %q is not a valid identity", ErrMalformedBundle, b.ClusterID)
	}
	if b.Version < MinBundleVersion {
		return env, fmt.Errorf("%w: version %d is below the first publishable version %d",
			ErrMalformedBundle, b.Version, MinBundleVersion)
	}
	if strings.TrimSpace(b.PackageName) == "" {
		return env, fmt.Errorf("%w: package_name is required", ErrMalformedBundle)
	}
	if strings.TrimSpace(b.PackageVersion) == "" {
		return env, fmt.Errorf("%w: package_version is required", ErrMalformedBundle)
	}
	if strings.TrimSpace(b.Envelope) == "" {
		return env, fmt.Errorf("%w: envelope is required; an unsigned bundle cannot be applied", ErrMalformedBundle)
	}

	raw, err := base64.StdEncoding.DecodeString(b.Envelope)
	if err != nil {
		return env, fmt.Errorf("%w: envelope is not base64: %v", ErrMalformedBundle, err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("%w: envelope is not a signature envelope: %v", ErrMalformedBundle, err)
	}
	if strings.TrimSpace(env.KeyID) == "" {
		return env, fmt.Errorf("%w: envelope names no key", ErrMalformedBundle)
	}
	if env.Algorithm != pluginmanifest.SignAlgorithm {
		return env, fmt.Errorf("%w: envelope claims algorithm %q, this build only accepts %q",
			ErrMalformedBundle, env.Algorithm, pluginmanifest.SignAlgorithm)
	}

	// The cross-check. A bundle that says "package v2" while its envelope
	// signs "package v1" is either a publisher bug or a rewrite in flight.
	// Either way the authenticated half wins and the message is refused,
	// because the alternative is a policy tree whose manifest says one
	// thing and whose receipt says another.
	if env.Name != b.PackageName {
		return env, fmt.Errorf("%w: bundle names package %q but its envelope signs %q",
			ErrMalformedBundle, b.PackageName, env.Name)
	}
	if env.Version != b.PackageVersion {
		return env, fmt.Errorf("%w: bundle names package version %q but its envelope signs %q",
			ErrMalformedBundle, b.PackageVersion, env.Version)
	}

	return env, nil
}

// String renders the bundle for a log line, without the signature blob.
//
// The envelope is base64 and a couple of hundred bytes of it in a log line
// turns a readable message into a wall of characters, and the part a human
// needs — which cluster, which version, which package — is the rest.
func (b Bundle) String() string {
	return fmt.Sprintf("cluster=%s version=%d package=%s@%s reason=%q",
		b.ClusterID, b.Version, b.PackageName, b.PackageVersion, b.Reason)
}
