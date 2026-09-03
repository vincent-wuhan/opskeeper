// Command host-fixture owns the only CPU-load processes used by the
// host/cpu-spike demo. It never accepts a PID from a client.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	manifestSchemaVersion   = "v1"
	manifestStateRunning    = "running"
	manifestStateTerminated = "terminated"
	manifestStateStale      = "stale"
	baselineCPUPercent      = 12.0
	loadedCPUPercent        = 94.0
	maxRequestBodyBytes     = 16 << 10
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var targetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:._-]{0,254}$`)

type ownedGroup struct {
	manifest  ProcessManifest
	processes []*exec.Cmd
	timer     *time.Timer
}

type ProcessManifest struct {
	SchemaVersion      string     `json:"schema_version"`
	ManifestID         string     `json:"manifest_id"`
	CaseID             string     `json:"case_id"`
	IncidentID         string     `json:"incident_id"`
	Resource           string     `json:"resource"`
	ProcessCount       int        `json:"process_count"`
	OwnedProcessCount  int        `json:"owned_process_count"`
	Status             string     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	TerminatedAt       *time.Time `json:"terminated_at,omitempty"`
	BaselineCPUPercent float64    `json:"baseline_cpu_percent"`
	CurrentCPUPercent  float64    `json:"current_cpu_percent"`
}

type Controller struct {
	mu        sync.Mutex
	token     string
	stateDir  string
	groups    map[string]*ownedGroup
	groupKeys map[string]string
	log       *slog.Logger
}

func NewController(token, stateDir string, log *slog.Logger) (*Controller, error) {
	if len(token) < 32 {
		return nil, fmt.Errorf("runtime token must contain at least 32 characters")
	}
	if stateDir == "" {
		return nil, fmt.Errorf("state directory is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	controller := &Controller{
		token:     token,
		stateDir:  stateDir,
		groups:    make(map[string]*ownedGroup),
		groupKeys: make(map[string]string),
		log:       log,
	}
	if err := controller.loadManifests(); err != nil {
		return nil, err
	}
	return controller, nil
}

type StartRequest struct {
	CaseID     string `json:"case_id"`
	IncidentID string `json:"incident_id"`
	Count      int    `json:"count"`
	TTLSeconds int    `json:"ttl_seconds"`
}

func (c *Controller) Start(ctx context.Context, request StartRequest) (ProcessManifest, error) {
	if !identifierPattern.MatchString(request.CaseID) || !identifierPattern.MatchString(request.IncidentID) ||
		request.Count < 2 || request.Count > 4 || request.TTLSeconds < 60 || request.TTLSeconds > 86400 {
		return ProcessManifest{}, errInvalid
	}
	key := request.CaseID + "/" + request.IncidentID

	c.mu.Lock()
	defer c.mu.Unlock()
	if manifestID, ok := c.groupKeys[key]; ok {
		existing := c.groups[manifestID]
		if existing != nil {
			if err := c.expireIfDueLocked(existing, time.Now().UTC()); err != nil {
				return ProcessManifest{}, err
			}
		}
		if existing != nil && existing.manifest.Status == manifestStateRunning &&
			existing.manifest.ProcessCount == request.Count {
			return c.snapshotLocked(existing), nil
		}
		return ProcessManifest{}, errConflict
	}
	manifestID, err := newManifestID()
	if err != nil {
		return ProcessManifest{}, err
	}
	startedAt := time.Now().UTC()
	manifest := ProcessManifest{
		SchemaVersion:      manifestSchemaVersion,
		ManifestID:         manifestID,
		CaseID:             request.CaseID,
		IncidentID:         request.IncidentID,
		Resource:           "host:fixture",
		ProcessCount:       request.Count,
		OwnedProcessCount:  request.Count,
		Status:             manifestStateRunning,
		StartedAt:          startedAt,
		ExpiresAt:          startedAt.Add(time.Duration(request.TTLSeconds) * time.Second),
		BaselineCPUPercent: baselineCPUPercent,
		CurrentCPUPercent:  loadedCPUPercent,
	}
	group := &ownedGroup{manifest: manifest}
	for index := 0; index < request.Count; index++ {
		if err := ctx.Err(); err != nil {
			return ProcessManifest{}, err
		}
		worker := exec.Command(os.Args[0], "--cpu-worker")
		worker.Stdout = io.Discard
		worker.Stderr = io.Discard
		if err := worker.Start(); err != nil {
			_ = c.stopGroupLocked(group)
			return ProcessManifest{}, fmt.Errorf("start owned CPU process: %w", err)
		}
		group.processes = append(group.processes, worker)
	}
	group.timer = time.AfterFunc(time.Duration(request.TTLSeconds)*time.Second, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.expireIfDueLocked(group, time.Now().UTC()); err != nil {
			c.log.Error("expire fixture", "error", err)
		}
	})
	if err := c.persistLocked(group); err != nil {
		_ = c.stopGroupLocked(group)
		return ProcessManifest{}, err
	}
	c.groups[manifestID] = group
	c.groupKeys[key] = manifestID
	return c.snapshotLocked(group), nil
}

func (c *Controller) Status(manifestID string) (ProcessManifest, error) {
	if !targetPattern.MatchString(manifestID) {
		return ProcessManifest{}, errInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	group, ok := c.groups[manifestID]
	if !ok {
		return ProcessManifest{}, errNotFound
	}
	if err := c.expireIfDueLocked(group, time.Now().UTC()); err != nil {
		return ProcessManifest{}, err
	}
	return c.snapshotLocked(group), nil
}

func (c *Controller) Statuses() []ProcessManifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	statuses := make([]ProcessManifest, 0, len(c.groups))
	for _, group := range c.groups {
		if err := c.expireIfDueLocked(group, time.Now().UTC()); err != nil {
			c.log.Error("expire fixture", "error", err)
		}
		statuses = append(statuses, c.snapshotLocked(group))
	}
	return statuses
}

func (c *Controller) Terminate(manifestID string) (ProcessManifest, error) {
	if !targetPattern.MatchString(manifestID) {
		return ProcessManifest{}, errInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	group, ok := c.groups[manifestID]
	if !ok {
		return ProcessManifest{}, errNotFound
	}
	if err := c.expireIfDueLocked(group, time.Now().UTC()); err != nil {
		return ProcessManifest{}, err
	}
	if group.manifest.Status == manifestStateTerminated {
		return c.snapshotLocked(group), nil
	}
	if group.manifest.Status != manifestStateRunning {
		return ProcessManifest{}, errConflict
	}
	if err := c.stopGroupLocked(group); err != nil {
		return ProcessManifest{}, err
	}
	terminatedAt := time.Now().UTC()
	group.manifest.Status = manifestStateTerminated
	group.manifest.TerminatedAt = &terminatedAt
	group.manifest.OwnedProcessCount = 0
	group.manifest.CurrentCPUPercent = baselineCPUPercent
	if err := c.persistLocked(group); err != nil {
		return ProcessManifest{}, err
	}
	return c.snapshotLocked(group), nil
}

func (c *Controller) Shutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var combined error
	for _, group := range c.groups {
		if group.timer != nil {
			group.timer.Stop()
		}
		if group.manifest.Status != manifestStateRunning {
			continue
		}
		if err := c.stopGroupLocked(group); err != nil {
			combined = errors.Join(combined, err)
		}
		terminatedAt := time.Now().UTC()
		group.manifest.Status = manifestStateTerminated
		group.manifest.TerminatedAt = &terminatedAt
		group.manifest.OwnedProcessCount = 0
		group.manifest.CurrentCPUPercent = baselineCPUPercent
		if err := c.persistLocked(group); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (c *Controller) snapshotLocked(group *ownedGroup) ProcessManifest {
	manifest := group.manifest
	manifest.OwnedProcessCount = 0
	for _, process := range group.processes {
		if process != nil && process.Process != nil && process.ProcessState == nil {
			manifest.OwnedProcessCount++
		}
	}
	if manifest.Status == manifestStateRunning {
		if manifest.OwnedProcessCount == 0 {
			manifest.Status = manifestStateStale
			manifest.CurrentCPUPercent = baselineCPUPercent
		} else {
			manifest.CurrentCPUPercent = loadedCPUPercent
		}
	}
	return manifest
}

func (c *Controller) stopGroupLocked(group *ownedGroup) error {
	var combined error
	for _, process := range group.processes {
		if process == nil || process.Process == nil || process.ProcessState != nil {
			continue
		}
		if err := process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			combined = errors.Join(combined, err)
		}
		if _, err := process.Process.Wait(); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	group.processes = nil
	if group.timer != nil {
		group.timer.Stop()
		group.timer = nil
	}
	return combined
}

func (c *Controller) expireIfDueLocked(group *ownedGroup, now time.Time) error {
	if group.manifest.Status != manifestStateRunning || !now.After(group.manifest.ExpiresAt) {
		return nil
	}
	if err := c.stopGroupLocked(group); err != nil {
		return err
	}
	group.manifest.Status = manifestStateStale
	group.manifest.OwnedProcessCount = 0
	group.manifest.CurrentCPUPercent = baselineCPUPercent
	return c.persistLocked(group)
}

func (c *Controller) loadManifests() error {
	entries, err := os.ReadDir(c.stateDir)
	if err != nil {
		return fmt.Errorf("read fixture state: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.stateDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read fixture manifest: %w", err)
		}
		var manifest ProcessManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("decode fixture manifest %s: %w", entry.Name(), err)
		}
		if manifest.SchemaVersion != manifestSchemaVersion || manifest.ManifestID == "" ||
			manifest.CaseID == "" || manifest.IncidentID == "" || manifest.Resource == "" {
			return fmt.Errorf("invalid fixture manifest %s", entry.Name())
		}
		if manifest.Status == manifestStateRunning {
			manifest.Status = manifestStateStale
			manifest.OwnedProcessCount = 0
			manifest.CurrentCPUPercent = baselineCPUPercent
		}
		c.groups[manifest.ManifestID] = &ownedGroup{manifest: manifest}
		c.groupKeys[manifest.CaseID+"/"+manifest.IncidentID] = manifest.ManifestID
	}
	return nil
}

func (c *Controller) persistLocked(group *ownedGroup) error {
	data, err := json.MarshalIndent(group.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fixture manifest: %w", err)
	}
	filename := group.manifest.ManifestID + ".json"
	temporary := filepath.Join(c.stateDir, "."+filename+".tmp")
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write fixture manifest: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(c.stateDir, filename)); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace fixture manifest: %w", err)
	}
	return nil
}

func newManifestID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate fixture manifest id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

var (
	errInvalid   = errors.New("invalid fixture request")
	errNotFound  = errors.New("fixture process group not found")
	errConflict  = errors.New("fixture process group conflict")
	errForbidden = errors.New("fixture target mismatch")
)

type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type Handler struct {
	controller *Controller
}

func NewHandler(controller *Controller) *Handler {
	return &Handler{controller: controller}
}

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
	case request.Method == http.MethodPost && request.URL.Path == "/v1/fixtures":
		h.create(writer, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v1/fixtures/") &&
		strings.HasSuffix(request.URL.Path, "/terminate"):
		h.terminate(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/fixtures/") &&
		strings.HasSuffix(request.URL.Path, "/metrics"):
		h.fixtureMetrics(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/fixtures/"):
		h.status(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "not found")
	}
}

// create
// @Summary Create a case-owned CPU fixture
// @Tags host-fixture
// @Accept json
// @Produce json
// @Param request body main.StartRequest true "Fixture request"
// @Success 201 {object} main.apiResponse
// @Router /v1/fixtures [post]
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
	data, err := json.Marshal(manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, http.StatusCreated, "success", data)
}

// status
// @Summary Get a case-owned CPU fixture status
// @Tags host-fixture
// @Produce json
// @Success 200 {object} main.apiResponse
// @Router /v1/fixtures/{manifest_id} [get]
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
	data, err := json.Marshal(manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, http.StatusOK, "success", data)
}

// terminate
// @Summary Terminate only an incident-owned CPU fixture
// @Tags host-fixture
// @Accept json
// @Produce json
// @Success 200 {object} main.apiResponse
// @Router /v1/fixtures/{manifest_id}/terminate [post]
func (h *Handler) terminate(writer http.ResponseWriter, request *http.Request) {
	manifestID, ok := parseManifestPath(request.URL.Path, "/terminate")
	if !ok || !decodeEmptyBody(writer, request) {
		writeError(writer, http.StatusNotFound, "not found")
		return
	}
	manifest, err := h.controller.Terminate(manifestID)
	if err != nil {
		writeDomainError(writer, err)
		return
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, http.StatusOK, "success", data)
}

// metrics
// @Summary Export one case-owned fixture CPU metric samples
// @Tags host-fixture
// @Produce json
// @Success 200 {object} main.apiResponse
// @Router /v1/fixtures/{manifest_id}/metrics [get]
func (h *Handler) fixtureMetrics(writer http.ResponseWriter, request *http.Request) {
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
	data, err := json.Marshal(fixtureMetricsSnapshot{
		ManifestID:        manifest.ManifestID,
		Status:            manifest.Status,
		ProcessCount:      manifest.ProcessCount,
		OwnedProcessCount: manifest.OwnedProcessCount,
		CPUValues:         []float64{manifest.CurrentCPUPercent},
		CPUSampleSize:     1,
	})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "encode failed")
		return
	}
	writeJSON(writer, http.StatusOK, "success", data)
}

// @Summary Export case-owned fixture CPU metrics
// @Tags host-fixture
// @Produce text/plain
// @Success 200 {string} string
// @Router /metrics [get]
func (h *Handler) prometheusMetrics(writer http.ResponseWriter) {
	statuses := h.controller.Statuses()
	writer.Header().Set("Content-Type", `text/plain; version=0.0.4; charset=utf-8`)
	for _, status := range statuses {
		fmt.Fprintf(writer, "opskeeper_host_fixture_cpu_usage_percent{target=%q} %.2f\n",
			status.Resource, status.CurrentCPUPercent)
		fmt.Fprintf(writer, "opskeeper_host_fixture_owned_processes{target=%q} %d\n",
			status.Resource, status.OwnedProcessCount)
	}
}

type fixtureMetricsSnapshot struct {
	ManifestID        string    `json:"manifest_id"`
	Status            string    `json:"status"`
	ProcessCount      int       `json:"process_count"`
	OwnedProcessCount int       `json:"owned_process_count"`
	CPUValues         []float64 `json:"cpu_usage_percent"`
	CPUSampleSize     int       `json:"sample_size"`
}

func parseManifestPath(path, action string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "v1" || parts[1] != "fixtures" || parts[2] == "" {
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

func decodeEmptyBody(writer http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 1)
	_, err := io.ReadAll(request.Body)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "body must be empty")
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
	case errors.Is(err, errConflict):
		writeError(writer, http.StatusConflict, "conflict")
	default:
		writeError(writer, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(writer http.ResponseWriter, status int, message string, data json.RawMessage) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(apiResponse{Code: status, Message: message, Data: data}); err != nil {
		_, _ = writer.Write([]byte(`{"code":500,"message":"encode failed","data":null}`))
	}
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(apiResponse{Code: status, Message: message})
}

func runCPUWorker(ctx context.Context) error {
	value := uint64(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			for inner := 0; inner < 100_000; inner++ {
				value = value*1664525 + 1013904223
			}
			runtime.KeepAlive(value)
			runtime.Gosched()
		}
	}
}

func loadRuntimeToken(tokenFile string) (string, error) {
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("read runtime token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if len(token) < 32 || strings.ContainsAny(token, "\r\n") {
			return "", fmt.Errorf("runtime token file must contain one line of at least 32 characters")
		}
		return token, nil
	}
	token := strings.TrimSpace(os.Getenv("HOST_FIXTURE_TOKEN"))
	if token == "" {
		return "", fmt.Errorf("HOST_FIXTURE_TOKEN or --token-file is required")
	}
	if len(token) < 32 {
		return "", fmt.Errorf("runtime token must contain at least 32 characters")
	}
	return token, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--cpu-worker" {
		workerContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runCPUWorker(workerContext); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	address := flag.String("address", ":8091", "HTTP listen address")
	stateDir := flag.String("state-dir", "/tmp/opskeeper-host-fixture", "manifest state directory")
	tokenFile := flag.String("token-file", "", "runtime token file (0600, one line)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	token, err := loadRuntimeToken(*tokenFile)
	if err != nil {
		log.Error("host fixture token invalid", slog.Any("err", err))
		os.Exit(1)
	}
	controller, err := NewController(token, *stateDir, log)
	if err != nil {
		log.Error("host fixture init failed", slog.Any("err", err))
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr:              *address,
		Handler:           NewHandler(controller),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		log.Info("host fixture listening", slog.String("address", *address))
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("host fixture server failed", slog.Any("err", err))
			_ = controller.Shutdown()
			os.Exit(1)
		}
	case <-ctx.Done():
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Error("host fixture shutdown failed", slog.Any("err", err))
	}
	if err := controller.Shutdown(); err != nil {
		log.Error("host fixture process cleanup failed", slog.Any("err", err))
	}
}
