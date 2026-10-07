package pluginmanifest

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Rolling a package out in waves, with the waves gated on each other.
//
// The reason this exists rather than being a loop at the call site is that
// the safety property is a rule about *time*: a new wave starts after the
// current one has proved itself, not after enough time has passed. A
// caller that loops "install, sleep, next" has written the second rule
// while believing it wrote the first, and the difference shows up as a
// package reaching the whole fleet while the node that would have caught
// it is still restarting.
//
// So the gate is here, and Advance refuses to move while anything in the
// current wave is unconfirmed.

// WaveSize bounds how many nodes one wave may hold.
//
// A package is admitted to one node, then to a handful, then to the rest.
// A wave larger than this collapses into a single wave, which is a
// legitimate choice for a package that changes nothing — but it should be
// one somebody made, not one that fell out of a default.
const WaveSize = 10

// CanaryFraction is the share of nodes that goes first, expressed in
// tenths so it can be tuned without floating point rounding deciding which
// nodes a release reaches.
//
// One tenth is a number with a reason behind it: enough nodes to notice a
// problem that only some nodes have — a kernel version, a filesystem, a
// particular service — and few enough that a mistake costs a tenth of the
// fleet rather than all of it.
const CanaryFractionTenths = 1

// Rollout is one package moving to a set of nodes.
//
// The zero value is not usable; build one with PlanRollout. It is not
// safe by default because there is no sensible default for "which nodes
// and in what order" — guessing would be a fleet-sized decision made
// without anybody choosing it.
type Rollout struct {
	// plugin and version identify what is being rolled out, for logs and
	// for the console's progress display.
	plugin  string
	version string
	// waves is the plan, in order. waves[0] is the canary.
	waves [][]uint64
	// current is the index of the wave in flight.
	current int
	// confirmed records the nodes that reported the install healthy.
	// A node in the current wave that is not here blocks Advance.
	confirmed map[uint64]bool
	// failed records nodes that reported a failure. A failure does not
	// halt the rollout by itself — a canary that fails should stop
	// everything, so Failed reports it and it is the caller's decision
	// whether one failure halts a fleet or is an isolated node.
	failed map[uint64]error
}

// PlanRollout orders the nodes for a package.
//
// The canary is chosen deterministically from the package's identity, not
// sampled randomly. That is deliberate: a random canary means a retried
// rollout canary a different fleet, so a package that failed on ten
// percent of nodes gets another ten percent on the retry, and the node
// that failed last time may never be retried at all. Hashing the package
// name and version means the same release always reaches the same first
// nodes, so "the canary" is a set an operator can name.
func PlanRollout(plugin, version string, nodes []uint64, strategy string) (*Rollout, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("pluginmanifest: %s v%s has no nodes to roll out to", plugin, version)
	}

	ordered := append([]uint64(nil), nodes...)
	// Sorted first so the input order cannot change the plan; the hash
	// below then imposes the release-dependent order on top of it.
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	// Duplicates would put one node in two waves, and the second wave
	// would install over the first without waiting for it.
	for i := 1; i < len(ordered); i++ {
		if ordered[i] == ordered[i-1] {
			return nil, fmt.Errorf("pluginmanifest: node %d appears twice in the rollout", ordered[i])
		}
	}

	shuffled := releaseShuffle(plugin, version, ordered)

	var waves [][]uint64
	switch strategy {
	case domain.InstallPin:
		// A pinned package is one an operator chose deliberately and that
		// will not be auto-upgraded later, so it goes out as a single
		// wave. Canary-ing it would be theatre: the next release will not
		// be canaried either.
		waves = [][]uint64{ordered}
	default:
		// Rolling. A canary first, then the rest in waves.
		canary := len(ordered) * CanaryFractionTenths / 10
		if canary < 1 {
			// Never a zero-node first wave: a rollout that starts by
			// installing to the whole fleet minus a canary it never ran
			// is a canary that does not exist.
			canary = 1
		}
		if canary > len(ordered) {
			canary = len(ordered)
		}
		waves = append(waves, shuffled[:canary])
		for rest := shuffled[canary:]; len(rest) > 0; {
			n := WaveSize
			if n > len(rest) {
				n = len(rest)
			}
			waves = append(waves, rest[:n])
			rest = rest[n:]
		}
	}
	return &Rollout{
		plugin:    plugin,
		version:   version,
		waves:     waves,
		confirmed: map[uint64]bool{},
		failed:    map[uint64]error{},
	}, nil
}

