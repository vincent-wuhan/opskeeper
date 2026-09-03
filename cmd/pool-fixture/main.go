// Command pool-fixture owns a disposable application pool used by the live
// PostgreSQL connection-exhaustion demo. It never accepts a backend PID.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	manifestSchemaVersion = "v1"
	poolStateRunning      = "running"
	poolStateRecovered    = "recovered"
	poolStateExpired      = "expired"
	poolStateStale        = "stale"
	maxRequestBodyBytes   = 16 << 10
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var targetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{7,127}$`)

type ProbeRecord struct {
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`
	BackendPID   int       `json:"-"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"-"`
}

type PoolManifest struct {
	SchemaVersion     string       `json:"schema_version"`
	ManifestID        string       `json:"manifest_id"`
	CaseID            string       `json:"case_id"`
	IncidentID        string       `json:"incident_id"`
	Resource          string       `json:"resource"`
	InitialCapacity   int          `json:"initial_capacity"`
	TargetCapacity    int          `json:"target_capacity"`
	ActiveConnections int          `json:"active_connections"`
	Status            string       `json:"status"`
	StartedAt         time.Time    `json:"started_at"`
	ExpiresAt         time.Time    `json:"expires_at"`
	RecoveredAt       *time.Time   `json:"recovered_at,omitempty"`
	ExpiredAt         *time.Time   `json:"expired_at,omitempty"`
	LastProbe         *ProbeRecord `json:"failed_probe,omitempty"`
	RecoveryProbe     *ProbeRecord `json:"recovery_probe,omitempty"`
}

type PoolConnection interface {
	BackendPID() int
	Release() error
}

type PoolRuntime interface {
	Saturate(ctx context.Context, capacity int) ([]PoolConnection, error)
	Probe(ctx context.Context) (ProbeRecord, error)
	ResizeAndRecycle(ctx context.Context, connections []PoolConnection, capacity int) error
	Close() error
}

type RuntimeFactory func(ctx context.Context, dsn string) (PoolRuntime, error)

type ownedPool struct {
	manifest    PoolManifest
	connections []PoolConnection
	runtime     PoolRuntime
	timer       *time.Timer
}

type Controller struct {
	mu         sync.Mutex
	token      string
	stateDir   string
	dsn        string
	newRuntime RuntimeFactory
	pools      map[string]*ownedPool
	poolKeys   map[string]string
	log        *slog.Logger
}

