// queries.go holds the SQL this adapter sends, and the two things that make
// it safe to send them: identifier validation and row decoding.
//
// The separation is deliberate. A query is a string, and a string that
// carries a caller-supplied table name is an injection site — the one class
// of bug in a database adapter that no amount of parameter binding covers,
// because identifiers cannot be bound. So every name that reaches SQL text
// goes through quoteQualified first, and every value goes through a bind
// parameter. There is no third path.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

// identRe is the shape of an unquoted PostgreSQL identifier this adapter
// will accept: a letter or underscore, then letters, digits, underscores,
// dollar signs or hyphens.
//
// It is a whitelist rather than a blacklist because a blacklist has to be
// right about everything an attacker can type, and this one only has to be
// right about what it refuses. A legitimate schema or table name that fails
// this is refused loudly at the call site, which is a five-second fix; a
// name that slips through is a dropped production database.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]*$`)

// quoteQualified validates each part of a possibly-qualified name and quotes
// it, so the result is safe to interpolate into DDL like VACUUM.
//
// It returns an error rather than quoting whatever it was given. Silently
// quoting an arbitrary string produces a valid identifier that names
// something the caller did not intend — the most confusing possible failure
// for a remediation action, because the operator approved "vacuum the orders
// table" and got "vacuum the \"orders; DROP DATABASE x\" table", which
// errors, or worse, matches a real table.
func quoteQualified(name string) (string, error) {
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("postgres: %q is not a table name: expected [schema.]table", name)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !identRe.MatchString(p) {
			return "", fmt.Errorf("postgres: %q is not a valid identifier part %q", name, p)
		}
		out = append(out, `"`+p+`"`)
	}
	return strings.Join(out, "."), nil
}

// rowsToMaps decodes a result set into the generic shape Diagnose and the
// tool handlers return.
//
// Values are normalised on the way out because the consumer is a model, not
// a Go program: a time arrives as RFC3339 rather than as Go's own layout, a
// []byte (which is how every driver returns json/jsonb) arrives decoded, and
// anything unrecognised arrives as its own text. A driver-specific type
// leaking into the findings would make the same query answer differently
// depending on which driver the operator configured.
func rowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("postgres: read columns: %w", err)
	}
	out := make([]map[string]any, 0, 16)
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("postgres: scan row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normaliseCell(cells[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate rows: %w", err)
	}
	return out, nil
}

// normaliseCell converts one driver value into something a model can read.
func normaliseCell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		// json/jsonb, and any text column the driver chose to hand back
		// as bytes. A raw []byte in a finding marshals to base64, which
		// reads as noise rather than as the value.
		var decoded any
		if err := json.Unmarshal(t, &decoded); err == nil {
			return decoded
		}
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return v
	}
}

// queryRows is the read path: bind, run, decode.
func (a *Adapter) queryRows(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: query: %w", err)
	}
	return rowsToMaps(rows)
}

// queryScalar runs a statement that answers with one boolean, which is what
// every pg_terminate_backend / pg_cancel_backend call returns.
//
// The distinction from queryRows matters for a remediation action: "the
// backend was not found" and "the backend refused to die" both return false,
// and reporting either as a row count of zero would tell the operator the
// remediation did nothing without saying whether it was a no-op or a
// failure. The boolean is the answer.
func (a *Adapter) queryScalar(ctx context.Context, query string, args ...any) (bool, error) {
	var ok bool
	if err := a.db.QueryRowContext(ctx, query, args...).Scan(&ok); err != nil {
		return false, fmt.Errorf("postgres: query: %w", err)
	}
	return ok, nil
}

// execStatement runs DDL/DML that returns no rows. VACUUM and ANALYZE are
// the only callers, and both are the reason a driver-level Exec is needed
// rather than a query: they cannot run inside a transaction block, and
// wrapping them in one would turn a working maintenance command into an
// error that names the wrapper rather than the problem.
func (a *Adapter) execStatement(ctx context.Context, statement string) error {
	if _, err := a.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("postgres: exec: %w", err)
	}
	return nil
}