// Plugin returns the package being rolled out.
func (r *Rollout) Plugin() string { return r.plugin }

// Version returns its version.
func (r *Rollout) Version() string { return r.version }

// Wave returns the nodes in the wave currently in flight.
func (r *Rollout) Wave() []uint64 {
	if r == nil || r.current >= len(r.waves) {
		return nil
	}
	return append([]uint64(nil), r.waves[r.current]...)
}

// WaveNumber is the 1-based index of the current wave, for a progress
// display that says "wave 2 of 4" rather than nothing.
func (r *Rollout) WaveNumber() int {
	if r == nil {
		return 0
	}
	return r.current + 1
}

// WaveCount is the total number of waves.
func (r *Rollout) WaveCount() int {
	if r == nil {
		return 0
	}
	return len(r.waves)
}

// Confirm records that a node reported the install healthy.
func (r *Rollout) Confirm(edgeID uint64) {
	if r == nil {
		return
	}
	r.confirmed[edgeID] = true
}

// Fail records that a node reported a failure.
func (r *Rollout) Fail(edgeID uint64, err error) {
	if r == nil {
		return
	}
	r.failed[edgeID] = err
	delete(r.confirmed, edgeID)
}

// Pending returns the nodes in the current wave that have neither
// confirmed nor failed.
func (r *Rollout) Pending() []uint64 {
	var out []uint64
	for _, id := range r.Wave() {
		if !r.confirmed[id] {
			if _, bad := r.failed[id]; !bad {
				out = append(out, id)
			}
		}
	}
	return out
}

// Failed returns the nodes in the current wave that reported a failure.
func (r *Rollout) Failed() []uint64 {
	var out []uint64
	for _, id := range r.Wave() {
		if _, bad := r.failed[id]; bad {
			out = append(out, id)
		}
	}
	return out
}

// Advance moves to the next wave once the current one is fully accounted
// for, and reports whether it did.
//
// "Accounted for" is confirmed *or* failed, not confirmed only. A rollout
// that refuses to advance because one node never reported would stall the
// fleet on a node that is down for unrelated reasons, which is a worse
// outcome than proceeding with the information the operator already has.
// The caller sees Failed() and can decide; this method only refuses to
// move while somebody is still going to answer.
func (r *Rollout) Advance() bool {
	if r == nil || r.current >= len(r.waves)-1 {
		return false
	}
	if len(r.Pending()) > 0 {
		return false
	}
	r.current++
	return true
}

// Done reports whether every wave has been worked through.
func (r *Rollout) Done() bool {
	return r != nil && r.current >= len(r.waves)-1 && len(r.Pending()) == 0
}

// Progress is a one-line summary for a log or a console.
func (r *Rollout) Progress() string {
	if r == nil {
		return "no rollout"
	}
	confirmed, failed := 0, 0
	for _, id := range r.Wave() {
		if r.confirmed[id] {
			confirmed++
		}
		if _, bad := r.failed[id]; bad {
			failed++
		}
	}
	return fmt.Sprintf("%s v%s wave %d/%d: %d ok, %d failed, %d awaiting",
		r.plugin, r.version, r.current+1, len(r.waves), confirmed, failed, len(r.Pending()))
}

// releaseShuffle orders nodes by a hash of the release identity and the
// node id.
//
// Hashing both is what makes the order stable for one release and
// different across releases: the same node leads one version and is in the
// middle of the next, so a canary keeps testing different machines instead
// of re-testing the one that has just been proven.
func releaseShuffle(plugin, version string, nodes []uint64) []uint64 {
	out := append([]uint64(nil), nodes...)
	keys := make(map[uint64][32]byte, len(out))
	for _, id := range out {
		keys[id] = releaseKey(plugin, version, id)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := keys[out[i]], keys[out[j]]
		for k := range a {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return out[i] < out[j]
	})
	return out
}

func releaseKey(plugin, version string, node uint64) [32]byte {
	h := sha256.New()
	// The fields are length-prefixed for the same reason the tree digest's
	// are: "ab"+"c" and "a"+"bc" must not collide.
	writeField(h, plugin)
	writeField(h, version)
	var id [8]byte
	binary.BigEndian.PutUint64(id[:], node)
	h.Write(id[:])
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
