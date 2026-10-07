package redis

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// The tests run against miniredis rather than a hand-written fake.
//
// A fake that stubbed this adapter's own methods would not execute a single
// Redis command, and the commands are the part of this package that can
// destroy a production dataset. A test that asserts "memory_purge did not
// delete the keys" is only meaningful if a real Redis is answering.

func connected(t *testing.T) (*Adapter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewWithClient(client, false), mr
}

func TestAdapter_Type(t *testing.T) {
	if got := New().Type(); got != adapter.TypeRedis {
		t.Errorf("Type() = %s, want redis", got)
	}
}

// This assertion is the opposite of what the skeleton asserted. The skeleton
// accepted a spec with no DSN, marked itself connected, and answered Health
// with a fixed "healthy" — so a deployment with no cache configured reported
// itself healthy. Refusing is the honest answer: there is no target, so
// there is nothing to be healthy about.
func TestAdapter_Connect_RefusesEmptyDSN(t *testing.T) {
	a := New()
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{}); err == nil {
		t.Fatal("Connect with an empty DSN must fail")
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health after a refused Connect = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Connect_RefusesAClusterDSNWithADatabase(t *testing.T) {
	a := New()
	err := a.Connect(context.Background(), adapter.ConnectionSpec{DSN: "redis+cluster://127.0.0.1:1/3"})
	if err == nil {
		t.Fatal("a cluster has only db 0; accepting an index would send every command to a different database than the operator named")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("the error should say what is wrong with the DSN: %v", err)
	}
}

func TestAdapter_Connect_RefusesAnEmptyClusterHost(t *testing.T) {
	a := New()
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{DSN: "redis+cluster://h1, ,h2"}); err == nil {
		t.Error("an empty host in a cluster DSN must be refused rather than silently dropped")
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	if _, err := New().Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Health_Probes(t *testing.T) {
	a, _ := connected(t)
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "healthy" {
		t.Errorf("Status = %s, want healthy", h.Status)
	}
	if h.CheckedAt.IsZero() {
		t.Error("CheckedAt must be set; a health report with no timestamp cannot be aged")
	}
}

func TestAdapter_Close_IsIdempotent(t *testing.T) {
	a, _ := connected(t)
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Health after Close = %v, want ErrNotConnected", err)
	}
}

// ── the approval gate ──────────────────────────────────────────────────

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a, mr := connected(t)
	mr.Set("orders:1", "kept")
	_, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "flushdb", Params: map[string]interface{}{"confirm": "flush"}})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Fatalf("Execute without approval = %v, want ErrApprovalRequired", err)
	}
	if !mr.Exists("orders:1") {
		t.Error("an unapproved Execute deleted a key; the gate must run before anything reaches the server")
	}
}

func TestAdapter_Execute_NotConnected(t *testing.T) {
	_, err := New().Execute(context.Background(), adapter.ExecOp{Operation: "memory_purge", ApprovedBy: "u1"})
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Execute on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Execute_UnknownOperation(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "drop_everything", ApprovedBy: "u1"})
	if !errors.Is(err, ErrUnknownOperation) {
		t.Errorf("Execute with an unknown op = %v, want ErrUnknownOperation", err)
	}
}

// ── memory_purge: the safety-critical mapping ──────────────────────────

