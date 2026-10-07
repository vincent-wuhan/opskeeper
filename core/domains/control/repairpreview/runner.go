package repairpreview

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const isolationBoundary = "Controlled fixed-workload reconstruction in disposable preview-pg; original active sessions are not copied."

func openPreviewDatabase(dsn string) (*sql.DB, error) {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open repair preview database: %w", err)
	}
	return database, nil
}

func Execute(ctx context.Context, database *sql.DB, spec WorkloadSpec) (Run, error) {
	if err := spec.Validate(); err != nil {
		return Run{}, err
	}
	binding := spec.RuntimeBinding
	if binding.RunID == "" || binding.TenantID == "" || binding.IncidentID == "" ||
		binding.ScenarioID == "" || binding.IdempotencyKey == "" || binding.TargetFingerprint == "" {
		return Run{}, errors.New(
			"repair preview: run, tenant, incident, scenario, idempotency key, and target fingerprint are required",
		)
	}
	if _, err := uuid.Parse(binding.RunID); err != nil {
		return Run{}, fmt.Errorf("repair preview: run id must be a UUID: %w", err)
	}
	if database == nil {
		return Run{}, errors.New("repair preview: database is required")
	}
	if err := database.PingContext(ctx); err != nil {
		return Run{}, fmt.Errorf("repair preview database unavailable: %w", err)
	}
	started := time.Now().UTC()
	baseline, err := executeBranch(ctx, database, spec, "baseline", CandidateSpec{})
	if err != nil {
		return Run{}, err
	}
	candidates := make([]Candidate, 0, len(spec.Candidates)+1)
	candidates = append(candidates, newBaselineCandidate(binding, baseline))
	for _, candidateSpec := range spec.Candidates {
		result, err := executeBranch(ctx, database, spec, candidateSpec.CandidateID, candidateSpec)
		if err != nil {
			return Run{}, fmt.Errorf("candidate %s: %w", candidateSpec.CandidateID, err)
		}
		candidate := Candidate{
			ID: deterministicCandidateID(binding.RunID, candidateSpec.CandidateID), RunID: binding.RunID,
			TenantID: binding.TenantID, IncidentID: binding.IncidentID, CandidateID: candidateSpec.CandidateID,
			Name: candidateSpec.Name, Kind: candidateSpec.Kind, Action: candidateSpec.Action,
			ChangeSummary: candidateSpec.ChangeSummary, Branch: result.schemaName,
			ResultChecksum: result.checksum, Consistent: result.checksum == baseline.checksum,
			AverageLatencyMS: average(result.latencies), MedianLatencyMS: percentile(result.latencies, 50),
			P95LatencyMS: percentile(result.latencies, 95), SampleCount: len(result.latencies),
			TPS: result.tps, ErrorCount: result.errorCount, WriteImpact: "preview_only",
			StorageDeltaBytes: storageDeltaBytes(baseline.storageBytes, result.storageBytes),
			BusinessProbePass: result.businessProbePass,
		}
		candidate.Decision, candidate.RejectionReason = Evaluate(candidate)
		candidates = append(candidates, candidate)
	}
	finished := time.Now().UTC()
	return Run{
		ID: binding.RunID, TenantID: binding.TenantID, IncidentID: binding.IncidentID,
		ScenarioID: binding.ScenarioID, IdempotencyKey: binding.IdempotencyKey,
		TargetFingerprint: binding.TargetFingerprint, BindingFingerprint: binding.Fingerprint(),
		BranchPrefix:    "preview/" + binding.IncidentID + "/" + shortHash(binding.RunID),
		SeedFingerprint: spec.SeedFingerprint(), WorkloadFingerprint: spec.WorkloadFingerprint(),
		WorkloadRevision: spec.Revision, ControlledLoad: true, IsolationBoundary: isolationBoundary,
		Status: "finished", StartedAt: started, FinishedAt: finished, Candidates: candidates,
	}, nil
}

func newBaselineCandidate(binding WorkloadBinding, baseline branchResult) Candidate {
	candidate := Candidate{
		ID:                deterministicCandidateID(binding.RunID, BaselineCandidateID),
		RunID:             binding.RunID,
		TenantID:          binding.TenantID,
		IncidentID:        binding.IncidentID,
		CandidateID:       BaselineCandidateID,
		Name:              "Baseline replay",
		Kind:              BaselineKind,
		Action:            BaselineAction,
		ChangeSummary:     "Controlled fixed-workload baseline",
		Branch:            baseline.schemaName,
		ResultChecksum:    baseline.checksum,
		Consistent:        true,
		AverageLatencyMS:  average(baseline.latencies),
		MedianLatencyMS:   percentile(baseline.latencies, 50),
		P95LatencyMS:      percentile(baseline.latencies, 95),
		SampleCount:       len(baseline.latencies),
		TPS:               baseline.tps,
		ErrorCount:        baseline.errorCount,
		WriteImpact:       "none",
		BusinessProbePass: baseline.businessProbePass,
	}
	candidate.Decision, candidate.RejectionReason = Evaluate(candidate)
	return candidate
}

