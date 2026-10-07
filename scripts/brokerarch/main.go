// Command brokerarch answers one question about the tunnel broker image the
// end-to-end harness pulls: which architectures does the published manifest
// actually offer?
//
// Why this is a command and not a paragraph. Decision 186 registered the
// arm64 gap as "the node's pig agent has never been run on arm", and decision
// 190 narrowed it to a single blocking question — the delivery job is the
// only thing in this repository that builds pig, it is an amd64 single leg,
// and it pulls its broker from a public registry. Adding an arm64 leg is
// cheap if that image has an arm64 manifest and impossible if it does not.
// Those two branches differ by one upstream checkout, so the question is
// worth asking mechanically instead of by hand.
//
// The exit status is the point of this command, so it is worth stating
// plainly:
//
//	0  the manifest was read and it offers linux/arm64  -> branch (a)
//	1  the manifest was read and it does not           -> branch (b)
//	2  this command was misused
//	3  UNKNOWN: the registry could not be asked
//
// Exit 1 and exit 3 are different worlds and must never be confused. A
// registry that cannot be reached has not told us the image is single-
// architecture; it has told us nothing. The harness already learned this
// lesson the hard way in tests/e2e/testenv/frontier_image.go, where a daemon
// refusal is classified as an environment precondition rather than a defect
// in the delivery path. The same discipline applies here one layer earlier:
// this command never reports "no arm64" on the strength of a failed request.
//
// Usage:
//
//	go run ./scripts/brokerarch .
//	go run ./scripts/brokerarch --registry-base https://registry-1.docker.io .
//
// --registry-base exists so the tests can point this at a fake registry. It
// is not a mirror switch: pointing it at a mirror that cannot serve the
// singchia namespace answers nothing, and the command will say so rather than
// guess.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Exit statuses. Named so the Makefile and the tests read as prose.
const (
	exitArmPresent = 0 // the manifest offers linux/arm64
	exitArmAbsent  = 1 // the manifest was read; it offers no linux/arm64
	exitUsage      = 2 // this command was misused
	exitUnknown    = 3 // the registry could not be asked
)

// armTarget is the platform the delivery leg would need in order to run on
// ubuntu-24.04-arm.
const armTarget = "linux/arm64"

// harnessSource is where the image reference lives. This command reads the
// constant instead of repeating it, because a second spelling of the broker
// image is exactly the drift decision 153 was built to stop, and a check
// that can itself drift is not a check.
const harnessSource = "tests/e2e/testenv/frontier.go"

const constName = "defaultFrontierImage"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run takes its writer so the tests can read the verdict instead of the
// process exit status alone. The verdict line is the thing an operator
// reads, and a test that only saw the status would not notice if the line
// stopped being printed.
func run(args []string, out io.Writer) int {
	registryBase := "https://registry-1.docker.io"
	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--registry-base" && i+1 < len(args):
			registryBase = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--registry-base="):
			registryBase = strings.TrimPrefix(args[i], "--registry-base=")
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(out, "usage: brokerarch [--registry-base URL] <repo-root>")
			return exitUsage
		default:
			positional = append(positional, args[i])
		}
	}

	// The repository root is required rather than discovered. The obvious way
	// to discover it is core/floor/reporoot, and .go-arch-lint.yml does not
	// let a script's production code reach into a component — a test may,
	// which is why the tests below import it and this file does not. The
	// Makefile passes ".".
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "brokerarch: pass the repository root (make broker-arch-report does this for you)")
		return exitUsage
	}
	root := positional[0]

	ref, err := harnessImage(filepath.Join(root, harnessSource))
	if err != nil {
		fmt.Fprintf(os.Stderr, "brokerarch: %v\n", err)
		return exitUsage
	}

	repo, tag := splitRef(ref)
	if repo == "" || tag == "" {
		fmt.Fprintf(os.Stderr, "brokerarch: %q is not an image reference this command can ask about\n", ref)
		return exitUsage
	}

	fmt.Fprintf(out, "brokerarch: asking %s about %s (%s:%s)\n", registryBase, ref, repo, tag)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 40 * time.Second}
	platforms, err := readManifestPlatforms(ctx, client, registryBase, repo, tag)
	if err != nil {
		// The whole point: an unreachable registry produces this line and
		// exit 3. It is not allowed to fall through to the "no arm64"
		// verdict, because that verdict is a claim about the image and we
		// have no claim to make.
		fmt.Fprintf(out, "\nVERDICT: UNKNOWN — the registry did not answer, so this run says nothing about arm64.\n  cause: %v\n", err)
		fmt.Fprintln(out, "  exit 3 is not exit 1: a registry that cannot be reached has not reported a")
		fmt.Fprintln(out, "  single-architecture image. Ask again from a network that can reach it, or")
		fmt.Fprintln(out, "  build the broker from source (make docker-build-broker).")
		return exitUnknown
	}

	fmt.Fprintf(out, "\nplatforms offered by %s:%s\n", repo, tag)
	for _, p := range platforms {
		fmt.Fprintf(out, "  %s\n", p)
	}
	for _, p := range platforms {
		if offersArm64(p) {
			fmt.Fprintln(out, "\nVERDICT: an arm64 linux manifest IS offered. The delivery job can take")
			fmt.Fprintln(out, "  an arm64 leg with no upstream checkout: matrix the runner and the")
			fmt.Fprintln(out, "  existing pull is enough.")
			return exitArmPresent
		}
	}
	fmt.Fprintln(out, "\nVERDICT: no arm64 linux manifest is offered. An arm64 delivery leg has to")
	fmt.Fprintln(out, "  build the broker from source first (make docker-build-broker, which follows")
	fmt.Fprintln(out, "  TARGET_ARCH), or the arm64 leg stays a registered gap. See decision 190.")
	return exitArmAbsent
}

