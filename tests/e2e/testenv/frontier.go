//go:build e2e

package testenv

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Frontier is a running tunnel broker, and the two addresses a deployment
// hands to the two processes that dial it.
//
// The split is the whole reason this file exists. The manager and the node
// are on opposite sides of the broker and neither can reach the other
// directly: 40011 is the servicebound side the manager dials *in* to, and
// 40012 is the edgebound side a node dials *in* to. A harness that handed
// both processes one address would be testing a topology nobody deploys,
// and the failure it hides is the one that matters — the manager's RPC
// registrations and the node's inbound calls travel over different
// listeners, so "the node is connected" and "the manager can call the node"
// are two different facts.
type Frontier struct {
	// ServiceAddr is host:port for the manager to dial (frontier's
	// servicebound listener, 40011 in the shipped config).
	ServiceAddr string
	// EdgeAddr is host:port for a node to dial (frontier's edgebound
	// listener, 40012 in the shipped config).
	EdgeAddr string

	// Architecture is what the broker process actually reports for itself,
	// read out of the running container with `uname -m`.
	//
	// It is a field rather than a comment because the acceptance's own
	// architecture used to be nobody's knowledge: Docker Desktop happily
	// starts an amd64 image on an arm64 host under emulation, prints one
	// WARNING to stderr, and keeps going. So a delivery run on an M-series
	// Mac could be, without anyone noticing, a run where the broker was
	// amd64 and everything else was arm64 — and a reader of the green
	// result had no way to tell which leg they had just verified.
	Architecture string
}

// frontierPlatform returns the platform the harness should ask the daemon
// for, or "" to take the daemon's default.
//
// It exists so CI can pin the leg (`OPSKEEPER_E2E_PLATFORM=linux/amd64`)
// rather than inheriting whatever the developer happens to be running on.
// Pinning does not report the result, which is why Frontier.Architecture
// exists too: one asks, the other answers.
func frontierPlatform() string {
	return strings.TrimSpace(os.Getenv("OPSKEEPER_E2E_PLATFORM"))
}

var (
	frontierOnce sync.Once
	frontierInst *Frontier
	frontierBox  tc.Container
	frontierErr  error
	// frontierNoImage records that the failure was the daemon refusing the
	// image, as opposed to a broker that started and then misbehaved. Only
	// the former is an environment problem, and only the former may be
	// reported in those words.
	frontierNoImage bool
)

// sharedFrontier brings up one tunnel broker per `go test` process.
//
// The config is the repository's own deploy/install/frontier.yaml rather
// than something written here. That file is what a deployment runs, so a
// harness with its own copy would be testing a broker configured in a way
// no deployment is — and the interesting failures in a broker topology are
// configuration failures (which port, whether the idservice allocates edge
// ids) rather than code failures.
func sharedFrontier(t *testing.T) *Frontier {
	t.Helper()
	frontierOnce.Do(func() {
		repo := repoRoot()
		if repo == "" {
			frontierErr = fmt.Errorf("cannot locate repo root from testenv source")
			return
		}
		configPath := filepath.Join(repo, "deploy", "install", "frontier.yaml")
		if _, err := os.Stat(configPath); err != nil {
			frontierErr = fmt.Errorf("frontier config: %w", err)
			return
		}
		if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
			_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		container, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
			ContainerRequest: tc.ContainerRequest{
				Image:         frontierImage(),
				ImagePlatform: frontierPlatform(),
				ExposedPorts: []string{
					"40011/tcp",
					"40012/tcp",
				},
				Cmd: []string{"--config", "/usr/conf/frontier.yaml"},
				Files: []tc.ContainerFile{{
					HostFilePath:      configPath,
					ContainerFilePath: "/usr/conf/frontier.yaml",
					FileMode:          0o644,
				}},
				WaitingFor: wait.ForLog("edgebound server listening on").
					WithStartupTimeout(2 * time.Minute),
			},
			Started: true,
		})
		if err != nil {
			frontierErr = fmt.Errorf("frontier container: %w", err)
			frontierNoImage = daemonRefusedImage(err)
			return
		}
		host, err := container.Host(ctx)
		if err != nil {
			frontierErr = err
			return
		}
		servicePort, err := container.MappedPort(ctx, "40011/tcp")
		if err != nil {
			frontierErr = err
			return
		}
		edgePort, err := container.MappedPort(ctx, "40012/tcp")
		if err != nil {
			frontierErr = err
			return
		}
		arch, err := containerArchitecture(ctx, container)
		if err != nil {
			frontierErr = fmt.Errorf("frontier architecture: %w", err)
			return
		}
		frontierBox = container
		frontierInst = &Frontier{
			ServiceAddr:  fmt.Sprintf("%s:%s", host, servicePort.Port()),
			EdgeAddr:     fmt.Sprintf("%s:%s", host, edgePort.Port()),
			Architecture: arch,
		}
	})
	if frontierErr != nil {
		if frontierNoImage {
			// Still a failure. The delivery acceptance has not been
			// delivered, and a red gate that says why is worth more than
			// a green one that means nothing.
			t.Fatal(frontierUnavailableMessage(frontierImage(), frontierErr))
		}
		t.Fatalf("testenv: %v", frontierErr)
	}
	return frontierInst
}

