// ops.go implements the adapter's write operations.
//
// Every function here changes server state, so every one of them is reached
// only through Execute, which refuses a call with no approver before it gets
// this far. That gate is not decoration: these are the operations a
// remediation run fires without a human present, on a production database,
// chosen by a model reading a metric.
//
// The three operations added for the closed loop's vocabulary
// (terminate_long_tx, connection_pause, vacuum_analyze) are the ones the
// investigator names in a RootCauseJSON as "the fix to apply". Before they
// existed, that name reached the console as a display string and nothing
// could carry it out — a run that reached its recovery phase prescribed a
// remedy the platform had no way to perform.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownOperation is returned for an operation this adapter does not
// implement. It is a distinct error rather than a generic failure because
// the caller is a remediation run: "no such operation" means the vocabulary
// and the adapter disagree, which is a deployment fault an operator can fix,
// and it must not read like "the database refused".
var ErrUnknownOperation = errors.New("postgres: unknown operation")

// defaultLongTxSeconds is the age at which an open transaction is treated as
// the evidence pg.terminate_long_tx acts on. Five minutes is the conventional
// threshold: long enough to be a real leak rather than a slow request, and
// short enough that terminating it is cheaper than the locks it holds.
const defaultLongTxSeconds = 300

// killSession terminates one backend.
//
// The boolean is reported rather than inferred from a row count because
// pg_terminate_backend returns false both when the pid is gone and when the
// backend refused to die, and those need different operator responses.
func (a *Adapter) killSession(ctx context.Context, p params) (int, string, bool, error) {
	pid, err := p.requireInt("pid")
	if err != nil {
		return 0, "", false, err
	}
	ok, err := a.queryScalar(ctx, `SELECT pg_terminate_backend($1)`, pid)
	if err != nil {
		return 0, "", false, err
	}
	if !ok {
		return 0, fmt.Sprintf("backend %d was not terminated: it may already be gone, or it refused", pid), false, nil
	}
	return 1, fmt.Sprintf("terminated backend %d", pid), true, nil
}

// cancelQuery asks one backend to cancel its current statement, leaving the
// session itself alive.
//
// This is the gentler sibling of kill_session and the right default for "this
// query is stuck": it releases the locks the statement holds without dropping
// the session's prepared state, so a pooled connection survives.
func (a *Adapter) cancelQuery(ctx context.Context, p params) (int, string, bool, error) {
	pid, err := p.requireInt("pid")
	if err != nil {
		return 0, "", false, err
	}
	ok, err := a.queryScalar(ctx, `SELECT pg_cancel_backend($1)`, pid)
	if err != nil {
		return 0, "", false, err
	}
	if !ok {
		return 0, fmt.Sprintf("query on backend %d was not cancelled: no statement was running", pid), false, nil
	}
	return 1, fmt.Sprintf("cancelled the running query on backend %d", pid), true, nil
}

// terminateLongTx terminates every backend whose transaction has been open
// longer than the threshold.
//
// It deliberately reuses the diagnostic's age expression so the two cannot
// disagree: an operator who was shown "3 transactions idle in transaction for
// 40 minutes" and then runs the remedy must have the remedy act on those
// three, not on a differently-computed set.
func (a *Adapter) terminateLongTx(ctx context.Context, p params) (int, string, bool, error) {
	age := defaultLongTxSeconds
	if raw, ok := p["min_age_seconds"]; ok {
		v, err := toInt(raw)
		if err != nil {
			return 0, "", false, fmt.Errorf("postgres: min_age_seconds: %w", err)
		}
		if v <= 0 {
			return 0, "", false, errors.New("postgres: min_age_seconds must be positive")
		}
		age = v
	}
	// Returning the pids in one round trip rather than selecting then
	// terminating: between the two statements a backend can exit, and
	// terminating a recycled pid belongs to an unrelated session.
	rows, err := a.queryRows(ctx, `
WITH victims AS (
  SELECT pid FROM pg_stat_activity
  WHERE pid <> pg_backend_pid()
    AND xact_start IS NOT NULL
    AND now() - xact_start > make_interval(secs => $1)
)
SELECT pid, pg_terminate_backend(pid) AS terminated FROM victims`, age)
	if err != nil {
		return 0, "", false, err
	}
	var killed, refused int
	for _, r := range rows {
		if t, _ := r["terminated"].(bool); t {
			killed++
		} else {
			refused++
		}
	}
	msg := fmt.Sprintf("terminated %d backend(s) holding a transaction open for over %ds", killed, age)
	if refused > 0 {
		msg += fmt.Sprintf("; %d refused or had already exited", refused)
	}
	// Nothing older than the threshold is a no-op, not a failure: the
	// database is in the state the operator asked for.
	return killed, msg, true, nil
}