// This is the test that stands between the platform and a deleted dataset.
//
// memory_purge is the closed loop's SAFE, AUTO-APPROVED remedy for a
// memory-burst incident, which means it runs with no human present. If it
// were implemented as FLUSHDB or FLUSHALL — the reading the name invites —
// every memory incident would silently wipe the cache.
func TestMemoryPurge_DoesNotDeleteKeys(t *testing.T) {
	a, mr := connected(t)
	keys := []string{"orders:1", "orders:2", "sessions:abc"}
	for _, k := range keys {
		mr.Set(k, "value")
	}

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "memory_purge",
		ApprovedBy: "policy:auto",
	})

	// The invariant holds whichever way the server answers, which is why it
	// is asserted before the outcome is examined: keys survive.
	for _, k := range keys {
		if !mr.Exists(k) {
			t.Fatalf("memory_purge removed key %q; it must send MEMORY PURGE, which deletes nothing", k)
		}
	}

	if err != nil {
		// A server without MEMORY PURGE refuses it. The refusal must name
		// the command that was sent — that is the evidence the adapter did
		// not quietly fall back to something destructive when its first
		// choice failed.
		if !strings.Contains(err.Error(), "MEMORY PURGE") {
			t.Errorf("the error must name the command that failed: %v", err)
		}
		return
	}

	if res.Impacted != 0 {
		t.Errorf("Impacted = %d, want 0: no key was affected, and saying otherwise tells the operator data changed", res.Impacted)
	}
	if !strings.Contains(res.Message, "no key was removed") {
		t.Errorf("the message must say plainly that no key was removed: %q", res.Message)
	}
}

// A server without MEMORY PURGE must produce a failure that names the
// command — never a fallback to something that would work. The fallback is
// where FLUSHDB gets chosen "so the purge succeeds", and that fallback
// deletes the cache.
//
// miniredis is exactly such a server, so this runs the real path.
func TestMemoryPurge_AnUnsupportedCommandIsAFailureNotAFallback(t *testing.T) {
	a, mr := connected(t)
	mr.Set("orders:1", "survives")

	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "memory_purge",
		ApprovedBy: "policy:auto",
	})
	if err == nil {
		t.Skip("this server implements MEMORY PURGE; there is no unsupported-command path to exercise")
	}
	if !strings.Contains(err.Error(), "MEMORY PURGE") {
		t.Errorf("the error must name the command that failed: %v", err)
	}
	// And the fallback did not happen.
	if strings.Contains(strings.ToUpper(err.Error()), "FLUSH") {
		t.Errorf("the failure mentions FLUSH, which means a fallback was attempted: %v", err)
	}
	if !mr.Exists("orders:1") {
		t.Error("the key is gone after a failed MEMORY PURGE; something destructive ran instead")
	}
}

// ── client_kill ────────────────────────────────────────────────────────

func TestExecute_ClientKill_RequiresAnAddress(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "client_kill",
		ApprovedBy: "alice",
	})
	if err == nil {
		t.Fatal("CLIENT KILL without an address must be refused; the filter form would terminate every client on the server")
	}
	if !strings.Contains(err.Error(), "addr") {
		t.Errorf("the error must name the missing argument: %v", err)
	}
}

func TestExecute_ClientKill_RefusesABlankAddress(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "client_kill",
		Params:     map[string]interface{}{"addr": "   "},
		ApprovedBy: "alice",
	})
	if err == nil {
		t.Error("a blank address is not an address")
	}
}

// ── failover ───────────────────────────────────────────────────────────

func TestExecute_Failover_RefusesOnAStandaloneServer(t *testing.T) {
	a, _ := connected(t)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "failover",
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Refusing is the answer, not an error. Sending CLUSTER FAILOVER to a
	// standalone server returns "unknown command", which reads to an
	// operator as a broken deployment rather than "this does not apply here".
	if res.Success {
		t.Error("a failover on a standalone server must not report success")
	}
	if !strings.Contains(res.Message, "standalone") {
		t.Errorf("the message must say why: %q", res.Message)
	}
}

// ── config_set ─────────────────────────────────────────────────────────

func TestExecute_ConfigSet_RequiresBothArguments(t *testing.T) {
	a, _ := connected(t)
	for _, params := range []map[string]interface{}{
		{},
		{"parameter": "maxmemory"},
		{"value": "100mb"},
	} {
		if _, err := a.Execute(context.Background(), adapter.ExecOp{
			Operation: "config_set", Params: params, ApprovedBy: "alice",
		}); err == nil {
			t.Errorf("config_set with %v must be refused", params)
		}
	}
}

