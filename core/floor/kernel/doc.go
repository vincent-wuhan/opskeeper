// Package kernel is OpsKeeper's microkernel.
//
// The problem it solves is the one this repository measured: every
// integration — the Prometheus client, the Loki parser, the Postgres
// adapter, the LLM provider switch — is compiled into the binary and wired
// by hand in main.go. Adding a second backend of any kind means editing the
// core and rebuilding every deployment. The governance envelope
// (pig-ops.yaml) and the node-side install port (ports.PluginInstaller)
// already exist, but they sit between the node and the control plane; they
// do not sit between the core and its vendors.
//
// This package moves that boundary inward. It is deliberately small: a
// table of extension points, a registry that refuses to be widened
// silently, and a host that admits a package through the existing
// admission gate (sdk.Admit) before it is allowed to register anything.
//
// The kernel holds no vendor vocabulary. It does not know what Prometheus
// is, or Loki, or Postgres. It knows six extension points and two rules:
//
//  1. A plugin registers through the host, never directly. Every entry
//     records which package registered it, so a package can be rolled back
//     by name and an undeclared entry is visible as one.
//  2. Admission is the host's, and it fails closed. A package above the
//     target's safety ceiling, aimed at the wrong target, or asking for a
//     scope the operator did not grant is refused before a single entry is
//     written — and a registration that fails midway leaves the table
//     exactly as it was.
//
// The first-party implementations (the Prometheus datasource, the Postgres
// adapter) are preinstalled plugins: they go through this same path, so
// the third-party path is exercised on every boot rather than only in a
// test.
package kernel