func NewController(token, stateDir, dsn string, newRuntime RuntimeFactory, log *slog.Logger) (*Controller, error) {
	if len(token) < 32 {
		return nil, fmt.Errorf("runtime token must contain at least 32 characters")
	}
	if stateDir == "" || dsn == "" || newRuntime == nil {
		return nil, fmt.Errorf("state directory, PostgreSQL DSN, and runtime factory are required")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	controller := &Controller{
		token:      token,
		stateDir:   stateDir,
		dsn:        dsn,
		newRuntime: newRuntime,
		pools:      make(map[string]*ownedPool),
		poolKeys:   make(map[string]string),
		log:        log,
	}
	if err := controller.loadManifests(); err != nil {
		return nil, err
	}
	return controller, nil
}

type StartRequest struct {
	CaseID          string `json:"case_id"`
	IncidentID      string `json:"incident_id"`
	InitialCapacity int    `json:"initial_capacity"`
	TargetCapacity  int    `json:"target_capacity"`
	TTLSeconds      int    `json:"ttl_seconds"`
}

func (c *Controller) Start(ctx context.Context, request StartRequest) (PoolManifest, error) {
	if !identifierPattern.MatchString(request.CaseID) || !identifierPattern.MatchString(request.IncidentID) ||
		request.InitialCapacity < 2 || request.InitialCapacity > 8 ||
		request.TargetCapacity <= request.InitialCapacity || request.TargetCapacity > 16 ||
		request.TTLSeconds < 60 || request.TTLSeconds > 86400 {
		return PoolManifest{}, errInvalid
	}
	key := request.CaseID + "/" + request.IncidentID
	c.mu.Lock()
	defer c.mu.Unlock()
	if manifestID, ok := c.poolKeys[key]; ok {
		pool := c.pools[manifestID]
		if pool != nil {
			c.expireIfDueLocked(pool, time.Now().UTC())
		}
		if pool != nil && pool.manifest.Status == poolStateRunning &&
			pool.manifest.InitialCapacity == request.InitialCapacity &&
			pool.manifest.TargetCapacity == request.TargetCapacity {
			return c.snapshotLocked(pool), nil
		}
		return PoolManifest{}, errConflict
	}
	runtime, err := c.newRuntime(ctx, c.dsn)
	if err != nil {
		return PoolManifest{}, fmt.Errorf("open owned PostgreSQL pool: %w", err)
	}
	connections, err := runtime.Saturate(ctx, request.InitialCapacity)
	if err != nil {
		_ = runtime.Close()
		return PoolManifest{}, fmt.Errorf("saturate owned PostgreSQL pool: %w", err)
	}
	manifestID, err := newManifestID()
	if err != nil {
		_ = runtime.Close()
		return PoolManifest{}, err
	}
	startedAt := time.Now().UTC()
	pool := &ownedPool{
		manifest: PoolManifest{
			SchemaVersion:     manifestSchemaVersion,
			ManifestID:        manifestID,
			CaseID:            request.CaseID,
			IncidentID:        request.IncidentID,
			Resource:          "pg:pool-fixture",
			InitialCapacity:   request.InitialCapacity,
			TargetCapacity:    request.TargetCapacity,
			ActiveConnections: len(connections),
			Status:            poolStateRunning,
			StartedAt:         startedAt,
			ExpiresAt:         startedAt.Add(time.Duration(request.TTLSeconds) * time.Second),
		},
		connections: connections,
		runtime:     runtime,
	}
	pool.timer = time.AfterFunc(time.Duration(request.TTLSeconds)*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.expireIfDueLocked(pool, time.Now().UTC()); err != nil {
			c.log.Error("expire pool fixture", "error", err)
		}
	})
	if err := c.persistLocked(pool); err != nil {
		_ = c.closePoolLocked(pool)
		return PoolManifest{}, err
	}
	c.pools[manifestID] = pool
	c.poolKeys[key] = manifestID
	return c.snapshotLocked(pool), nil
}

func (c *Controller) Status(manifestID string) (PoolManifest, error) {
	if !targetPattern.MatchString(manifestID) {
		return PoolManifest{}, errInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pool, ok := c.pools[manifestID]
	if !ok {
		return PoolManifest{}, errNotFound
	}
	if err := c.expireIfDueLocked(pool, time.Now().UTC()); err != nil {
		return PoolManifest{}, err
	}
	return c.snapshotLocked(pool), nil
}

func (c *Controller) Statuses() []PoolManifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	statuses := make([]PoolManifest, 0, len(c.pools))
	for _, pool := range c.pools {
		if err := c.expireIfDueLocked(pool, time.Now().UTC()); err != nil {
			c.log.Error("expire pool fixture", "error", err)
		}
		statuses = append(statuses, c.snapshotLocked(pool))
	}
	return statuses
}

type ProbeRequest struct {
	TimeoutMilliseconds int `json:"timeout_milliseconds"`
}