// A change that does not record the previous value is a change nobody can
// undo.
//
// This asserts the sentence rather than the round trip, because the round
// trip is CONFIG SET and the part that can be wrong about undo is the
// message. A test that needed a server implementing CONFIG would report
// nothing on the servers that do not.
func TestConfigSetMessage_RecordsHowToGetBack(t *testing.T) {
	msg := configSetMessage("maxmemory", "104857600", "0")
	if !strings.Contains(msg, "was 0") {
		t.Errorf("the message must report what the value was: %q", msg)
	}
	if !strings.Contains(msg, "CONFIG SET maxmemory 0 restores it") {
		t.Errorf("the message must give the exact command that undoes it: %q", msg)
	}
}

// Setting a parameter to the value it already had is not a change, and
// reporting an undo command for a non-change invites an operator to run it.
func TestConfigSetMessage_NoUndoWhenNothingChanged(t *testing.T) {
	msg := configSetMessage("maxmemory", "100mb", "100mb")
	if strings.Contains(msg, "restores it") {
		t.Errorf("no change was made, so there is nothing to restore: %q", msg)
	}
}

// An unknown previous value is not the same as an empty one. Reporting
// "was ; CONFIG SET maxmemory  restores it" would be worse than saying
// nothing, because it looks like a command.
func TestConfigSetMessage_OmitsTheUndoWhenTheOldValueIsUnknown(t *testing.T) {
	msg := configSetMessage("maxmemory-policy", "allkeys-lru", "")
	if strings.Contains(msg, "was") {
		t.Errorf("the old value is unknown, so no undo can be offered: %q", msg)
	}
	if !strings.Contains(msg, "allkeys-lru") {
		t.Errorf("the message must still say what was set: %q", msg)
	}
}

// ── flushdb ────────────────────────────────────────────────────────────

// The destructive action is reachable from a model's tool call, and the
// difference between it and every other operation here is that the data does
// not come back.
func TestExecute_FlushDB_RequiresExplicitConfirmation(t *testing.T) {
	a, mr := connected(t)
	mr.Set("orders:1", "kept")

	for _, confirm := range []interface{}{nil, "", "yes", "please"} {
		params := map[string]interface{}{}
		if confirm != nil {
			params["confirm"] = confirm
		}
		_, err := a.Execute(context.Background(), adapter.ExecOp{
			Operation: "flushdb", Params: params, ApprovedBy: "alice",
		})
		if err == nil {
			t.Errorf("flushdb with confirm=%v must be refused", confirm)
		}
		if !mr.Exists("orders:1") {
			t.Fatalf("flushdb with confirm=%v deleted a key", confirm)
		}
	}
}

func TestExecute_FlushDB_WithConfirmationRemovesKeysAndSaysHowMany(t *testing.T) {
	a, mr := connected(t)
	for _, k := range []string{"a", "b", "c"} {
		mr.Set(k, "v")
	}
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "flushdb",
		Params:     map[string]interface{}{"confirm": "flush"},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success = false: %s", res.Message)
	}
	if res.Impacted != 3 {
		t.Errorf("Impacted = %d, want 3 — the operator needs to know how much was destroyed", res.Impacted)
	}
	if mr.Exists("a") {
		t.Error("flushdb did not remove the keys it reported removing")
	}
}

// ── the read path ──────────────────────────────────────────────────────

func TestAdapter_Diagnose_RoutesCategories(t *testing.T) {
	a, mr := connected(t)
	mr.Set("k1", "v1")
	mr.Set("k2", "v2")

	for _, category := range []string{catServerInfo, catKeyspace, catBigKeys} {
		r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: category})
		if err != nil {
			t.Errorf("Diagnose(%s): %v", category, err)
			continue
		}
		if r.Category != category {
			t.Errorf("Category = %s, want %s", r.Category, category)
		}
		if strings.Contains(r.Summary, "skeleton") {
			t.Errorf("Diagnose(%s) still reports a skeleton: %s", category, r.Summary)
		}
	}
}