// ── read queries ───────────────────────────────────────────────────────
//
// Every one of these is read-only and runs against the catalog views rather
// than against a table, so none of them can block a writer. That is the
// property that makes the L1 classification honest: a diagnostic that took
// an ACCESS EXCLUSIVE lock would be a write wearing a read's name.

const qListDatabases = `
SELECT datname,
       pg_database_size(datname) AS size_bytes,
       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname) AS connections
FROM pg_database d
WHERE NOT datistemplate AND datallowconn
ORDER BY datname`

const qListSchemas = `
SELECT nspname AS schema,
       (SELECT count(*) FROM pg_class c WHERE c.relnamespace = n.oid AND c.relkind = 'r') AS tables
FROM pg_namespace n
WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
ORDER BY nspname`

const qListTables = `
SELECT n.nspname AS schema,
       c.relname AS table,
       c.reltuples::bigint AS approx_rows,
       pg_total_relation_size(c.oid) AS total_bytes,
       pg_total_relation_size(c.reltoastrelid) AS toast_bytes
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r' AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
ORDER BY pg_total_relation_size(c.oid) DESC`

const qActiveSessions = `
SELECT pid, usename, datname, application_name, state,
       now() - query_start AS query_age,
       now() - state_change AS state_age,
       wait_event_type, wait_event, left(query, 500) AS query
FROM pg_stat_activity
WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'
ORDER BY query_start NULLS LAST`

// qLongRunningTxns is the evidence pg.terminate_long_tx acts on, so the two
// must agree on what "long" means. The threshold is a bind parameter for
// exactly that reason: a hardcoded 30s in one place and 60s in the other
// would make the remediation skip the very transactions the diagnostic
// reported.
const qLongRunningTxns = `
SELECT pid, usename, datname, application_name, state,
       now() - xact_start AS xact_age,
       now() - state_change AS idle_in_tx_age,
       left(query, 500) AS last_query
FROM pg_stat_activity
WHERE pid <> pg_backend_pid()
  AND xact_start IS NOT NULL
  AND now() - xact_start > make_interval(secs => $1)
ORDER BY xact_start`

const qTopQueriesByTime = `
SELECT queryid, calls, total_exec_time, mean_exec_time, rows,
       left(query, 1000) AS query
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT $1`

const qTopQueriesByCalls = `
SELECT queryid, calls, total_exec_time, mean_exec_time, rows,
       left(query, 1000) AS query
FROM pg_stat_statements
ORDER BY calls DESC
LIMIT $1`

// qLockWaits reports the waiter/blocker pair rather than a flat lock list,
// because a flat list does not answer the only question an operator has:
// who is holding me up. A row whose blocker_pid is null is a lock nobody is
// waiting on, which is a different finding.
const qLockWaits = `
SELECT w.pid AS waiting_pid,
       w.usename AS waiting_user,
       w.datname,
       w.wait_event_type,
       w.wait_event,
       left(w.query, 300) AS waiting_query,
       b.pid AS blocker_pid,
       b.usename AS blocker_user,
       left(b.query, 300) AS blocker_query
FROM pg_stat_activity w
JOIN pg_locks wl ON wl.pid = w.pid AND NOT wl.granted
JOIN pg_locks bl ON bl.locktype = wl.locktype
                 AND bl.database IS NOT DISTINCT FROM wl.database
                 AND bl.relation IS NOT DISTINCT FROM wl.relation
                 AND bl.page IS NOT DISTINCT FROM wl.page
                 AND bl.tuple IS NOT DISTINCT FROM wl.tuple
                 AND bl.virtualxid IS NOT DISTINCT FROM wl.virtualxid
                 AND bl.transactionid IS NOT DISTINCT FROM wl.transactionid
                 AND bl.classid IS NOT DISTINCT FROM wl.classid
                 AND bl.objid IS NOT DISTINCT FROM wl.objid
                 AND bl.objsubid IS NOT DISTINCT FROM wl.objsubid
                 AND bl.granted
JOIN pg_stat_activity b ON b.pid = bl.pid
WHERE w.pid <> pg_backend_pid()
ORDER BY w.wait_event_start NULLS LAST
LIMIT $1`

