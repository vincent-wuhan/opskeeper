// Command agentteams-matrix renders the canonical Worker permission
// matrix as JSON, validates it against the spec artifact, and exits
// non-zero on drift. Run via:
//
//	go run ./cmd/agentteams-matrix --check openspec/changes/agentteams-opskeeper-integration/specs/agentteams-integration-layer/worker-permission-matrix.json
//
// Without --check the command prints the rendered matrix to stdout
// for one-shot JSON regeneration.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
)

func main() {
	checkPath := flag.String("check", "", "path to spec JSON; when set, validate instead of print")
	flag.Parse()

	rows := auth.AgentTeamsWorkerPermissions()
	out := struct {
		SchemaVersion string                  `json:"schema_version"`
		Description   string                  `json:"description"`
		Matrix        []auth.WorkerPermission `json:"matrix"`
	}{
		SchemaVersion: "v1",
		Description:   "Canonical 7-role minimum permission matrix for AgentTeams opsKeeper integration. Single source of truth: internal/pkg/auth/jwt.go::AgentTeamsWorkerPermissions.",
		Matrix:        rows,
	}

	if *checkPath == "" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(&out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	raw, err := os.ReadFile(*checkPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read spec:", err)
		os.Exit(1)
	}
	var spec struct {
		SchemaVersion string                  `json:"schema_version"`
		Matrix        []auth.WorkerPermission `json:"matrix"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		fmt.Fprintln(os.Stderr, "parse spec:", err)
		os.Exit(1)
	}
	if spec.SchemaVersion != out.SchemaVersion {
		fmt.Fprintf(os.Stderr, "schema_version drift: code=%s spec=%s\n", out.SchemaVersion, spec.SchemaVersion)
		os.Exit(2)
	}
	if len(spec.Matrix) != len(out.Matrix) {
		fmt.Fprintf(os.Stderr, "matrix size drift: code=%d spec=%d\n", len(out.Matrix), len(spec.Matrix))
		os.Exit(2)
	}
	for i := range out.Matrix {
		a := out.Matrix[i]
		b := spec.Matrix[i]
		if a.Role != b.Role {
			fmt.Fprintf(os.Stderr, "row[%d] role drift: code=%s spec=%s\n", i, a.Role, b.Role)
			os.Exit(2)
		}
		if a.Worker != b.Worker {
			fmt.Fprintf(os.Stderr, "row[%d] worker drift: code=%s spec=%s\n", i, a.Worker, b.Worker)
			os.Exit(2)
		}
		if a.Mutating != b.Mutating {
			fmt.Fprintf(os.Stderr, "row[%d] mutating drift: code=%v spec=%v\n", i, a.Mutating, b.Mutating)
			os.Exit(2)
		}
		if a.ReadOnly != b.ReadOnly {
			fmt.Fprintf(os.Stderr, "row[%d] read_only drift: code=%v spec=%v\n", i, a.ReadOnly, b.ReadOnly)
			os.Exit(2)
		}
		if len(a.Tools) != len(b.Tools) {
			fmt.Fprintf(os.Stderr, "row[%d] tools size drift: code=%d spec=%d\n", i, len(a.Tools), len(b.Tools))
			os.Exit(2)
		}
		toolSet := map[string]bool{}
		for _, t := range a.Tools {
			toolSet[t] = true
		}
		for _, t := range b.Tools {
			if !toolSet[t] {
				fmt.Fprintf(os.Stderr, "row[%d] tool drift: spec has %q not in code\n", i, t)
				os.Exit(2)
			}
		}
	}
	fmt.Printf("agentteams-matrix: %d roles validated against %s\n", len(out.Matrix), *checkPath)
}
