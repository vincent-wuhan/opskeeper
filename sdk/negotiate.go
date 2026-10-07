package sdk

import (
	"errors"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The build-time half of the compatibility matrix.
//
// The node checks a package's declared requirements when it admits the
// package (admitPackages over pluginmanifest), and that check is the one
// that decides. This file exists because the node's check is the *first*
// time an author finds out: without a way to run the same arithmetic at
// build time, a plugin whose min_pig_version is too high — or misspelled,
// or written as "0.8.0-rc1" — passes review, ships, and is refused on
// somebody's fleet at the moment an operator is relying on it.
//
// The comparison lives in core/domain so this file and the node cannot
// disagree. What is here is the author's entry point and the manifest
// fields it reads; the arithmetic is one function away in both callers.

// Host is what a node can state about itself.
//
// It is the SDK's name for the same two values domain.HostComponents
// carries. Two typed names for one shape is a cost; the benefit is that a
// plugin author writes sdk.Host{Pig: "0.87.1"} rather than reaching into a
// domain type whose name means nothing to them.
type Host struct {
	// Edge is the node agent's version, e.g. "0.8.0". Empty means the
	// caller does not know it.
	Edge string
	// Pig is the PiG agent binary's version, e.g. "0.87.1". Empty means
	// the caller does not know it.
	Pig string
}

// NegotiationError is a package that cannot be hosted, or a check that
// could not be run.
//
// It carries the fields rather than only a sentence because the author's
// fix depends on which field is wrong: a bad min_edge_version is a YAML
// edit, and an unknown node version is not the author's problem at all.
type NegotiationError struct {
	// Field is the manifest path of the offending value, e.g.
	// "spec.install.min_pig_version".
	Field string
	// Reason is what is wrong, in the author's terms.
	Reason string
	// Err is the underlying domain error, when there is one.
	Err error
}

func (e *NegotiationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

func (e *NegotiationError) Unwrap() error { return e.Err }

// Negotiate checks a manifest's compatibility requirements against a host.
//
// Both axes are checked and both are reported: an author fixing one and
// rediscovering the other on the next run has been made to iterate for no
// reason. That is the one place this differs from the node's check, which
// stops at the first failure because an operator only needs to know what to
// upgrade.
//
// A host field left empty is NOT a refusal here. It would be the wrong
// answer for a build: `sdk check` on a developer machine has no pig binary
// to ask, and refusing every package because the local machine cannot state
// its own agent version would make the command useless exactly where it is
// most used. The node is the one that has to refuse that case, because the
// node is the one that will run the code.
func Negotiate(m domain.PluginManifest, host Host) error {
	var failures []error

	edge := domain.VersionRequirement{
		Axis: domain.AxisEdge,
		Min:  m.Spec.Install.MinEdgeVersion,
		Host: host.Edge,
	}
	if err := negotiateAxis("spec.install.min_edge_version", edge, host.Edge); err != nil {
		failures = append(failures, err)
	}
	pig := domain.VersionRequirement{
		Axis: domain.AxisPig,
		Min:  m.Spec.Install.MinPigVersion,
		Host: host.Pig,
	}
	if err := negotiateAxis("spec.install.min_pig_version", pig, host.Pig); err != nil {
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		return nil
	}
	return errors.Join(failures...)
}

// negotiateAxis runs one requirement and renders the failure in the
// author's terms.
//
// hostUnset short-circuits: the requirement itself must still parse, so a
// typo is caught regardless, but a comparison against an unknown host is
// skipped rather than failed. The parse check is run separately for exactly
// that reason — it is the half that is always answerable.
func negotiateAxis(field string, req domain.VersionRequirement, hostUnset string) error {
	if req.Min == "" {
		return nil
	}
	if _, err := domain.ParseVersion(req.Min); err != nil {
		return &NegotiationError{
			Field:  field,
			Reason: fmt.Sprintf("%q is not a version this fleet compares; write a dotted number like \"0.87.1\"", req.Min),
			Err:    err,
		}
	}
	if hostUnset == "" {
		// The caller cannot say what the host runs, so the comparison is
		// skipped. See Negotiate's doc comment for why this is not a
		// refusal in the build-time check.
		return nil
	}
	ok, err := req.Satisfied()
	if ok {
		return nil
	}
	var tooOld *domain.VersionTooOldError
	if errors.As(err, &tooOld) {
		return &NegotiationError{
			Field:  field,
			Reason: fmt.Sprintf("this host runs %s %s, which is older than the required %s", req.Axis, tooOld.Host, tooOld.Min),
			Err:    err,
		}
	}
	return &NegotiationError{Field: field, Reason: err.Error(), Err: err}
}

// RequireVersion is the one-line guard a plugin author puts at build time.
//
// It returns the same error Negotiate does, and exists so the common case
// reads as a sentence rather than as a struct literal. A plugin's main or
// its init is the intended home:
//
//	if err := sdk.RequireVersion(manifest, sdk.HostFromEnv()); err != nil {
//		log.Fatal(err)
//	}
func RequireVersion(m domain.PluginManifest, host Host) error {
	if err := Negotiate(m, host); err != nil {
		return fmt.Errorf("pig-ops.yaml is not compatible with this host: %w", err)
	}
	return nil
}

// HostVersionEnv names the environment variables a plugin author uses to
// tell the SDK what their target host runs.
//
// They are the same variables the node reads to report its own versions
// (OPSKEEPER_EDGE_VERSION at the edge, OPSKEEPER_EDGE_PIG_VERSION for the
// agent), reused rather than invented so an author who can already see one
// of them in a bug report does not have to learn a second spelling.
const (
	// EnvEdgeVersion is the node agent's version.
	EnvEdgeVersion = "OPSKEEPER_EDGE_VERSION"
	// EnvPigVersion is the PiG agent binary's version.
	EnvPigVersion = "OPSKEEPER_EDGE_PIG_VERSION"
)