func (c *Controller) Probe(ctx context.Context, manifestID string, request ProbeRequest) (ProbeRecord, error) {
	if !targetPattern.MatchString(manifestID) || request.TimeoutMilliseconds < 250 || request.TimeoutMilliseconds > 5000 {
		return ProbeRecord{}, errInvalid
	}
	c.mu.Lock()
	pool, ok := c.pools[manifestID]
	if !ok {
		c.mu.Unlock()
		return ProbeRecord{}, errNotFound
	}
	if err := c.expireIfDueLocked(pool, time.Now().UTC()); err != nil {
		c.mu.Unlock()
		return ProbeRecord{}, err
	}
	if pool.manifest.Status != poolStateRunning {
		c.mu.Unlock()
		return ProbeRecord{}, errConflict
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutMilliseconds)*time.Millisecond)
	defer cancel()
	record, err := pool.runtime.Probe(probeCtx)
	if err != nil && record.Status == "" {
		record.Status = "failed"
		record.ErrorCode = "pool_probe_failed"
		record.ErrorMessage = err.Error()
	}
	pool.manifest.LastProbe = &record
	if persistErr := c.persistLocked(pool); persistErr != nil {
		c.mu.Unlock()
		return record, persistErr
	}
	c.mu.Unlock()
	if record.Status != "success" {
		return record, fmt.Errorf("%w: %s", errProbeFailed, record.ErrorCode)
	}
	return record, nil
}

type RecoverRequest struct {
	Reason string `json:"reason"`
}

func (c *Controller) Recover(ctx context.Context, manifestID string, request RecoverRequest) (PoolManifest, error) {
	if !targetPattern.MatchString(manifestID) || request.Reason == "" || len(request.Reason) > 512 {
		return PoolManifest{}, errInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	pool, ok := c.pools[manifestID]
	if !ok {
		return PoolManifest{}, errNotFound
	}
	if err := c.expireIfDueLocked(pool, time.Now().UTC()); err != nil {
		return PoolManifest{}, err
	}
	if pool.manifest.Status != poolStateRunning || pool.manifest.LastProbe == nil || pool.manifest.LastProbe.Status != "failed" {
		return PoolManifest{}, errProbeRequired
	}
	if err := pool.runtime.ResizeAndRecycle(ctx, pool.connections, pool.manifest.TargetCapacity); err != nil {
		return PoolManifest{}, fmt.Errorf("resize and recycle owned pool: %w", err)
	}
	pool.connections = nil
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recovery, err := pool.runtime.Probe(probeCtx)
	if err != nil && recovery.Status == "" {
		recovery.Status = "failed"
		recovery.ErrorCode = "recovery_probe_failed"
		recovery.ErrorMessage = err.Error()
	}
	pool.manifest.RecoveryProbe = &recovery
	if recovery.Status != "success" {
		if persistErr := c.persistLocked(pool); persistErr != nil {
			return PoolManifest{}, persistErr
		}
		return PoolManifest{}, fmt.Errorf("%w: %s", errRecoveryProbeFailed, recovery.ErrorCode)
	}
	recoveredAt := time.Now().UTC()
	pool.manifest.RecoveredAt = &recoveredAt
	pool.manifest.Status = poolStateRecovered
	pool.manifest.ActiveConnections = 0
	if pool.timer != nil {
		pool.timer.Stop()
	}
	if err := c.persistLocked(pool); err != nil {
		return PoolManifest{}, err
	}
	return c.snapshotLocked(pool), nil
}

func (c *Controller) Shutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var combined error
	for _, pool := range c.pools {
		if err := c.closePoolLocked(pool); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (c *Controller) closePoolLocked(pool *ownedPool) error {
	var combined error
	for _, connection := range pool.connections {
		if err := connection.Release(); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	pool.connections = nil
	if pool.runtime != nil {
		if err := pool.runtime.Close(); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	if pool.timer != nil {
		pool.timer.Stop()
	}
	expiredAt := time.Now().UTC()
	pool.manifest.ExpiredAt = &expiredAt
	pool.manifest.Status = poolStateExpired
	pool.manifest.ActiveConnections = 0
	return combined
}

func (c *Controller) snapshotLocked(pool *ownedPool) PoolManifest {
	manifest := pool.manifest
	manifest.ActiveConnections = len(pool.connections)
	if manifest.Status == poolStateRunning && len(pool.connections) < manifest.InitialCapacity {
		manifest.Status = poolStateStale
	}
	return manifest
}

func (c *Controller) expireIfDueLocked(pool *ownedPool, now time.Time) error {
	if pool.manifest.Status != poolStateRunning || now.Before(pool.manifest.ExpiresAt) {
		return nil
	}
	if err := c.closePoolLocked(pool); err != nil {
		return err
	}
	return c.persistLocked(pool)
}

func (c *Controller) loadManifests() error {
	paths, err := filepath.Glob(filepath.Join(c.stateDir, "*.json"))
	if err != nil {
		return fmt.Errorf("list pool manifests: %w", err)
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open pool manifest: %w", err)
		}
		var manifest PoolManifest
		decoder := json.NewDecoder(file)
		decodeErr := decoder.Decode(&manifest)
		closeErr := file.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode pool manifest %s: %w", path, decodeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close pool manifest: %w", closeErr)
		}
		if manifest.SchemaVersion != manifestSchemaVersion || !targetPattern.MatchString(manifest.ManifestID) {
			return fmt.Errorf("invalid pool manifest %s", path)
		}
		manifest.Status = poolStateStale
		manifest.ActiveConnections = 0
		pool := &ownedPool{manifest: manifest}
		if err := c.persistLocked(pool); err != nil {
			return err
		}
		c.pools[manifest.ManifestID] = pool
		c.poolKeys[manifest.CaseID+"/"+manifest.IncidentID] = manifest.ManifestID
	}
	return nil
}

func (c *Controller) persistLocked(pool *ownedPool) error {
	data, err := json.MarshalIndent(c.snapshotLocked(pool), "", "  ")
	if err != nil {
		return fmt.Errorf("encode pool manifest: %w", err)
	}
	path := filepath.Join(c.stateDir, pool.manifest.ManifestID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write pool manifest: %w", err)
	}
	return nil
}

type postgresRuntime struct {
	db *sql.DB
}

func newPostgresRuntime(ctx context.Context, dsn string) (PoolRuntime, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &postgresRuntime{db: db}, nil
}

func (r *postgresRuntime) Saturate(ctx context.Context, capacity int) ([]PoolConnection, error) {
	r.db.SetMaxOpenConns(capacity)
	r.db.SetMaxIdleConns(capacity)
	connections := make([]PoolConnection, 0, capacity)
	for index := 0; index < capacity; index++ {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			releaseConnections(connections)
			return nil, fmt.Errorf("acquire connection %d: %w", index, err)
		}
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			_ = conn.Close()
			releaseConnections(connections)
			return nil, fmt.Errorf("begin held transaction %d: %w", index, err)
		}
		var backendPID int
		if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
			_ = conn.Close()
			releaseConnections(connections)
			return nil, fmt.Errorf("inspect held connection %d: %w", index, err)
		}
		connections = append(connections, &postgresConnection{conn: conn, backendPID: backendPID})
	}
	return connections, nil
}

