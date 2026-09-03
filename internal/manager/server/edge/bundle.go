// bundle.go — edge upgrade bundle resolver + static HTTP file
// server. The manager image bakes bundles at
// /usr/share/opskeeper/edge-bundles/edge-bundle-<arch>-<version>.tar.gz
// (plus .sha256 companion); we expose them at
// /api/v1/edge-bundle/<arch>/<filename> so the edge can pull them
// over the same nginx pipeline it already trusts.
package edge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// FileBundleResolver implements PackageResolver against a local
// directory layout. Bundle files are named:
//
//	edge-bundle-<arch>-<version>.tar.gz
//	edge-bundle-<arch>-<version>.tar.gz.sha256
//
// (see dist/build-edge-bundle.sh). The resolver picks the bundle
// matching the requested (arch, version); empty version → "manager's
// own version" which the constructor takes verbatim. The returned URL
// is publicURL + the static route this same file registers; admins
// don't have to know the manager's listening port.
type FileBundleResolver struct {
	dir            string
	managerVersion string
	publicURL      string
}

// NewFileBundleResolver builds the resolver. dir typically
// /usr/share/opskeeper/edge-bundles (set by Dockerfile.opskeeper). publicURL
// is the manager's externally-reachable origin (no trailing slash);
// resolver constructs `{publicURL}/api/v1/edge-bundle/<arch>/<file>`.
func NewFileBundleResolver(dir, managerVersion, publicURL string) *FileBundleResolver {
	return &FileBundleResolver{
		dir:            strings.TrimRight(dir, "/"),
		managerVersion: managerVersion,
		publicURL:      strings.TrimRight(publicURL, "/"),
	}
}

// ResolveBundle implements PackageResolver.
func (r *FileBundleResolver) ResolveBundle(arch, version string) (url, sha256, resolvedVersion string, err error) {
	if r == nil {
		return "", "", "", errors.New("bundle resolver not wired")
	}
	if !knownArch(arch) {
		return "", "", "", fmt.Errorf("%w: unsupported arch %q", errs.ErrInvalid, arch)
	}
	if strings.TrimSpace(version) == "" {
		version = r.managerVersion
	}
	if version == "" {
		return "", "", "", errors.New("manager version unknown; cannot resolve bundle")
	}
	name := fmt.Sprintf("edge-bundle-%s-%s.tar.gz", arch, version)
	tarball := filepath.Join(r.dir, name)
	if _, err := os.Stat(tarball); err != nil {
		return "", "", "", fmt.Errorf("bundle missing: %s (this manager image may have been built without build-edge-bundle)", name)
	}
	shaBytes, err := os.ReadFile(tarball + ".sha256")
	if err != nil {
		return "", "", "", fmt.Errorf("bundle sha file missing: %s.sha256", name)
	}
	sha := strings.TrimSpace(string(shaBytes))
	if len(sha) < 64 {
		return "", "", "", fmt.Errorf("bundle sha file malformed: %s.sha256", name)
	}
	sha = sha[:64]
	if r.publicURL == "" {
		return "", "", "", errors.New("publicURL not configured; cannot build bundle URL")
	}
	// Bundle bytes are served by nginx from the same /edge/ static
	// path it already exposes for install.sh + individual binaries —
	// the bundle file lands next to them after install/upgrade.sh
	// extracts edge-bundles/ into bin/ (host) → /usr/share/nginx/
	// html/edge/ (container). Anonymous fetch; sha256 is the gate.
	return fmt.Sprintf("%s/edge/%s", r.publicURL, name), sha, version, nil
}

func knownArch(a string) bool {
	switch a {
	case "linux-amd64":
		return true
	case "linux-arm64":
		return true
	default:
		return false
	}
}