// SharedFrontier is sharedFrontier, exported for tests that need the
// addresses to hand to something other than a manager (a node, mainly).
func SharedFrontier(t *testing.T) *Frontier { return sharedFrontier(t) }

// defaultFrontierImage is the broker this harness runs, and it is the same
// broker the release ships.
//
// Getting that sentence true took a decision this file used to leave
// unmade. The broker reaches operators two ways. The release builds it from
// an upstream git tag and ships the image inside the tarball, so those files
// name a local image — `singchia/frontier:v1.2.5`, a tag that has never
// existed on Docker Hub and is never pulled. The development stack and this
// harness pull it, so they say `1.2.5`, which is how upstream tags its
// published images. Two spellings, one version, six files.
//
// They disagreed: the release shipped v1.2.4 while this harness ran 1.2.5.
// Every individual file was correct and every comment explained itself, so
// nothing was red. The property that mattered — that the acceptance proves
// something about the thing that ships — had no owner. `make
// broker-pin-check` owns it now.
//
// One thing is still not this repository's to fix. A machine whose registry
// cannot reach the singchia namespace cannot pull either spelling, and then
// these two tests cannot run at all. That is an environment precondition and
// it is reported as one (see frontier_image.go) rather than as a failure of
// the thing under test. Building the broker from source with the same
// Dockerfile the release uses sidesteps it entirely:
//
//	make docker-build-broker FRONTIER_SRC=/path/to/frontier
//	OPSKEEPER_E2E_FRONTIER_IMAGE=singchia/frontier:v1.2.5 make e2e-delivery-check
//
// That override names the shipped image, so the acceptance stops testing a
// broker the release never ships.
const defaultFrontierImage = "docker.io/singchia/frontier:1.2.5"

// frontierImage resolves the broker image, overridable so a mirror or a
// locally built tag can be used without editing the test.
func frontierImage() string {
	if image := strings.TrimSpace(os.Getenv("OPSKEEPER_E2E_FRONTIER_IMAGE")); image != "" {
		return image
	}
	return defaultFrontierImage
}

// TerminateSharedFrontier kills the broker container. Called from TestMain
// alongside TerminateSharedMySQL, for the same reason: ryuk is disabled,
// so nothing else reaps it.
func TerminateSharedFrontier() {
	if frontierBox == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = frontierBox.Terminate(ctx)
	frontierBox = nil
}

// WithFrontier points a manager at a real tunnel broker instead of the
// disabled one the default harness uses.
//
// The default harness sets OPSKEEPER_FRONTIER_DISABLED and reaches edge
// flows through the in-process edgesim helper. That is the right trade for
// twenty tests and the wrong one for the delivery acceptance: the thing
// being accepted is a node process and a manager process talking across a
// broker, and a simulated edge answers none of the questions it is meant
// to.
func WithFrontier(f *Frontier) Option {
	return func(c *envConfig) {
		if c.extraEnv == nil {
			c.extraEnv = map[string]string{}
		}
		c.extraEnv["OPSKEEPER_FRONTIER_DISABLED"] = "false"
		c.extraEnv["OPSKEEPER_FRONTIER_ADDR"] = f.ServiceAddr
	}
}

// containerArchitecture asks the running broker what architecture it is.
//
// `uname -m` inside the container rather than an inspect call, because the
// property being asked for is the one the process would act on. A container
// that fails this is a broker we cannot make claims about, and the harness
// says so rather than continuing with an unknown leg.
func containerArchitecture(ctx context.Context, c tc.Container) (string, error) {
	code, out, err := c.Exec(ctx, []string{"uname", "-m"})
	if err != nil {
		return "", fmt.Errorf("exec uname -m: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("uname -m exited %d in the broker container", code)
	}
	// The exec reader hands back whatever the multiplexed stream carried,
	// padding and non-printables included, so the value is filtered down to
	// what an architecture name is made of rather than trimmed: a leading
	// NUL survives strings.TrimSpace and would end up in the log line and,
	// worse, in any equality check against runtime.GOARCH.
	arch := sanitizeArch(readAll(out))
	if arch == "" {
		return "", fmt.Errorf("uname -m printed nothing usable in the broker container")
	}
	return arch, nil
}

// sanitizeArch keeps [a-z0-9_] from the first thing that looks like an
// architecture name and drops the rest.
func sanitizeArch(raw string) string {
	var sb strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(r + 32)
		case r == '\n' && sb.Len() > 0:
			return sb.String()
		}
	}
	return sb.String()
}

func readAll(r io.Reader) string {
	var sb strings.Builder
	_, _ = io.Copy(&sb, r)
	return sb.String()
}