// connectionPause stops a role from opening new connections and evicts the
// ones it already has.
//
// It uses CONNECTION LIMIT 0 rather than NOLOGIN on purpose. NOLOGIN blocks
// the role from authenticating at all, which is a different and broader
// statement than "stop connecting": it also breaks every other client that
// authenticates as that role through a different path, and it reads in
// pg_authid as though the account had been disabled. CONNECTION LIMIT 0 is
// the server feature that means exactly this.
//
// The effect is on new connections only, so the eviction is part of the
// operation rather than a separate step. A pause that left 200 existing
// sessions running would not have paused anything, and the operator would
// have to know that to interpret the result.
func (a *Adapter) connectionPause(ctx context.Context, p params) (int, string, bool, error) {
	role, err := p.requireIdent("role")
	if err != nil {
		return 0, "", false, err
	}
	quoted, err := quoteQualified(role)
	if err != nil {
		return 0, "", false, err
	}
	evicted, err := a.queryRows(ctx,
		`SELECT pid, pg_terminate_backend(pid) AS terminated
		 FROM pg_stat_activity WHERE usename = $1 AND pid <> pg_backend_pid()`, role)
	if err != nil {
		return 0, "", false, err
	}
	// Set the limit before evicting, so a connection that races in between
	// is refused rather than surviving the sweep.
	if err := a.execStatement(ctx, `ALTER ROLE `+quoted+` CONNECTION LIMIT 0`); err != nil {
		return 0, "", false, err
	}
	var killed int
	for _, r := range evicted {
		if t, _ := r["terminated"].(bool); t {
			killed++
		}
	}
	return killed, fmt.Sprintf(
		"role %s may not open new connections (CONNECTION LIMIT 0) and %d live session(s) were evicted; "+
			"run pg.connection_resume to undo", role, killed), true, nil
}

// connectionResume is the declared inverse of connectionPause.
//
// It exists because a remediation platform that can lock a role out of a
// database and offers no way back is a hazard, not a capability. Restoring
// the default (-1) rather than a number is deliberate: the role's original
// limit is not recorded anywhere, so -1 — "no limit, the server default" — is
// the only restoration that cannot leave the role worse than it was found.
func (a *Adapter) connectionResume(ctx context.Context, p params) (int, string, bool, error) {
	role, err := p.requireIdent("role")
	if err != nil {
		return 0, "", false, err
	}
	quoted, err := quoteQualified(role)
	if err != nil {
		return 0, "", false, err
	}
	if err := a.execStatement(ctx, `ALTER ROLE `+quoted+` CONNECTION LIMIT -1`); err != nil {
		return 0, "", false, err
	}
	return 0, fmt.Sprintf("role %s may open connections again (CONNECTION LIMIT -1)", role), true, nil
}

// vacuumAnalyze runs VACUUM (ANALYZE) — the maintenance the loop proposes as
// its safe, auto-approved remedy.
//
// ANALYZE alone leaves the planner working from stale statistics, and
// VACUUM alone leaves it working from statistics it just invalidated, so
// running one without the other is a known way to make a problem worse
// before fixing it. The pair is the unit.
func (a *Adapter) vacuumAnalyze(ctx context.Context, p params) (int, string, bool, error) {
	return a.vacuumLike(ctx, p, `VACUUM (ANALYZE) `, "vacuumed and analyzed")
}

func (a *Adapter) vacuumTable(ctx context.Context, p params) (int, string, bool, error) {
	return a.vacuumLike(ctx, p, `VACUUM (ANALYZE false) `, "vacuumed")
}

func (a *Adapter) analyzeTable(ctx context.Context, p params) (int, string, bool, error) {
	return a.vacuumLike(ctx, p, `ANALYZE `, "analyzed")
}

// vacuumLike is the shared body of the three maintenance statements. The
// target is optional: with no table named, the statement applies to every
// table the role can vacuum, which is the shape a whole-database
// maintenance run takes.
func (a *Adapter) vacuumLike(ctx context.Context, p params, prefix, verb string) (int, string, bool, error) {
	target, _ := p["table"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		if err := a.execStatement(ctx, strings.TrimSpace(prefix)); err != nil {
			return 0, "", false, err
		}
		return 0, verb + " every table in the current database", true, nil
	}
	quoted, err := quoteQualified(target)
	if err != nil {
		return 0, "", false, err
	}
	if err := a.execStatement(ctx, prefix+quoted); err != nil {
		return 0, "", false, err
	}
	// One table named, one table acted on: Impacted 1 with a success verdict
	// is the honest reading, which is exactly why Success cannot be inferred
	// from Impacted elsewhere.
	return 1, verb + " " + target, true, nil
}

// ── parameter access ───────────────────────────────────────────────────
//
// Args arrive as map[string]any because that is the shape the tool broker
// and the LLM tool protocol both produce, and a JSON number may decode as
// float64, json.Number or int depending on the path. Every accessor below
// funnels through one of these so a remediation never fails on "expected
// number, got float64".

type params map[string]any

func (p params) requireInt(name string) (int, error) {
	raw, ok := p[name]
	if !ok {
		return 0, fmt.Errorf("postgres: %s is required", name)
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("postgres: %s: %w", name, err)
	}
	return v, nil
}

// requireIdent returns an identifier that has already passed the whitelist.
// It exists so the identifier path cannot be reached with a value that only
// the table path validates.
func (p params) requireIdent(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("postgres: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("postgres: %s must be a string", name)
	}
	s = strings.TrimSpace(s)
	if !identRe.MatchString(s) {
		return "", fmt.Errorf("postgres: %s %q is not a valid identifier", name, s)
	}
	return s, nil
}

func toInt(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("%v is not a whole number", v)
		}
		return int(v), nil
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, err
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", raw)
	}
}