type branchResult struct {
	schemaName        string
	checksum          string
	latencies         []float64
	tps               float64
	errorCount        int
	storageBytes      int64
	businessProbePass bool
}

func executeBranch(ctx context.Context, database *sql.DB, spec WorkloadSpec, label string, candidate CandidateSpec) (branchResult, error) {
	schemaName := repairPreviewSchemaPrefix(spec.RuntimeBinding.RunID) + "_" + safeSchemaLabel(label)
	if safeSchemaLabel(label) == "" {
		return branchResult{}, fmt.Errorf("repair preview: branch label %q is invalid", label)
	}
	if _, err := database.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		return branchResult{}, fmt.Errorf("create preview schema: %w", err)
	}
	defer func() {
		_, _ = database.Exec("DROP SCHEMA IF EXISTS " + schemaName + " CASCADE")
	}()
	connection, err := database.Conn(ctx)
	if err != nil {
		return branchResult{}, fmt.Errorf("acquire preview connection: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "SET search_path TO "+schemaName); err != nil {
		return branchResult{}, fmt.Errorf("set preview search path: %w", err)
	}
	if _, err := connection.ExecContext(ctx, repairPreviewSeedSQL); err != nil {
		return branchResult{}, fmt.Errorf("seed preview schema: %w", err)
	}
	if candidate.Action != "" {
		actionCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
		err := applyCandidateAction(actionCtx, connection, candidate.Action)
		cancel()
		if err != nil {
			return branchResult{}, err
		}
	}
	for index := 0; index < spec.WarmupQueries; index++ {
		query := spec.Queries[index%len(spec.Queries)]
		queryCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
		_, err := connection.ExecContext(queryCtx, query.SQL)
		cancel()
		if err != nil {
			return branchResult{}, fmt.Errorf("warm up query %s: %w", query.Name, err)
		}
	}
	result := branchResult{schemaName: schemaName}
	if err := replayFixedWorkload(ctx, database, spec, &result); err != nil {
		return branchResult{}, err
	}
	checksum, err := queryChecksum(ctx, connection, spec)
	if err != nil {
		return branchResult{}, err
	}
	result.checksum = checksum
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
	probeCount, err := executeBusinessProbe(probeCtx, connection, spec.BusinessProbe)
	cancel()
	if err != nil {
		return branchResult{}, err
	}
	result.businessProbePass = probeCount >= spec.BusinessProbe.ExpectedMinimum
	result.storageBytes, err = previewStorageBytes(ctx, connection, schemaName)
	if err != nil {
		return branchResult{}, err
	}
	return result, nil
}

func applyCandidateAction(ctx context.Context, connection *sql.Conn, action string) error {
	switch action {
	case "resize_pool":
		_, err := connection.ExecContext(ctx, "UPDATE repair_preview_pool_settings SET pool_size = 40 WHERE id = 1")
		return err
	case "reset_pool":
		_, err := connection.ExecContext(ctx, "DELETE FROM repair_preview_sessions")
		return err
	default:
		return fmt.Errorf("unsupported preview candidate action %q", action)
	}
}

func replayFixedWorkload(ctx context.Context, database *sql.DB, spec WorkloadSpec, result *branchResult) error {
	tokens := make(chan struct{}, spec.Concurrency)
	var mutex sync.Mutex
	var waitGroup sync.WaitGroup
	started := time.Now()
	for index := 0; index < spec.Samples; index++ {
		query := spec.Queries[index%len(spec.Queries)]
		waitGroup.Add(1)
		tokens <- struct{}{}
		go func(query QuerySpec) {
			defer waitGroup.Done()
			defer func() { <-tokens }()
			queryStarted := time.Now()
			queryCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
			worker, err := database.Conn(queryCtx)
			if err != nil {
				cancel()
				mutex.Lock()
				result.errorCount++
				mutex.Unlock()
				return
			}
			_, err = worker.ExecContext(queryCtx, "SET search_path TO "+result.schemaName)
			if err == nil {
				var rows *sql.Rows
				rows, err = worker.QueryContext(queryCtx, query.SQL)
				if err == nil {
					_ = rows.Close()
				}
			}
			_ = worker.Close()
			cancel()
			if err != nil {
				mutex.Lock()
				result.errorCount++
				mutex.Unlock()
				return
			}
			elapsedMS := float64(time.Since(queryStarted).Microseconds()) / 1000
			mutex.Lock()
			result.latencies = append(result.latencies, elapsedMS)
			mutex.Unlock()
		}(query)
	}
	waitGroup.Wait()
	if result.errorCount > 0 {
		return fmt.Errorf("fixed workload completed with %d query errors", result.errorCount)
	}
	elapsedSeconds := time.Since(started).Seconds()
	if elapsedSeconds <= 0 {
		result.tps = float64(spec.Samples)
	} else {
		result.tps = float64(spec.Samples) / elapsedSeconds
	}
	return nil
}

