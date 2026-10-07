package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	aiopschatruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"
	managerbizreport "github.com/vincent-wuhan/opskeeper/core/manager/biz/report"
)

// This file is the boot-path guard for the `report -> aiops` cut.
//
// The cut lives in three files and none of them can see the wiring: biz/report
// narrowed its port to its own value types, cmd/opskeeper grew a translator,
// and the domaincheck table lost a row. What decided the cut was in main.go,
// and what it changed was invisible from both sides.
//
// Measured before the cut: NewWorkerGenerator was being handed `reportRT`, a
// *chatruntime.Runtime, as its WorkerSpawner. The port's signature was the
// only thing that made that a dependency, and it named two structs the report
// domain has no other reason to know: chatruntime.SpawnRequest (10+ fields,
// six read) and chatruntime.Worker (10+ fields, four read, two of them behind
// a mutex and a cancel func).
//
// So the boundary was package-shaped wearing an interface's clothes — the
// fifth time this repository has written that sentence, and the reason these
// tests exist rather than a comment. The port is now stated in the report
// domain's own words, the runtime no longer satisfies it, and the old wiring
// at main.go does not compile. The second test is the enforceable half: it
// names the two projections the translator has to carry, because that is the
// part no compiler checks.

// TestTheReportPortIsStatedInTheReportDomainsWords is the structural
// consequence in both directions.
//
// The negative half is the one that matters and the one that can actually go
// red: a type assertion, not a `var _` line. If the port is ever widened back
// to the chatruntime shapes, *chatruntime.Runtime satisfies it again and this
// test fails in a world where `go build ./...` is perfectly happy — the
// wiring would still compile, the domain would still have its own types, and
// the edge would quietly be back. A `var _ = (*Runtime)(nil)` negative
// assertion cannot do this: it would fail the build instead, and decision 252
// already recorded why "the compiler rejects it" is weaker evidence than it
// looks — it proves the mutation is illegal, not that a guard would have
// caught it.
func TestTheReportPortIsStatedInTheReportDomainsWords(t *testing.T) {
	// Positive: the translator main.go actually passes must satisfy the
	// port. If this stops compiling, the wiring at the generator is broken.
	var _ managerbizreport.ReporterRunner = reportRunner{}

	// Negative: the runtime itself must not satisfy it.
	if _, ok := any((*aiopschatruntime.Runtime)(nil)).(managerbizreport.ReporterRunner); ok {
		t.Error("*chatruntime.Runtime satisfies report.ReporterRunner again. The port is " +
			"back to naming the agent kernel's own structs, which means main.go can hand " +
			"the runtime straight to NewWorkerGenerator and nothing — not the compiler, " +
			"not the domain table, not the release report — will say that report once " +
			"again depends on a type it never asked for")
	}
}

// The projection is the part of this cut that no compiler checks.
//
// A translator that drops a field still compiles, still satisfies the port,
// and still runs: it just hands the agent kernel a zero value where the
// caller meant something. Six fields cross here today, and every one of them
// was read by the generator before the cut. The failure mode is silent — a
// report whose persona silently reverts to the routing default, or whose
// locale silently becomes the manager's instead of the operator's.
//
// So the check is textual, against the translator's own source, and it is
// exhaustive in the direction that matters: every field of the request is
// named, and the request is built with exactly the caller's value rather than
// a literal.
var reportRequestField = regexp.MustCompile(`(?m)^\s+(\w+):\s+req\.(\w+),$`)

func TestTheReportTranslatorCarriesEveryFieldTheRequestHas(t *testing.T) {
	fn := reportRunnerSource(t)

	got := map[string]string{}
	for _, m := range reportRequestField.FindAllStringSubmatch(fn, -1) {
		got[m[1]] = m[2]
	}

	// Every field the report domain's request carries. Read this list against
	// the struct: a field added to ReporterRequest that is missing here is a
	// value that will be silently dropped in production.
	for _, field := range []string{"AgentName", "Prompt", "SessionKind", "OwnerUserID", "Locale"} {
		gotField, ok := got[field]
		if !ok {
			t.Errorf("reportRunner.RunReporter does not set SpawnRequest.%s. It is a field of "+
				"report.ReporterRequest and the generator reads it; a zero value here compiles, "+
				"wires and runs, and fails silently", field)
			continue
		}
		if gotField != field {
			t.Errorf("reportRunner.RunReporter sets SpawnRequest.%s from req.%s — the translator "+
				"is swapping two fields of the same shape, which is the kind of mistake that "+
				"survives every test in the domain", field, gotField)
		}
	}

	// Background is the one field the request deliberately does not carry,
	// and the one that must not be left to whatever the zero value means.
	// Both callers share the same contract: the caller owns the report row's
	// lifecycle and has to choose a terminal state before it can flip it, so
	// a background spawn would return an empty result and the row would be
	// failed on a truth rather than on a cause.
	if !strings.Contains(fn, "Background:  false,") {
		t.Error("reportRunner.RunReporter no longer hard-codes Background: false. If it is now " +
			"true, the generator blocks on nothing and writes a failed report on an empty result; " +
			"if it was dropped, the zero value made that decision instead of this line")
	}

	// The nil-worker guard moved here with the cut, and it is the only place
	// it can be reached from: a value return cannot smuggle a (nil, nil)
	// past the generator any more, so the runtime handing one back has to
	// become an error or the report is written from an empty outcome.
	if !strings.Contains(fn, "if worker == nil {") {
		t.Error("reportRunner.RunReporter no longer turns a nil worker into an error. " +
			"The generator used to do this, and it was the one place in the tree that could " +
			"produce the value; with a value return the check has to live here or the empty " +
			"outcome reads as success")
	}
}

