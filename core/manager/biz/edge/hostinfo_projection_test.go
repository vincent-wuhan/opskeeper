package edge

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// tunnel.HostInfo is a wire struct that grows: an agent gains a sensor, the
// column is added, and every consumer that translates it into a stored
// device fact starts shipping a zero value. Nothing about that is an error —
// a struct literal is not required to mention every field, and no compiler
// in this repository can see the translation from another package's struct.
//
// Two columns were permanently zero that way and nothing said so: os_version
// and disk_total_bytes. The port comment promised both were refreshed on
// every register, the device API returned both, and neither had a wire
// column to come from. The first test below is the evidence that they now
// arrive; the second is the guard that stops the next one from arriving
// silently.

// hostInfoConsumedElsewhere lists the HostInfo columns HandleRegister reads
// without copying them into a device row, each with the reason. Every other
// column has to arrive as a verbatim `info.<Column>` in one of the two
// literals, so this map is the complete list of columns where a reader has
// to take the code's word for something.
//
// It is keyed by column and matched on how the column is USED, not on whether
// its name appears in a literal: the seed writes `Fingerprint: fp`, and fp is
// a hash of Fingerprint and HardwareFingerprint rather than either of them.
// An earlier version of this guard matched on the key name, which made the
// two entries below dead weight and the "a reason is required" check
// unreachable — the guard read as documented while checking nothing.
//
// An entry without a reason fails, and an entry for a column HostInfo no
// longer has fails too: a stale exemption is how a guard quietly stops
// guarding.
var hostInfoConsumedElsewhere = map[string]string{
	"Fingerprint": "identity, not a fact: hashed into fp, which keys the row " +
		"through the seed literal, and replayed against the old row by " +
		"RebindFingerprint when the algorithm changed",
	"HardwareFingerprint": "identity, not a fact: deviceFingerprint prefers it " +
		"over Fingerprint, and fp is what gets stored",
}

func TestRegisterProjectsEveryHostInfoColumn(t *testing.T) {
	repo := newFakeRepo()
	devices := newFakeDeviceRepo()
	uc := NewUsecase(repo, devices, nil, nil)
	ctx := context.Background()

	res, err := uc.Create(ctx, "edge-facts", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	info := tunnel.HostInfo{
		Hostname:       "node-facts",
		OS:             "linux",
		OSVersion:      "22.04",
		Arch:           "arm64",
		KernelVersion:  "5.15.0-91-generic",
		CPUCount:       8,
		MemTotalBytes:  1 << 40,
		DiskTotalBytes: 500 << 30,
		IPAddress:      "10.0.0.9",
		Fingerprint:    "host-id-1",
	}
	if err := uc.HandleRegister(ctx, res.Edge.ID, info, ""); err != nil {
		t.Fatalf("HandleRegister: %v", err)
	}

	got := devices.lastFacts
	if got.OSVersion != info.OSVersion {
		t.Errorf("HostFacts.OSVersion = %q, want %q — the column is on the wire and in the port comment, so a zero here is a lost fact",
			got.OSVersion, info.OSVersion)
	}
	if got.DiskTotalBytes != info.DiskTotalBytes {
		t.Errorf("HostFacts.DiskTotalBytes = %d, want %d — the device list cannot render used/total without it",
			got.DiskTotalBytes, info.DiskTotalBytes)
	}
	if got.Hostname != info.Hostname || got.OS != info.OS || got.Arch != info.Arch ||
		got.KernelVersion != info.KernelVersion || got.CPUCount != info.CPUCount ||
		got.MemTotalBytes != info.MemTotalBytes || got.IPAddress != info.IPAddress {
		t.Errorf("HostFacts = %+v, want the remaining HostInfo columns of %+v", got, info)
	}
}

// TestHostInfoColumnsAreAccountedFor is the guard: every exported column of
// tunnel.HostInfo is either written by one of the two literals in usecase.go
// or exempted above with a reason. Adding a column to the wire struct fails
// this test until somebody decides where it lands, which is the decision
// that was skipped when os_version and disk_total_bytes were added.
func TestHostInfoColumnsAreAccountedFor(t *testing.T) {
	projected := projectedHostInfoColumns(t)

	typ := reflect.TypeOf(tunnel.HostInfo{})
	var missing []string
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		seen[field.Name] = true
		if projected[field.Name] {
			continue
		}
		reason, ok := hostInfoConsumedElsewhere[field.Name]
		if !ok {
			missing = append(missing, field.Name)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("tunnel.HostInfo.%s is listed as consumed elsewhere with no reason; "+
				"a column that is neither copied nor explained is the defect this guard exists for",
				field.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("tunnel.HostInfo columns HandleRegister neither copies nor explains: %v\n"+
			"each one needs a decision: project it as info.<Column> into devicemodel.Device / "+
			"devicebiz.HostFacts in usecase.go, or add it to hostInfoConsumedElsewhere with the "+
			"reason it is read somewhere else", missing)
	}

	var stale []string
	for name := range hostInfoConsumedElsewhere {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("hostInfoConsumedElsewhere names columns tunnel.HostInfo no longer has: %v — "+
			"a stale entry is a guard that reads as coverage and is not", stale)
	}
}

// projectedHostInfoColumns returns the field names written by the Device and
// HostFacts composite literals in usecase.go. It reads the source rather than
// calling Register because the alternative — asserting the fields the current
// code happens to set — is the thing that already let two columns through.
func projectedHostInfoColumns(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "usecase.go", nil, 0)
	if err != nil {
		t.Fatalf("parse usecase.go: %v", err)
	}
	// receiver is the identifier HandleRegister binds tunnel.HostInfo to. It
	// is read out of the signature rather than hardcoded, so renaming the
	// parameter cannot turn every projection into "derived".
	receiver := hostInfoParam(t, file)

	out := map[string]bool{}
	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || (pkg.Name != "devicemodel" && pkg.Name != "devicebiz") {
			return true
		}
		switch sel.Sel.Name {
		case "Device", "HostFacts":
		default:
			return true
		}
		found++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			// A column counts as projected only when its value IS the
			// source column. `Fingerprint: fp` sets the same key with a
			// different value, and treating that as coverage is how this
			// guard first shipped checking nothing.
			if sel, ok := kv.Value.(*ast.SelectorExpr); ok {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == receiver {
					out[key.Name] = true
				}
			}
		}
		return true
	})
	if found != 2 {
		t.Fatalf("found %d Device/HostFacts literals in usecase.go, want 2 — the guard is "+
			"reading a shape that moved and would pass while checking nothing", found)
	}
	return out
}

// hostInfoParam returns the name HandleRegister's tunnel.HostInfo parameter
// is bound to.
func hostInfoParam(t *testing.T, file *ast.File) string {
	t.Helper()
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Type.Params == nil {
			continue
		}
		for _, field := range fd.Type.Params.List {
			sel, ok := field.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HostInfo" {
				continue
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "tunnel" {
				continue
			}
			for _, name := range field.Names {
				if name.Name != "_" {
					return name.Name
				}
			}
		}
	}
	t.Fatal("no function takes a tunnel.HostInfo parameter — HandleRegister moved and this " +
		"guard would otherwise pass while checking nothing")
	return ""
}
