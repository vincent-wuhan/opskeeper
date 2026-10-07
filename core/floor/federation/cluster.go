package federation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxClusterIDLen bounds a cluster identity.
//
// The limit is not arbitrary: the id reaches a console URL, a log line, an
// audit row and a staging directory name, and those four disagree about what
// is safe. Sixty-four is the longest value that is simultaneously a DNS label,
// a filename component and a URL path segment on every platform OpsKeeper
// ships to.
const MaxClusterIDLen = 64

// clusterIDPattern is the whole grammar.
//
// It is DNS-label shaped on purpose. A cluster id is written by an operator
// once and then read by humans in a dropdown, so it has to survive being put
// in a hostname, a path and a log field without escaping any of them. The
// alternative — allowing dots and underscores because they are convenient —
// buys a friendlier name and pays for it with an id that cannot be used as a
// Kubernetes name, a directory name, or an unescaped URL path segment.
var clusterIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ErrUnknownCluster is returned by operations that name a cluster the registry
// has never heard of.
//
// It is distinct from a policy refusal on purpose. "I do not know this
// cluster" is a routing mistake the caller can fix; "I know this cluster and
// I am refusing your policy" is a security decision, and a caller that
// collapses the two will retry the second one forever.
var ErrUnknownCluster = errors.New("federation: unknown cluster")

// ClusterID names one child cluster to a root control plane.
//
// It is a value rather than a bare string because it is validated at
// construction, and every use afterwards — a log field, a path, a map key —
// can then assume it is already safe. A type that validates on the way in and
// is trusted on the way out is the difference between one check and a hundred
// call sites each remembering to run one.
type ClusterID string

// NewClusterID validates and returns a cluster identity.
func NewClusterID(raw string) (ClusterID, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", errors.New("federation: a cluster needs an id")
	}
	if len(id) > MaxClusterIDLen {
		return "", fmt.Errorf("federation: cluster id %q is %d bytes, the limit is %d",
			id, len(id), MaxClusterIDLen)
	}
	if !clusterIDPattern.MatchString(id) {
		return "", fmt.Errorf("federation: cluster id %q must be %s (lowercase letters, digits and dashes, starting and ending with a letter or digit)",
			id, "a DNS label")
	}
	return ClusterID(id), nil
}

// String returns the id.
func (c ClusterID) String() string { return string(c) }

// Valid reports whether the id is well formed.
//
// It exists for the places that receive an id from a wire message rather than
// from NewClusterID, where there is no constructor to have checked it.
func (c ClusterID) Valid() bool {
	return c != "" && len(c) <= MaxClusterIDLen && clusterIDPattern.MatchString(string(c))
}

// Cluster is what a child says about itself when it registers.
//
// Everything in it is a claim the child makes about itself, and the root's
// job is to record the claim and to decide what that claim is allowed to
// receive. It is deliberately not a capability: nothing here grants the child
// anything. A cluster's reach is decided by the bundles it is willing to
// accept, and the acceptance is on the child.
type Cluster struct {
	// ID is the identity the root knows this cluster by. Two children
	// claiming one id is an operator problem, not something to resolve by
	// taking the later claim silently.
	ID ClusterID `json:"id"`
	// Name is what a human sees. It is free-form on purpose — the id has
	// to be a DNS label, and an operator should not have to name their
	// cluster "prod-cn-north-1" to display it as "华东生产一区".
	Name string `json:"name,omitempty"`
	// Version is the child control plane's own version string. It is
	// recorded, not enforced here: a root pushing a bundle that needs a
	// newer child is a rollout problem, and the honest place to catch it
	// is compatibility checking at publish time, not a version string
	// comparison on an opaque value.
	Version string `json:"version,omitempty"`
	// EdgeCount is how many nodes the child says it manages. It is a
	// claim, and it is used for capacity questions ("is this child the one
	// holding two hundred nodes?"), never for accounting.
	EdgeCount int `json:"edge_count,omitempty"`
	// TrustKeyID is the id of the key the child will use to resolve a
	// policy signature. The root uses it to fail a publish early rather
	// than shipping a bundle this particular child cannot verify.
	TrustKeyID string `json:"trust_key_id,omitempty"`
	// JoinedAt is when the root first recorded this cluster.
	JoinedAt time.Time `json:"joined_at"`
	// LastSeen is when the root last heard from it.
	LastSeen time.Time `json:"last_seen"`
}

// Validate checks the fields a root needs before it will record a claim.
func (c Cluster) Validate() error {
	if !c.ID.Valid() {
		return fmt.Errorf("federation: cluster id %q is not a valid identity", c.ID)
	}
	if c.EdgeCount < 0 {
		return fmt.Errorf("federation: cluster %q claims %d nodes, which is not a count anyone has", c.ID, c.EdgeCount)
	}
	return nil
}
