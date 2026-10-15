// Package marketplace is the manager-side application service for the
// skill marketplace install / list / uninstall workflow.
//
// Layout (mirrors biz/setting):
//
//	source.go — Source / SourceType / Caller value types
//	signature.go — VerifySignature stub (cosign skeleton)
//	repo.go — Repo interface + InstallResult / CapabilityDeclaration
//	usecase.go — Usecase = the orchestrator wired in cmd/opskeeper
package marketplace

// SourceType is a constant set of install transports —
// (4 个安装路径). The string values are also the canonical
// wire shape over the install API and are persisted into
// installed_skills.source for diagnostics.
type SourceType string

const (
	// SourceTypeLocal copies a directory already on the manager host
	// (admin scp'd it). Path must be absolute and existing.
	SourceTypeLocal SourceType = "local"

	// SourceTypeTarball curl + tar -xz a remote .tgz / .tar.gz into
	// staging.
	SourceTypeTarball SourceType = "tarball"

	// SourceTypeGit git clone --depth=1 (--branch=Ref) into staging.
	SourceTypeGit SourceType = "git"

	// SourceTypeRegistry resolves (Registry, PackID, Version) through a
	// registry index this control plane is configured to read, and installs
	// what that index names.
	//
	// Three things about it are deliberate and each has a refusal behind it:
	//
	//   - the url and the digest come from the index, never from the
	//     request. A client that could hand over a url here could install
	//     any tarball while the install ledger recorded it as having come
	//     from an allowlisted registry.
	//   - the version is required. "The latest" is what a human wants and
	//     what a node must not do: a floating version leaves the node
	//     holding bytes nobody compared a digest against at review time.
	//   - the registry label must be one this control plane was configured
	//     with (RegistryIndexes), on top of the source allowlist.
	SourceTypeRegistry SourceType = "registry"
)

// Source describes where a pack should be fetched from. Exactly one
// of the typed fields is required by Type; the rest are ignored.
//
// The struct is the wire shape decoded from the install request body
// — keeping it flat (no nested oneOf) matches the openapi style we
// already use elsewhere (see core/domains/server/integration).
type Source struct {
	Type SourceType `json:"type"`

	// Path is the absolute host path for SourceTypeLocal.
	Path string `json:"path,omitempty"`

	// URL is the http(s) URL for SourceTypeTarball / SourceTypeGit.
	URL string `json:"url,omitempty"`

	// Ref is the optional git ref / branch / tag for SourceTypeGit.
	// Empty defaults to HEAD of the default branch.
	Ref string `json:"ref,omitempty"`

	// Registry is the configured registry name for SourceTypeRegistry
	// (e.g. "opskeeper-official"). Must appear in cfg.AllowedSources or
	// cfg.DevMode must be true.
	Registry string `json:"registry,omitempty"`

	// PackID is the slug to fetch from the registry — e.g.
	// "etcd-troubleshoot".
	PackID string `json:"pack_id,omitempty"`

	// Version is the requested version under PackID (semver).
	Version string `json:"version,omitempty"`
}

// SourceLabel is the value persisted into installed_skills.source —
// either the source kind for non-registry installs, or the registry
// name for registry installs. Stable wire shape.
func (s Source) SourceLabel() string {
	if s.Type == SourceTypeRegistry && s.Registry != "" {
		return s.Registry
	}
	return string(s.Type)
}

// SourceURL is the diagnostic value persisted into
// installed_skills.source_url — best-effort human-readable origin.
func (s Source) SourceURL() string {
	switch s.Type {
	case SourceTypeLocal:
		return s.Path
	case SourceTypeTarball, SourceTypeGit:
		return s.URL
	case SourceTypeRegistry:
		return s.Registry + ":" + s.PackID + "@" + s.Version
	}
	return ""
}

// Caller is the per-request identity of the API caller. The HTTP
// handler builds it from tenantctx; biz layer uses it for tenant
// scoping + audit (installed_by). Mirrors the shape of
// server/setting.caller without importing it.
type Caller struct {
	UserID   uint64
	TenantID uint64
	Role     string
}

// IsAdmin returns true if the caller has the admin role. Install /
// uninstall require admin.
func (c Caller) IsAdmin() bool { return c.Role == "admin" }