// qTableBloat is the standard dead-tuple estimate: the share of a heap that
// is free space no live row occupies. It is an estimate because the exact
// figure needs a full table scan, and a diagnostic that scans every table in
// production to answer a question is a different kind of incident.
const qTableBloat = `
SELECT schemaname, relname AS table, n_live_tup, n_dead_tup,
       CASE WHEN n_live_tup + n_dead_tup = 0 THEN 0
            ELSE round(100.0 * n_dead_tup / (n_live_tup + n_dead_tup), 2)
       END AS dead_pct,
       last_vacuum, last_autovacuum, last_analyze, last_autoanalyze
FROM pg_stat_user_tables
WHERE n_dead_tup > 0
ORDER BY n_dead_tup DESC
LIMIT $1`

const qIndexUsage = `
SELECT schemaname, relname AS table, indexrelname AS index,
       idx_scan, idx_tup_read, idx_tup_fetch,
       pg_size_pretty(pg_relation_size(indexrelid)) AS index_size
FROM pg_stat_user_indexes
ORDER BY idx_scan ASC
LIMIT $1`

const qVacuumStatus = `
SELECT p.pid, n.nspname AS schema, c.relname AS table,
       p.phase, p.heap_blks_total, p.heap_blks_scanned, p.heap_blks_vacuumed
FROM pg_stat_progress_vacuum p
JOIN pg_class c ON c.oid = p.relid
JOIN pg_namespace n ON n.oid = c.relnamespace`

// qSlowLog comes from pg_stat_statements rather than the server log file:
// reading a log file means finding it, parsing a format the operator chose,
// and holding a handle on a file the rotation may take away mid-read. The
// extension already keeps the aggregate the console shows.
const qSlowLog = `
SELECT queryid, calls, mean_exec_time, max_exec_time, total_exec_time,
       rows, left(query, 1000) AS query
FROM pg_stat_statements
WHERE mean_exec_time > $1
ORDER BY mean_exec_time DESC
LIMIT $2`

// qExplain is EXPLAIN without ANALYZE, on purpose. ANALYZE executes the
// statement, which for a write is a write — and this is the tool an operator
// reaches for precisely when the database is already in trouble. A plan is
// what is wanted; running the query to get one is not.
const qExplain = `EXPLAIN (FORMAT JSON, COSTS true, VERBOSE false) `

// ── Diagnose category routing ──────────────────────────────────────────
//
// Diagnose is a single entry point with a category string, and the category
// names are the adapter's public vocabulary: a caller that cannot guess them
// has no way to discover them, so they are listed in one table and the error
// for an unknown one names the rest.
//
// The bind functions are separate from the query strings because only some
// queries take parameters. Appending a limit to a query that does not have a
// placeholder is an error, and the mistake is invisible until a production
// database says so.

const (
	catDatabases    = "databases"
	catSchemas      = "schemas"
	catTables       = "tables"
	catSessions     = "sessions"
	catLongTxns     = "long_transactions"
	catSlowByTime   = "slow_by_time"
	catSlowByCalls  = "slow_by_calls"
	catLockWaits    = "lock_waits"
	catBloat        = "bloat"
	catIndexUsage   = "index_usage"
	catVacuumStatus = "vacuum_status"
	catSlowLog      = "slow_log"
	catExplain      = "explain"
	catReplication  = "replication"
)