// The outcome is the same trap from the other side, and the first mutation
// round proved it: swapping SessionID and WorkerID in the translator compiles,
// wires, runs, and passes every test in this file, because the request
// direction was guarded and the outcome direction was not. The two fields are
// both strings, so even a type checker has nothing to say.
//
// What it costs in production is worse than a wrong number. SessionID goes
// onto the report row as AuditSessionID — the id an operator follows to read
// the run that produced the report — and WorkerID is the id a stop call takes.
// Swapped, every generated report points its audit trail at a worker id that
// is not a session, and nothing in the system complains: the column is
// non-null either way, the transcript it names exists, and the row is merely
// wrong in a way that only shows up when someone tries to use it.
var reportOutcomeField = regexp.MustCompile(`(?m)^\s+(\w+):\s+worker\.(\w+),$`)

func TestTheReportTranslatorDoesNotSwapTheOutcomeFields(t *testing.T) {
	fn := reportRunnerSource(t)

	// The map is spelled out rather than derived, and the first version of
	// this test derived it — it assumed the projection keeps the producer's
	// field names. It does not: `Worker` calls it ID, the outcome calls it
	// WorkerID, because "the id of a worker" is a more useful name in a file
	// that also carries a SessionID. The derived version reported that
	// correct line as a mismatch, and a guard that cries wolf on the
	// right answer trains its reader to skip it.
	//
	// Which matters more than it sounds: this file exists because a mutation
	// went unnoticed, and a guard that goes red for the wrong reason is the
	// same failure in the other direction — it costs a real signal to buy a
	// false one.
	want := map[string]string{
		"SessionID": "SessionID",
		"WorkerID":  "ID",
		"Result":    "Result",
		"Err":       "Err",
	}
	got := map[string]string{}
	for _, m := range reportOutcomeField.FindAllStringSubmatch(fn, -1) {
		got[m[1]] = m[2]
	}
	for field, producer := range want {
		gotProducer, ok := got[field]
		if !ok {
			t.Errorf("reportRunner.RunReporter does not build ReporterOutcome.%s. The generator "+
				"reads it — SessionID and WorkerID are written onto the report row", field)
			continue
		}
		if gotProducer != producer {
			t.Errorf("reportRunner.RunReporter builds ReporterOutcome.%s from worker.%s, want "+
				"worker.%s. Both are strings, so nothing but this test can see it: the report row "+
				"would carry a worker id in the column an operator follows to read the transcript "+
				"that produced it", field, gotProducer, producer)
		}
	}
}

// reportRunnerSource returns the body of reportRunner.RunReporter as it is
// written in main.go, so the two field guards above read the translator rather
// than a paraphrase of it.
func reportRunnerSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (r reportRunner) RunReporter(")
	if start < 0 {
		t.Fatal("reportRunner.RunReporter is gone from main.go. If the report domain is back on " +
			"the agent kernel's structs, this file is the only place that can still say so")
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of reportRunner.RunReporter")
	}
	return rest[:end]
}

// The wiring itself.
//
// Decision 249 measured that report's own seam tests could not catch a boot
// path that forgot to wire: a handler with no importer still serves 404 and
// every test in its domain is green. The same shape is available here —
// NewWorkerGenerator panics on a nil runner, so a forgotten wiring shows up
// as a crash on the first report rather than as a failing test, and a report
// domain test cannot see it because the domain test constructs its own fake.
//
// So this one reads main.go and checks the argument. It is the third place in
// this repository that has needed such a test, which is a fact about
// constructors taking interfaces as arguments: they are not the same thing as
// the wiring existing.
func TestTheReportGeneratorIsWiredThroughTheTranslator(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	call := strings.Index(body, "managerbizreport.NewWorkerGenerator(")
	if call < 0 {
		t.Fatal("NewWorkerGenerator is not called from main.go. The report API is mounted " +
			"even when the LLM runtime is unavailable, so this is not a compile error — it is " +
			"a report generator that will not exist at runtime")
	}
	rest := body[call:]
	// The argument list ends at the closing paren of the call.
	end := strings.Index(rest, "\n\t\t\tlog,")
	if end < 0 {
		end = strings.Index(rest, ")\n")
	}
	if end < 0 {
		t.Fatal("could not find the end of the NewWorkerGenerator call")
	}
	args := rest[:end]

	if !strings.Contains(args, "reportRunner{rt: reportRT}") {
		t.Errorf("NewWorkerGenerator is not handed reportRunner{rt: reportRT}; the call is:\n%s",
			args)
		return
	}
	if strings.Contains(args, "\n\t\t\treportRT,") {
		t.Error("NewWorkerGenerator is being handed reportRT directly again, alongside the " +
			"translator. That compiles only if the port has been widened back to the agent " +
			"kernel's structs, and it would mean the domain table and this cut are both stale")
	}
}
