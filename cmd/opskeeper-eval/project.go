package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/judge"
	"github.com/vincent-wuhan/opskeeper/core/harness/projection"
)

// opskeeper-eval project — turn a production root-cause contract into the
// response the judge scores.
//
// This is the hop that was missing. The investigated phase writes a
// RootCauseJSON; `judge` scores a judge.AgentResponse; nothing connected
// them, so every score ever produced came from a hand-written file and the
// golden corpus was never held to anything the system actually produced.
//
// The three vocabularies that meet here do not all line up. Remediation
// actions are `<family>.<method>` in both, so they carry across. The root
// cause is a closed enum on the contract side (pg_lock) and a `<family>.
// <method>` symbol on the case side, so it needs a mapping a human has to
// declare — supplied as --kind-map rather than guessed in code, because
// deciding that pg_lock and pg.lock_waits are the same finding is a
// judgement about meaning that only a domain owner can make. Evidence tool
// names are bare and cross-family (query_promql), so they ride along as
// tool calls for the LLM judge to reason over and take no part in exact
// matching.
//
// A kind with no mapping is reported and, by default, makes the command
// fail. Writing a response with an empty root_cause_matched would be scored
// as a diagnosis that found nothing, and that reading is a fabrication.
type projectFlags struct {
	contractPath   string
	kindMapPath    string
	detectedAt     string
	investigatedAt string
	recoveredAt    string
	allowUnmapped  bool
	bare           bool
	out            string
}

func cmdProject(_ context.Context, args []string) error {
	var f projectFlags
	fs := flag.NewFlagSet("project", flag.ExitOnError)
	fs.StringVar(&f.contractPath, "contract", "", "RootCauseJSON 文档路径（必填）")
	fs.StringVar(&f.kindMapPath, "kind-map", "", "kind → case 词表符号的映射 JSON（可选）")
	fs.StringVar(&f.detectedAt, "detected-at", "", "告警时间 RFC3339")
	fs.StringVar(&f.investigatedAt, "investigated-at", "", "investigated 阶段完成时间 RFC3339")
	fs.StringVar(&f.recoveredAt, "recovered-at", "", "恢复验证时间 RFC3339")
	fs.BoolVar(&f.allowUnmapped, "allow-unmapped-root-cause", false,
		"kind 无映射时仍然输出（结果会标注 unmapped_root_cause）")
	fs.BoolVar(&f.bare, "bare", false,
		"只输出 judge.AgentResponse 本身（不带诊断外壳，可直接喂给 judge --response）")
	fs.StringVar(&f.out, "out", "", "AgentResponse JSON 输出路径（默认打印到 stdout）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.contractPath == "" {
		return errors.New("--contract required")
	}
	return runProject(f, os.Stdout)
}

func runProject(f projectFlags, out *os.File) error {
	raw, err := os.ReadFile(f.contractPath)
	if err != nil {
		return fmt.Errorf("read contract: %w", err)
	}
	var doc projection.Doc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// A contract with a field this build does not know is not a contract
	// this build can project. Decoding leniently would drop the field and
	// produce a response that looks complete while missing whatever that
	// field carried.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("parse %s as a RootCauseJSON contract: %w", f.contractPath, err)
	}

	timing, err := parseTiming(f)
	if err != nil {
		return err
	}
	resolve, err := loadKindMap(f.kindMapPath)
	if err != nil {
		return err
	}

	res, err := projection.FromContract(&doc, timing, resolve)
	if err != nil {
		return fmt.Errorf("project %s: %w", f.contractPath, err)
	}

	if res.UnmappedKind != "" {
		if !f.allowUnmapped {
			return fmt.Errorf("refusing to write a response with no root cause:\n"+
				"  the contract names its root cause as %q, which is not a symbol the judge\n"+
				"  can compare against a case. Supply --kind-map with an entry for it, or pass\n"+
				"  --allow-unmapped-root-cause to write it anyway (the artifact will say so).",
				res.UnmappedKind)
		}
		fmt.Fprintf(os.Stderr, "warning: %s\n", res.UnmappedReason)
	}

	// The envelope and the bare response answer different needs. The
	// envelope says how the response was derived and what could not be
	// carried across, which is what a person debugging a low score needs.
	// The bare response is the scoring input, and judge already decodes
	// judge.AgentResponse strictly — wrapping it would mean teaching judge
	// a second schema for the same type.
	var payload any = res.Response
	if !f.bare {
		payload = struct {
			Response          *judge.AgentResponse `json:"response"`
			ContractKind      string               `json:"contract_kind"`
			UnmappedRootCause string               `json:"unmapped_root_cause,omitempty"`
			UnmappedReason    string               `json:"unmapped_root_cause_reason,omitempty"`
			KindMapPath       string               `json:"kind_map,omitempty"`
		}{
			Response:          res.Response,
			ContractKind:      doc.RootCauseObject.Kind,
			UnmappedRootCause: res.UnmappedKind,
			UnmappedReason:    res.UnmappedReason,
			KindMapPath:       f.kindMapPath,
		}
	}
	out2, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if f.out == "" {
		fmt.Println(string(out2))
		return nil
	}
	if err := os.WriteFile(f.out, append(out2, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f.out, err)
	}
	fmt.Fprintf(os.Stderr, "project: kind=%s root_cause=%v remediations=%v tool_calls=%d → %s\n",
		doc.RootCauseObject.Kind, res.Response.RootCause, res.Response.Remediations,
		len(res.Response.ToolCalls), f.out)
	return nil
}

// loadKindMap reads the declared kind → case-symbol mapping.
//
// The file is the seam where a semantic judgement becomes reviewable: it is
// data someone can read, diff and sign off on, rather than a branch in code
// that nobody notices was a claim about meaning. A kind the file does not
// mention resolves to nothing, which FromContract reports by name — a
// silently-ignored entry would be indistinguishable from a typo.
func loadKindMap(path string) (projection.Resolver, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kind map: %w", err)
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s as {kind: [symbols]}: %w", path, err)
	}
	return func(kind string) ([]string, bool) {
		syms, ok := m[kind]
		if !ok || len(syms) == 0 {
			return nil, false
		}
		return syms, true
	}, nil
}

// parseTiming reads the three wall-clock marks. A mark given in a format
// that does not parse is an error rather than a zero: a mistyped timestamp
// that silently becomes "not observed" would score as a full mark on
// time_efficiency, which is the one place where a quiet default is most
// expensive.
func parseTiming(f projectFlags) (projection.Timing, error) {
	var out projection.Timing
	for _, spec := range []struct {
		flag string
		val  string
		dst  *time.Time
	}{
		{"--detected-at", f.detectedAt, &out.DetectedAt},
		{"--investigated-at", f.investigatedAt, &out.InvestigatedAt},
		{"--recovered-at", f.recoveredAt, &out.RecoveredAt},
	} {
		if spec.val == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, spec.val)
		if err != nil {
			return out, fmt.Errorf("%s %q: %w", spec.flag, spec.val, err)
		}
		*spec.dst = t
	}
	return out, nil
}