func (r *postgresRuntime) Probe(ctx context.Context) (ProbeRecord, error) {
	startedAt := time.Now().UTC()
	var backendPID int
	err := r.db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backendPID)
	finishedAt := time.Now().UTC()
	record := ProbeRecord{
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		DurationMS: finishedAt.Sub(startedAt).Milliseconds(),
		BackendPID: backendPID,
	}
	if err != nil {
		record.Status = "failed"
		record.ErrorCode = "pool_exhausted"
		record.ErrorMessage = err.Error()
		return record, err
	}
	record.Status = "success"
	return record, nil
}

func (r *postgresRuntime) ResizeAndRecycle(ctx context.Context, connections []PoolConnection, capacity int) error {
	for _, connection := range connections {
		if err := connection.Release(); err != nil {
			return fmt.Errorf("recycle owned connection: %w", err)
		}
	}
	r.db.SetMaxOpenConns(capacity)
	r.db.SetMaxIdleConns(capacity)
	return r.db.PingContext(ctx)
}

func (r *postgresRuntime) Close() error { return r.db.Close() }

func releaseConnections(connections []PoolConnection) {
	for _, connection := range connections {
		_ = connection.Release()
	}
}

type postgresConnection struct {
	conn       *sql.Conn
	backendPID int
}

func (c *postgresConnection) BackendPID() int { return c.backendPID }
func (c *postgresConnection) Release() error {
	if _, err := c.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		_ = c.conn.Close()
		return fmt.Errorf("rollback owned transaction: %w", err)
	}
	return c.conn.Close()
}