var diagnoseRoutes = map[string]diagnoseRoute{
	catDatabases: {
		query:      qListDatabases,
		summary:    "databases with size and live connection count",
		suggestion: "pg.connect to another database, or pg.active_sessions to see who is connecting",
	},
	catSchemas: {
		query:   qListSchemas,
		summary: "schemas with table counts",
	},
	catTables: {
		query:   qListTables,
		summary: "tables with row estimates and total size",
	},
	catSessions: {
		query:      qActiveSessions,
		bind:       func(q adapter.DiagnoseQuery) ([]any, error) { return nil, nil },
		summary:    "live client backends with query age and wait event",
		suggestion: "pg.cancel_query or pg.kill_session on a pid that is blocking others",
	},
	catLongTxns: {
		query: qLongRunningTxns,
		bind: func(q adapter.DiagnoseQuery) ([]any, error) {
			age, err := intArg(q.Params, "min_age_seconds", 30)
			if err != nil {
				return nil, err
			}
			return []any{age}, nil
		},
		summary:    "transactions held open past the age threshold",
		suggestion: "pg.terminate_long_tx — the same threshold, so the remedy acts on these rows",
	},
	catSlowByTime: {
		query:   qTopQueriesByTime,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return []any{q.Limit}, nil },
		summary: "statements ordered by total execution time",
	},
	catSlowByCalls: {
		query:   qTopQueriesByCalls,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return []any{q.Limit}, nil },
		summary: "statements ordered by call count",
	},
	catLockWaits: {
		query:   qLockWaits,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return []any{q.Limit}, nil },
		summary: "lock waiters joined to their blockers",
	},
	catBloat: {
		query:   qTableBloat,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return []any{q.Limit}, nil },
		summary: "tables by dead-tuple share",
	},
	catIndexUsage: {
		query:   qIndexUsage,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return []any{q.Limit}, nil },
		summary: "indexes by ascending scan count",
	},
	catVacuumStatus: {
		query:      qVacuumStatus,
		bind:       func(q adapter.DiagnoseQuery) ([]any, error) { return nil, nil },
		summary:    "vacuum currently in progress",
		suggestion: "a vacuum running for hours with heap_blks_scanned flat is the pg.table_bloat case",
	},
	catSlowLog: {
		query: qSlowLog,
		bind: func(q adapter.DiagnoseQuery) ([]any, error) {
			minMs, err := intArg(q.Params, "min_ms", 100)
			if err != nil {
				return nil, err
			}
			return []any{float64(minMs), q.Limit}, nil
		},
		summary: "statements slower than the threshold",
	},
	catExplain: {
		query:   qExplain,
		summary: "query plan; the statement is not executed",
	},
	catReplication: {
		query:   qReplicationStatus,
		bind:    func(q adapter.DiagnoseQuery) ([]any, error) { return nil, nil },
		summary: "standbys and their WAL lag",
		// The empty answer is the one that needs saying here: an empty
		// pg_stat_replication is most often "this is not the primary" or
		// "the DSN points somewhere else", and both read as "replication
		// is fine" if nobody says otherwise.
		emptySuggestion: "no rows means this instance is not a primary with connected standbys, which is a different answer from zero lag",
	},
}

// diagnoseCategoryNames lists the categories, for the error message.
func diagnoseCategoryNames() []string {
	names := make([]string, 0, len(diagnoseRoutes))
	for name := range diagnoseRoutes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// qReplicationStatus reads the WAL sender's view of each standby.
//
// Why pg_stat_replication and not the standby's own view. This adapter
// connects to one DSN, which is normally the primary. pg_stat_replication
// lives on the primary and reports every standby connected to it, which
// answers "is replication behind" for the whole cluster from one connection.
// A standby's own pg_last_wal_replay_lsn() answers a narrower question and
// requires connecting to that standby, which is a different DSN.
//
// The lag figures are computed two ways because they mean different things.
// `replay_lag_bytes` is how much WAL has not been applied — what grows during
// an incident and what an operator sizes a fix against. `replay_lag_seconds`
// is the time behind the primary, which is what an RPO statement is written
// in. Neither is derivable from the other: a standby can be a gigabyte behind
// and two seconds behind, or a megabyte behind and twenty minutes behind
// after a clock or a long transaction.
//
// A NULL replay_lsn or NULL replay_lag is NOT zero lag. It means the standby
// has not yet reported those positions — a new connection, or a standby that
// is still catching up from a base backup — and the adapter reports those as
// unknown rather than as healthy. `pg_wal_lsn_diff` returns NULL when either
// argument is NULL, which is what carries that through.
const qReplicationStatus = `
SELECT application_name,
       client_addr,
       state,
       sync_state,
       sent_lsn,
       write_lsn,
       flush_lsn,
       replay_lsn,
       pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS replay_lag_bytes,
       EXTRACT(EPOCH FROM replay_lag) AS replay_lag_seconds
FROM pg_stat_replication
ORDER BY pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) DESC NULLS FIRST`