// platformString renders one platform the way a manifest names it. The
// variant is part of the name: Docker publishes arm64 as "linux/arm64/v8"
// often enough that dropping it would make this command report an image as
// single-architecture when it is not.
func platformString(os, arch, variant string) string {
	out := os + "/" + arch
	if variant != "" {
		out += "/" + variant
	}
	return out
}

// offersArm64 is deliberately not an equality test against "linux/arm64".
// A variant suffix is part of the platform name, and an arm64 manifest that
// carries one is still an arm64 manifest — comparing strings would answer
// "not offered" about an image that does offer arm, which is the one
// direction this command must never be wrong in.
func offersArm64(platform string) bool {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 {
		return false
	}
	return parts[0] == "linux" && parts[1] == "arm64"
}

// harnessImage reads the image reference out of the harness constant rather
// than repeating it here.
func harnessImage(path string) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != constName || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				return strings.Trim(lit.Value, `"`), nil
			}
		}
	}
	return "", fmt.Errorf("%s declares no string constant %s", path, constName)
}

// splitRef separates the repository path from the tag. The registry host is
// not needed: the caller already decided which registry to ask.
func splitRef(ref string) (repo, tag string) {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "registry-1.docker.io/")
	if at := strings.LastIndex(ref, "@"); at > 0 {
		return ref[:at], ref[at+1:]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

type indexOrManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

type imageConfig struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

const acceptHeader = "application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.oci.image.manifest.v1+json"

// readManifestPlatforms asks the registry which platforms a tag resolves to.
func readManifestPlatforms(ctx context.Context, client *http.Client, base, repo, tag string) ([]string, error) {
	token, err := pullToken(ctx, client, base, repo)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: registry answered %s", resp.Status)
	}

	var doc indexOrManifest
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	if len(doc.Manifests) > 0 {
		out := make([]string, 0, len(doc.Manifests))
		for _, m := range doc.Manifests {
			out = append(out, platformString(m.Platform.OS, m.Platform.Architecture, m.Platform.Variant))
		}
		return out, nil
	}

	// Single-platform manifest: the architecture is in the config blob.
	if doc.Config.Digest == "" {
		return nil, fmt.Errorf("manifest carries neither a platform list nor a config digest")
	}
	cfg, err := readConfig(ctx, client, base, repo, doc.Config.Digest, token)
	if err != nil {
		return nil, err
	}
	return []string{cfg.OS + "/" + cfg.Architecture}, nil
}

func readConfig(ctx context.Context, client *http.Client, base, repo, digest, token string) (imageConfig, error) {
	var cfg imageConfig
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/v2/"+repo+"/blobs/"+digest, nil)
	if err != nil {
		return cfg, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("fetch config blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cfg, fmt.Errorf("fetch config blob: registry answered %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config blob: %w", err)
	}
	return cfg, nil
}

// pullToken asks for an anonymous pull token. Docker Hub serves one without
// credentials for public namespaces; a registry that does not (a mirror, a
// private host) may answer 401, and that is not fatal here — the manifest
// request is attempted anyway, because some registries serve anonymous pulls
// without issuing a token at all.
func pullToken(ctx context.Context, client *http.Client, base, repo string) (string, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	tokenURL := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)
	if host != "registry-1.docker.io" {
		tokenURL = strings.TrimSuffix(base, "/") + "/token?scope=repository:" + repo + ":pull"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		// No token is not a failed answer; it is a registry that will not
		// issue one. The manifest request decides.
		return "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", nil
	}
	return body.Token, nil
}