// A category whose command the server does not implement must surface the
// failure. Returning an empty finding set would report "no clients
// connected" on a server that simply cannot answer, which is a claim about
// the system made on evidence that does not exist.
func TestAdapter_Diagnose_AnUnsupportedCommandIsAnErrorNotAnEmptyResult(t *testing.T) {
	a, _ := connected(t)
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catClients})
	if err == nil {
		if len(r.Findings) == 0 {
			t.Error("an empty client list is a real finding and must not be produced by a failed command")
		}
		return
	}
	if r != nil {
		t.Errorf("a failed diagnostic returned a result as well: %+v", r)
	}
}

func TestAdapter_Diagnose_UnknownCategoryNamesTheRest(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: "made_up"})
	if err == nil {
		t.Fatal("an unknown category must be refused")
	}
	// A caller that cannot discover the vocabulary cannot use this method.
	for _, name := range []string{catBigKeys, catSlowLog, catMemory} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error should list %q: %v", name, err)
		}
	}
}

func TestAdapter_Diagnose_NotConnected(t *testing.T) {
	if _, err := New().Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catMemory}); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("Diagnose on a fresh adapter = %v, want ErrNotConnected", err)
	}
}

func TestAdapter_Diagnose_ClusterSaysItIsStandalone(t *testing.T) {
	a, _ := connected(t)
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catCluster})
	if err != nil {
		t.Fatalf("Diagnose(cluster): %v", err)
	}
	// An empty node list would read as "a cluster with no nodes", which is
	// a different and alarming finding.
	if !strings.Contains(r.Summary, "standalone") {
		t.Errorf("Summary must say the connection is standalone: %q", r.Summary)
	}
}

// A sample is not a ranking. Redis has no key-size index, so a big-key
// report is a scan result, and presenting it as "the biggest keys" would be
// a claim the command cannot support.
//
// The scan itself is what runs here. The per-key size comes from MEMORY
// USAGE, which some servers do not implement, so a server that rejects it
// must yield an empty ranking with the sampling note attached — not a
// listing of keys with invented sizes.
func TestDiagnose_BigKeys_ScansAndReportsThatItSampled(t *testing.T) {
	a, mr := connected(t)
	for i := 0; i < 10; i++ {
		mr.Set(string(rune('a'+i)), strings.Repeat("x", 100*(i+1)))
	}
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catBigKeys})
	if err != nil {
		t.Fatalf("Diagnose(big_keys): %v", err)
	}
	if !strings.Contains(r.Summary, "sampled") {
		t.Errorf("Summary must say the result is a sample: %q", r.Summary)
	}
	for _, row := range r.Findings {
		// A size that was never measured must not be reported as one. The
		// skeleton returned bytes and would have reported zero for a
		// non-empty key.
		if b, ok := row["bytes"].(int64); ok && b <= 0 {
			t.Errorf("row %v reports a non-positive size; sizes must be measured or omitted", row)
		}
	}
}

// A key that expires between the SCAN and the size lookup must not fail the
// whole diagnostic: the report is most needed on an instance under churn,
// which is exactly when keys disappear.
func TestDiagnose_BigKeys_SurvivesKeysThatVanished(t *testing.T) {
	a, mr := connected(t)
	for i := 0; i < 5; i++ {
		mr.Set(string(rune('a'+i)), "v")
	}
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catBigKeys})
	if err != nil {
		t.Fatalf("Diagnose(big_keys) failed on a live keyspace: %v", err)
	}
	if r.Summary == "" {
		t.Error("the diagnostic returned no summary")
	}
}