func queryChecksum(ctx context.Context, connection *sql.Conn, spec WorkloadSpec) (string, error) {
	digest := sha256.New()
	for _, query := range spec.Queries {
		queryCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.QueryTimeoutMS)*time.Millisecond)
		rows, err := connection.QueryContext(queryCtx, query.SQL)
		if err != nil {
			cancel()
			return "", fmt.Errorf("checksum query %s: %w", query.Name, err)
		}
		if err := hashQueryRows(digest, rows); err != nil {
			_ = rows.Close()
			cancel()
			return "", fmt.Errorf("hash query %s: %w", query.Name, err)
		}
		if err := rows.Close(); err != nil {
			cancel()
			return "", fmt.Errorf("close checksum query %s: %w", query.Name, err)
		}
		cancel()
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func hashQueryRows(digest hash.Hash, rows *sql.Rows) error {
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	_, _ = digest.Write([]byte(strings.Join(columns, "\x00") + "\x1e"))
	values := make([]any, len(columns))
	scans := make([]any, len(columns))
	for index := range scans {
		scans[index] = &values[index]
	}
	for rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			return err
		}
		encoded, err := json.Marshal(normalizeDatabaseValues(values))
		if err != nil {
			return err
		}
		_, _ = digest.Write(encoded)
		_, _ = digest.Write([]byte("\x1e"))
	}
	return rows.Err()
}

func normalizeDatabaseValues(values []any) []any {
	normalized := make([]any, len(values))
	for index, value := range values {
		switch typed := value.(type) {
		case []byte:
			normalized[index] = string(typed)
		default:
			normalized[index] = typed
		}
	}
	return normalized
}

func executeBusinessProbe(ctx context.Context, connection *sql.Conn, probe BusinessProbeSpec) (int64, error) {
	var count int64
	if err := connection.QueryRowContext(ctx, probe.Query).Scan(&count); err != nil {
		return 0, fmt.Errorf("business probe %s: %w", probe.Name, err)
	}
	return count, nil
}

func previewStorageBytes(ctx context.Context, connection *sql.Conn, schemaName string) (int64, error) {
	var bytes int64
	if err := connection.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pg_total_relation_size(quote_ident(schemaname) || '.' || quote_ident(tablename))), 0)
		FROM pg_tables WHERE schemaname = $1`, schemaName).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("measure preview storage: %w", err)
	}
	return bytes, nil
}

func storageDeltaBytes(baselineBytes, candidateBytes int64) int64 {
	delta := candidateBytes - baselineBytes
	if delta < 0 {
		return -delta
	}
	return delta
}

func repairPreviewSchemaPrefix(runID string) string {
	return "rp_" + shortHash(runID)
}

func safeSchemaLabel(label string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(label) {
		if builder.Len() >= 47 {
			break
		}
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			continue
		}
		if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "_") {
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "_")
}

func deterministicCandidateID(runID, candidateID string) string {
	namespace := uuid.NewSHA1(uuid.NameSpaceURL, []byte("opskeeper:repair-preview-candidate"))
	return uuid.NewSHA1(namespace, []byte(runID+"/"+candidateID)).String()
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func percentile(values []float64, target float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	position := target / 100 * float64(len(ordered)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return ordered[lower]
	}
	weight := position - float64(lower)
	return ordered[lower]*(1-weight) + ordered[upper]*weight
}

const repairPreviewSeedSQL = `
CREATE TABLE repair_preview_accounts (
    customer_id TEXT PRIMARY KEY,
    balance BIGINT NOT NULL
);
CREATE TABLE repair_preview_pool_settings (
    id BIGINT PRIMARY KEY,
    pool_size INTEGER NOT NULL
);
CREATE TABLE repair_preview_sessions (
    customer_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    PRIMARY KEY (customer_id, session_id)
);
INSERT INTO repair_preview_accounts (customer_id, balance) VALUES
    ('customer-a', 1000), ('customer-b', 2000), ('customer-c', 3000);
INSERT INTO repair_preview_pool_settings (id, pool_size) VALUES (1, 20);
INSERT INTO repair_preview_sessions (customer_id, session_id) VALUES
    ('customer-a', 'session-a'), ('customer-b', 'session-b'), ('customer-c', 'session-c');
`
