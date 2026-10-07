package crystallize

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Draft is one emitted package waiting for a human.
//
// It is a value rather than a path because rendering and writing are
// separate acts: a console wants to show a draft, a diff wants to compare it
// with the admitted version, and neither should touch a disk to do it. Write
// is the only method that puts anything anywhere, and it refuses to
// overwrite, because a draft that silently replaces a package is a review
// that did not happen.
type Draft struct {
	Manifest domain.PluginManifest
	// Run is the evidence the draft was promoted on. It is not written into
	// the manifest — the manifest is a contract, not a log — but it travels
	// with the draft so the console can render "promoted on 3 clean
	// verifications" next to the Approve button.
	Run Run
}

// Name is the package's name.
func (d Draft) Name() string { return d.Manifest.Metadata.Name }

// ActionName is the declaration's action name, which is what appears in the
// node's audit rows when it fires.
func (d Draft) ActionName() string {
	if len(d.Manifest.Spec.Autonomy.Actions) == 0 {
		return ""
	}
	return d.Manifest.Spec.Autonomy.Actions[0].Name
}

// YAML renders the draft as a pig-ops.yaml document.
//
// The provenance block is a comment rather than a field on purpose. The
// manifest schema is the plugin contract and has no place to record why a
// human should trust it; a reviewer reads a file, and the file is where the
// answer belongs. Comments survive admission because the loader decodes the
// document and never re-renders it.
func (d Draft) YAML() ([]byte, error) {
	body, err := yaml.Marshal(d.Manifest)
	if err != nil {
		return nil, fmt.Errorf("crystallize: render draft: %w", err)
	}
	var buf bytes.Buffer
	r := d.Run
	fmt.Fprintf(&buf, "# Draft, not a release: emitted by opskeeper crystallize and awaiting review.\n")
	fmt.Fprintf(&buf, "# Pattern: %s\n", d.Manifest.Metadata.Name)
	fmt.Fprintf(&buf, "# Fault:   %s on %s\n", r.Pattern.Fault.Kind, familyOrUnknown(r.Pattern.Fault.Family))
	fmt.Fprintf(&buf, "# Target:  %s\n", r.Pattern.Action.Target)
	fmt.Fprintf(&buf, "# Evidence: %d of %d attempts verified on the first try, %d consecutive; first %s, last %s\n",
		r.Verified, r.Attempts, r.Streak, r.FirstSeen.UTC().Format(time.RFC3339), r.LastSeen.UTC().Format(time.RFC3339))
	if len(r.Evidence) > 0 {
		fmt.Fprintf(&buf, "# Runs:    %s\n", joinEvidence(r.Evidence))
	}
	fmt.Fprintf(&buf, "# The argv, trigger, reach and window below were observed on those runs, not chosen here.\n")
	fmt.Fprintf(&buf, "# Scopes, signature and admission are a human's decision and are not in this file.\n")
	buf.Write(body)
	return buf.Bytes(), nil
}

func familyOrUnknown(f string) string {
	if f == "" {
		return "(family not recorded)"
	}
	return f
}

func joinEvidence(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// Write lays the draft down as a package directory under base and returns the
// directory. It refuses to overwrite an existing package.
func (d Draft) Write(base string) (string, error) {
	if base == "" {
		return "", errors.New("crystallize: draft write needs a base directory")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("crystallize: draft base %s: %w", base, err)
	}
	dir := filepath.Join(base, d.Manifest.Metadata.Name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("crystallize: refusing to write draft into %s: %w", dir, err)
	}
	data, err := d.YAML()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, pluginmanifest.ManifestFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("crystallize: write %s: %w", path, err)
	}
	return dir, nil
}

// DraftFor renders one promoted pattern as a draft.
//
// Every field comes from the evidence or the policy, and the result is
// validated with the same code the control plane uses to admit a package, so
// a draft that cannot be admitted is an error here rather than a mysterious
// refusal at install time.
func (l *Ledger) DraftFor(r Run) (Draft, error) {
	if !r.Promoted || r.Retired {
		return Draft{}, fmt.Errorf("crystallize: pattern %s has not earned a draft (promoted=%t retired=%t)",
			r.Pattern.Action.Target, r.Promoted, r.Retired)
	}
	p := l.policy
	a := r.Pattern.Action

	level := domain.SafetyL2
	if a.Class == domain.ClassDestructive {
		level = domain.SafetyL3
	}
	ttl := r.GrantedTTL
	if ttl <= 0 || ttl > p.MaxTTL {
		ttl = p.MaxTTL
	}

	m := domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion,
		Kind:       domain.PluginKind,
		Metadata: domain.PluginMeta{
			Name:    r.Pattern.packageName(p.PackagePrefix),
			Version: p.Version,
			Vendor:  p.Vendor,
		},
		Spec: domain.PluginSpec{
			Targets:        domain.Targets{domain.TargetEdge},
			SafetyLevel:    level,
			Capabilities:   []domain.ToolClass{a.Class},
			Tools:          domain.Tools{{Name: a.Tool, Class: a.Class}},
			RequiredScopes: p.RequiredScopes,
			Audit:          domain.AuditPolicy{Emits: true, Mutates: false},
			Approval:       domain.ApprovalPolicy{Required: true, MaxBlastRadius: r.GrantedRadius},
			Install:        domain.InstallPolicy{Strategy: domain.InstallPin},
			Autonomy: domain.AutonomyPolicy{
				OfflineAfter: domain.Duration(p.OfflineAfter),
				Actions: []domain.AutonomyAction{{
					Name:           r.Pattern.actionName(),
					Tool:           a.Tool,
					Trigger:        a.Trigger,
					Argv:           append([]string(nil), a.Argv...),
					BlastRadius:    r.GrantedRadius,
					TTL:            domain.Duration(ttl),
					IdempotencyKey: fmt.Sprintf("crystallized:%s:{{%s}}:{{%s}}", slug(r.Pattern.Fault.Kind), domain.KeyTarget, domain.KeyWindow),
				}},
			},
		},
	}
	if err := pluginmanifest.Validate(m); err != nil {
		return Draft{}, fmt.Errorf("crystallize: the emitted draft would fail admission, which is a bug in this package: %w", err)
	}
	return Draft{Manifest: m, Run: r}, nil
}