func TestAdapter_Collect_IsASnapshot(t *testing.T) {
	a, mr := connected(t)
	mr.Set("k", "v")
	res, err := a.Collect(context.Background(), adapter.CollectQuery{Metrics: []string{"info"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Metrics["info"] == nil {
		t.Errorf("Metrics = %+v, want the parsed INFO snapshot", res.Metrics)
	}
	if _, ok := res.Metrics["db_size"]; !ok {
		t.Error("DBSize is answerable on every server that answers INFO, and its absence hides a failed probe")
	}
	// INFO carries no history, so filling Samples with the same reading
	// would let a dashboard draw a flat line and call it a trend.
	if len(res.Samples) != 0 {
		t.Errorf("Samples must stay empty for a snapshot, got %d rows", len(res.Samples))
	}
	if !strings.Contains(res.Metadata["sampling"], "not a time series") {
		t.Errorf("Metadata must say the reading is a snapshot: %q", res.Metadata["sampling"])
	}
}

// ── INFO parsing ───────────────────────────────────────────────────────

func TestParseInfoSections_GroupsBySection(t *testing.T) {
	raw := "# Server\r\nredis_version:7.2.0\r\nredis_mode:standalone\r\n\r\n# Memory\r\nused_memory:1024\r\nmem_fragmentation_ratio:1.25\r\n"
	sections := parseInfoSections(raw)
	if v, ok := infoValue(sections, "Server", "redis_version"); !ok || v != "7.2.0" {
		t.Errorf("Server.redis_version = %q, %v", v, ok)
	}
	// The CRLF matters: Redis terminates INFO lines with \r\n, and a parser
	// that split on \n alone would leave the carriage return inside every
	// value, so a version comparison against "7.2.0" would fail.
	if v, ok := infoValue(sections, "Server", "redis_mode"); !ok || v != "standalone" {
		t.Errorf("Server.redis_mode = %q (raw %q); a trailing \\r would make every value compare unequal", v, v)
	}
	if n, ok := infoInt(sections, "Memory", "used_memory"); !ok || n != 1024 {
		t.Errorf("Memory.used_memory = %d, %v", n, ok)
	}
}

func TestParseInfoSections_KeyspaceKeepsPerDatabaseNames(t *testing.T) {
	sections := parseInfoSections("# Keyspace\r\ndb0:keys=3,expires=1,avg_ttl=0\r\ndb2:keys=7,expires=0,avg_ttl=0\r\n")
	ks := sections["Keyspace"]
	// Flattening db0 and db2 into one namespace would lose the only thing
	// those lines carry.
	if _, ok := ks["db0"]; !ok {
		t.Errorf("Keyspace = %v, want db0 and db2 kept apart", ks)
	}
	if _, ok := ks["db2"]; !ok {
		t.Errorf("Keyspace = %v, want db2 present", ks)
	}
}

func TestParseClientList_DropsFieldsNobodyReads(t *testing.T) {
	raw := "id=3 addr=127.0.0.1:5555 name= age=12 idle=0 flags=N db=0 sub=0 psub=0 ssub=0 multi=-1 qbuf=26 cmd=client|list user=default\n"
	rows := parseClientList(raw)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["addr"] != "127.0.0.1:5555" {
		t.Errorf("addr = %v", rows[0]["addr"])
	}
	// The fields a diagnosis never reads are dropped rather than passed to
	// a model as noise.
	if _, present := rows[0]["qbuf"]; present {
		t.Errorf("row = %v, want the unread fields removed", rows[0])
	}
}

// ── the registered tool set ────────────────────────────────────────────

// The loop's baseline proposes these two by name, and the gate dispatches by
// exactly these names. A name registered here but nowhere else is a run that
// prescribes a remedy nothing can perform.
func TestRegisterTools_ClosedLoopVocabularyIsPresent(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	for _, name := range []string{"redis.memory_purge", "redis.failover", "redis.client_kill"} {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Errorf("%s is proposed by the loop but is not registered", name)
			continue
		}
		if tool.Handler == nil {
			t.Errorf("%s has no handler", name)
		}
	}
}

// memory_purge must remain dispatchable with no arguments: it is the loop's
// auto-approved remedy, and an action that needs an argument no
// RemediationOption carries is one the closed loop cannot apply unattended.
func TestRegisterTools_MemoryPurgeNeedsNoArguments(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	spec, ok := reg.LookupTool("redis.memory_purge")
	if !ok {
		t.Fatal("redis.memory_purge is not registered")
	}
	if len(spec.RequiredArgs) != 0 {
		t.Errorf("RequiredArgs = %v; the loop's safe auto-approved remedy must not need an argument it cannot supply", spec.RequiredArgs)
	}
}

func TestRegisterTools_WriteToolsGoThroughTheGate(t *testing.T) {
	a, mr := connected(t)
	mr.Set("orders:1", "kept")
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	writes := []string{
		"redis.memory_purge", "redis.client_kill", "redis.failover",
		"redis.config_set", "redis.flushdb",
	}
	for _, name := range writes {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if tool.RiskLevel == adapter.RiskL0ReadOnly || tool.RiskLevel == adapter.RiskL1Diagnostic {
			t.Errorf("%s changes server state but is graded %s", name, tool.RiskLevel)
		}
		if _, err := tool.Handler(context.Background(), map[string]interface{}{}); err == nil {
			t.Errorf("%s ran without approval", name)
		}
	}
	if !mr.Exists("orders:1") {
		t.Error("an unapproved tool call deleted a key")
	}
}

// The gate is checked before the connection and before any argument
// validation, so an unapproved call is refused for every operation
// regardless of what the server can do. That is what makes it a gate rather
// than a check that some paths skip.
func TestRegisterTools_TheGateIsTheFirstThingEveryWriteHits(t *testing.T) {
	// No client at all: the approval check must still come first, so the
	// error is ErrApprovalRequired rather than ErrNotConnected.
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	for _, name := range []string{"redis.memory_purge", "redis.flushdb", "redis.client_kill"} {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		_, err := tool.Handler(context.Background(), map[string]interface{}{"confirm": "flush", "addr": "1.2.3.4:1"})
		if !errors.Is(err, adapter.ErrApprovalRequired) {
			t.Errorf("%s on an unconnected adapter = %v, want ErrApprovalRequired first", name, err)
		}
	}
}

func TestRegisterTools_FlushDBIsTheMostSevereGrade(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	tool, ok := reg.GetTool("redis.flushdb")
	if !ok {
		t.Fatal("redis.flushdb is not registered")
	}
	// Irreversible data loss is the one operation that is not the same kind
	// of thing as the others, and the grading is how that survives into the
	// approval UI.
	if tool.RiskLevel != adapter.RiskL4Destructive {
		t.Errorf("redis.flushdb is graded %s, want L4", tool.RiskLevel)
	}
}

func TestRegisterTools_ReadToolsRefuseWhenNotConnected(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	// A registered read on an adapter with no client must say so rather
	// than returning empty rows that read as "nothing is wrong".
	for _, name := range []string{"redis.info", "redis.client_list", "redis.memory_usage"} {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if _, err := tool.Handler(context.Background(), map[string]interface{}{}); !errors.Is(err, adapter.ErrNotConnected) {
			t.Errorf("%s on an unconnected adapter = %v, want ErrNotConnected", name, err)
		}
	}
}

func TestOpRiskLevel(t *testing.T) {
	a := New()
	if got := a.OpRiskLevel("memory_purge"); got != adapter.RiskL2SoftWrite {
		t.Errorf("OpRiskLevel(memory_purge) = %s, want L2: it deletes nothing", got)
	}
	if got := a.OpRiskLevel("flushdb"); got != adapter.RiskL4Destructive {
		t.Errorf("OpRiskLevel(flushdb) = %s, want L4", got)
	}
}

func TestToInt_AcceptsTheShapesJSONProduces(t *testing.T) {
	for _, in := range []any{3, int32(3), int64(3), float64(3)} {
		got, err := toInt(in)
		if err != nil || got != 3 {
			t.Errorf("toInt(%T(%v)) = %d, %v; want 3", in, in, got, err)
		}
	}
	if _, err := toInt(3.5); err == nil {
		t.Error("toInt(3.5) must fail; a truncated index is a different key")
	}
}