func newManifestID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate pool manifest id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

var (
	errInvalid             = errors.New("invalid pool fixture request")
	errNotFound            = errors.New("pool fixture not found")
	errConflict            = errors.New("pool fixture conflict")
	errProbeRequired       = errors.New("failed pool probe required before recovery")
	errProbeFailed         = errors.New("pool probe failed")
	errRecoveryProbeFailed = errors.New("recovery probe failed")
)

type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type Handler struct {
	controller *Controller
}

func NewHandler(controller *Controller) *Handler { return &Handler{controller: controller} }

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, "ok", json.RawMessage(`{"status":"ok"}`))
		return
	}
	if authorized(request, h.controller.token) {
		h.dispatch(writer, request)
		return
	}
	writeError(writer, http.StatusUnauthorized, "unauthorized")
}

func (h *Handler) dispatch(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/readyz":
		writeJSON(writer, http.StatusOK, "ok", json.RawMessage(`{"status":"ok"}`))
	case request.Method == http.MethodGet && request.URL.Path == "/metrics":
		h.prometheusMetrics(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/pool-fixtures":
		h.create(writer, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v1/pool-fixtures/") &&
		strings.HasSuffix(request.URL.Path, "/probe"):
		h.probe(writer, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v1/pool-fixtures/") &&
		strings.HasSuffix(request.URL.Path, "/recover"):
		h.recover(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/pool-fixtures/") &&
		strings.HasSuffix(request.URL.Path, "/metrics"):
		h.metrics(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/pool-fixtures/"):
		h.status(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "not found")
	}
}

func (h *Handler) create(writer http.ResponseWriter, request *http.Request) {
	var input StartRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	manifest, err := h.controller.Start(request.Context(), input)
	if err != nil {
		writeDomainError(writer, err)
		return
	}
	writeManifest(writer, http.StatusCreated, manifest)
}

func (h *Handler) status(writer http.ResponseWriter, request *http.Request) {
	manifestID, ok := parseManifestPath(request.URL.Path, "")
	if !ok {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	manifest, err := h.controller.Status(manifestID)
	if err != nil {
		writeDomainError(writer, err)
		return
	}
	writeManifest(writer, http.StatusOK, manifest)
}

func (h *Handler) probe(writer http.ResponseWriter, request *http.Request) {
	manifestID, ok := parseManifestPath(request.URL.Path, "/probe")
	if !ok {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	var input ProbeRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	record, err := h.controller.Probe(request.Context(), manifestID, input)
	data, encodeErr := json.Marshal(record)
	if encodeErr != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, "pool probe failed", data)
		return
	}
	writeJSON(writer, http.StatusOK, "success", data)
}

func (h *Handler) recover(writer http.ResponseWriter, request *http.Request) {
	manifestID, ok := parseManifestPath(request.URL.Path, "/recover")
	if !ok {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	var input RecoverRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	manifest, err := h.controller.Recover(request.Context(), manifestID, input)
	if err != nil {
		writeDomainError(writer, err)
		return
	}
	writeManifest(writer, http.StatusOK, manifest)
}

func (h *Handler) metrics(writer http.ResponseWriter, request *http.Request) {
	manifestID, ok := parseManifestPath(request.URL.Path, "/metrics")
	if !ok {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	manifest, err := h.controller.Status(manifestID)
	if err != nil {
		writeDomainError(writer, err)
		return
	}
	capacity := manifest.TargetCapacity
	if manifest.Status == poolStateRunning {
		capacity = manifest.InitialCapacity
	}
	snapshot := poolMetricsSnapshot{
		ManifestID:        manifest.ManifestID,
		Status:            manifest.Status,
		ActiveConnections: manifest.ActiveConnections,
		Capacity:          capacity,
		FailedProbeCount:  0,
	}
	if manifest.LastProbe != nil && manifest.LastProbe.Status == "failed" {
		snapshot.FailedProbeCount = 1
	}
	if capacity > 0 {
		snapshot.UtilizationPercent = float64(manifest.ActiveConnections) / float64(capacity) * 100
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, http.StatusOK, "success", data)
}

func (h *Handler) prometheusMetrics(writer http.ResponseWriter) {
	statuses := h.controller.Statuses()
	writer.Header().Set("Content-Type", `text/plain; version=0.0.4; charset=utf-8`)
	for _, status := range statuses {
		capacity := status.TargetCapacity
		if status.Status == poolStateRunning {
			capacity = status.InitialCapacity
		}
		fmt.Fprintf(writer, "opskeeper_pool_fixture_active_connections{target=%q} %d\n", status.Resource, status.ActiveConnections)
		fmt.Fprintf(writer, "opskeeper_pool_fixture_capacity{target=%q} %d\n", status.Resource, capacity)
	}
}

type poolMetricsSnapshot struct {
	ManifestID         string  `json:"manifest_id"`
	Status             string  `json:"status"`
	ActiveConnections  int     `json:"active_connections"`
	Capacity           int     `json:"capacity"`
	UtilizationPercent float64 `json:"utilization_percent"`
	FailedProbeCount   int     `json:"failed_probe_count"`
}

func writeManifest(writer http.ResponseWriter, status int, manifest PoolManifest) {
	data, err := json.Marshal(manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, status, "success", data)
}

func parseManifestPath(path, action string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "v1" || parts[1] != "pool-fixtures" || parts[2] == "" {
		return "", false
	}
	if action == "" {
		return parts[2], len(parts) == 3
	}
	return parts[2], len(parts) == 4 && parts[3] == strings.TrimPrefix(action, "/")
}

func authorized(request *http.Request, token string) bool {
	if request.Header.Get("X-Opskeeper-Version") != "v1" {
		return false
	}
	authorization := request.Header.Get("Authorization")
	prefix := "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(authorization[len(prefix):]), []byte(token)) == 1
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid json")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(writer, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func writeDomainError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalid):
		writeError(writer, http.StatusBadRequest, "invalid request")
	case errors.Is(err, errNotFound):
		writeError(writer, http.StatusNotFound, "not found")
	case errors.Is(err, errConflict), errors.Is(err, errProbeRequired):
		writeError(writer, http.StatusConflict, "conflict")
	default:
		writeError(writer, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(writer http.ResponseWriter, status int, message string, data json.RawMessage) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(apiResponse{Code: status, Message: message, Data: data})
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(apiResponse{Code: status, Message: message})
}

func main() {
	address := flag.String("address", ":8092", "HTTP listen address")
	stateDir := flag.String("state-dir", "/var/lib/opskeeper-pool-fixture", "manifest state directory")
	flag.Parse()
	token := strings.TrimSpace(os.Getenv("POOL_FIXTURE_TOKEN"))
	dsn := strings.TrimSpace(os.Getenv("POOL_FIXTURE_DB_DSN"))
	controller, err := NewController(token, *stateDir, dsn, newPostgresRuntime, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pool-fixture: %v\n", err)
		os.Exit(1)
	}
	server := &http.Server{Addr: *address, Handler: NewHandler(controller), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "pool-fixture: %v\n", serveErr)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if err := controller.Shutdown(); err != nil {
		fmt.Fprintf(os.Stderr, "pool-fixture shutdown: %v\n", err)
		os.Exit(1)
	}
}
