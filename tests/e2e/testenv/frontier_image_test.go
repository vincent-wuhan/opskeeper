//go:build e2e

package testenv

import (
	"errors"
	"strings"
	"testing"
)

// daemonRefusedImage is the difference between "the delivery gate could not
// run" and "the delivery gate ran and found something wrong", and the whole
// value of the classifier is that it never blurs those two.
//
// Every string below is one a daemon actually produced on this repository's
// e2e run, not one invented to fit the matcher. The negative cases matter
// more than the positive ones: they are what stops a real broker regression
// from being reclassified as a missing dependency and quietly skipped past.

func TestTheClassifierSeparatesAMissingImageFromABrokenBroker(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{
			// Observed: the configured registry mirror answers 403 for the
			// whole singchia namespace.
			name: "registry mirror refuses the manifest",
			err: `Error response from daemon: unknown: failed to resolve reference ` +
				`"docker.io/singchia/frontier:1.2.5": unexpected status from HEAD request to ` +
				`https://docker.m.daocloud.io/v2/singchia/frontier/manifests/1.2.5?ns=docker.io: 403 Forbidden`,
			want: true,
		},
		{
			// Observed: the tag does not exist at all.
			name: "manifest unknown",
			err:  `Error response from daemon: manifest unknown: manifest tagged by "docker.io/singchia/frontier:nope" not found`,
			want: true,
		},
		{
			// Observed on the other local engine: a blob the content store
			// claims to hold but cannot read back.
			name: "content store cannot read a blob it just wrote",
			err: `Error response from daemon: rpc error: code = Unknown desc = blob sha256:66e90c7 ` +
				`expected at /var/lib/containerd/io.containerd.content.v1.content/blobs/sha256/66e90c7: ` +
				`open /var/lib/containerd/...: input/output error`,
			want: true,
		},
		{
			name: "no credentials for the namespace",
			err:  `Error response from daemon: pull access denied for singchia/frontier, repository does not exist or may require 'docker login'`,
			want: true,
		},

		// --- the side that must stay hard-failed ---

		{
			// The broker image was obtained. The container ran. It just
			// never printed the line the harness waits for. That is a
			// broker-topology failure and it is the failure this gate
			// exists to catch.
			name: "broker started but never listened",
			err:  `testcontainers: container is not ready: wait.ForLog("edgebound server listening on"): context deadline exceeded after 2m0s`,
			want: false,
		},
		{
			name: "config file is wrong",
			err:  `frontier container: Error response from daemon: No such container: /usr/conf/frontier.yaml`,
			want: false,
		},
		{
			name: "port mapping failed",
			err:  `frontier container: Error response from daemon: driver failed programming external connectivity`,
			want: false,
		},
		{
			// "input/output error" on its own is not an image verdict. A
			// mounted volume on a node that lost power produces one too,
			// and calling that a missing image would hide it.
			name: "an unrelated io error is not an image verdict",
			err:  `Error response from daemon: error while creating mount source path: input/output error`,
			want: false,
		},
		{
			// A slow registry and a broker that never started produce the
			// same words. Refusing to guess between them is the point.
			name: "a bare timeout is deliberately not classified",
			err:  `Error response from daemon: net/http: TLS handshake timeout`,
			want: false,
		},
		{
			name: "no error at all",
			err:  "",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.err != "" {
				err = errors.New(tc.err)
			}
			if got := daemonRefusedImage(err); got != tc.want {
				t.Fatalf("daemonRefusedImage(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTheUnavailableMessageCarriesWhatAnOperatorNeeds(t *testing.T) {
	msg := frontierUnavailableMessage(
		"docker.io/singchia/frontier:1.2.5",
		errors.New("403 Forbidden"),
	)

	// Three things, each of which has been missing from a failure message
	// that cost somebody an afternoon: which image, that no product code
	// ran, and what to type next.
	for _, want := range []string{
		"docker.io/singchia/frontier:1.2.5",      // the image, so nobody re-pulls the wrong one
		"exercised nothing on the delivery path", // so nobody debugs the manager
		"OPSKEEPER_E2E_FRONTIER_IMAGE",           // an action, not just a cause
		"UNVERIFIED",                             // open, not passing
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message is missing %q:\n%s", want, msg)
		}
	}
}
