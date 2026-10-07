//go:build e2e

package testenv

import (
	"fmt"
	"strings"
)

// The broker image is the one input this harness cannot supply for itself.
//
// Every other input is either in the repository (the manager and edge
// binaries, deploy/install/frontier.yaml) or a public image any daemon can
// fetch. The broker is neither: singchia/frontier does not resolve through
// the registry mirror this machine is configured with, and the namespace
// needs credentials the harness does not have. When the daemon refuses, the
// refusal arrives as one long sentence from a container-create call, and the
// tests that need it fail with a line that reads like a defect in the
// delivery path. It is not one. Nothing about the manager, the node, the pig
// subprocess or the tunnel protocol has been exercised at that point — the
// run stopped before any of them started.
//
// So the failure is classified, not swallowed. It stays a FAIL. A delivery
// gate that reports green without having delivered is a worse outcome than
// one that is red, and this repository has already been bitten by that
// shape: the coverage gate once had an axis that could not move, and a
// number that cannot move cannot catch a regression. What changes here is
// the sentence an operator reads, not the verdict.

// imageRefusalMarkers are the phrases a daemon uses when it declines to
// hand over an image.
//
// The list is deliberately short, and every entry names a refusal rather
// than a failure. That distinction is the whole point: a broker that was
// obtained and then did not come up must NOT be classified as an image
// problem, because that is a real regression in the broker topology and it
// has to stay red. The tempting entry to add is a timeout, and it is
// excluded on purpose — wait.ForLog's startup deadline and a slow registry
// are indistinguishable in the text, and guessing between them would turn a
// broken broker into a skip.
var imageRefusalMarkers = []string{
	"failed to resolve reference",
	"manifest unknown",
	"pull access denied",
	"repository does not exist",
	"no such host",
	"401 unauthorized",
	"403 forbidden",
	"toomanyrequests",
}

// daemonRefusedImage reports whether err is the daemon declining to fetch
// the broker image, as opposed to a broker that was fetched and then failed.
//
// Unrecognised text answers false, which keeps the original error intact and
// the failure hard. Guessing the other way — assuming anything unrecognised
// is environmental — is how a real defect gets reclassified as a missing
// dependency and stops being looked at.
func daemonRefusedImage(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range imageRefusalMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	// A content store that cannot read back a blob it just wrote is a
	// separate fault from a registry that will not serve a manifest, and it
	// needs both halves: a single engine that lists its own images can fail
	// this way, and "input/output error" alone appears in unrelated paths.
	if strings.Contains(msg, "blob") && strings.Contains(msg, "input/output error") {
		return true
	}
	return false
}

// frontierUnavailableMessage is what an operator reads when the broker
// image could not be obtained.
//
// It has to carry three things or it is not worth printing: which image, so
// nobody re-pulls the wrong one; that no product code ran, so nobody goes
// looking for a defect in the manager or the node; and what to do, because
// "the registry said no" is not an action.
func frontierUnavailableMessage(image string, err error) string {
	return fmt.Sprintf(`the tunnel broker image could not be obtained, so this run exercised nothing on the delivery path.

  image:  %s
  daemon: %v

This is an environment precondition, not a defect in the manager, the node,
the pig subprocess or the tunnel protocol. The container was never created,
so none of them ran.

Two ways out, in the order worth trying:
  1. point the harness at an image this machine can reach
       OPSKEEPER_E2E_FRONTIER_IMAGE=<ref> make test-e2e
  2. make the registry reachable for that namespace (credentials, or an
     unproxied path to docker.io)

Until one of them is done, the delivery acceptance and the node
telemetry-through-an-outage check are UNVERIFIED. They are open, not
passing, and nothing downstream of them should be read as evidence.`, image, err)
}
