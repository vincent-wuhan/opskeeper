package repairpreview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type WorkloadSpec struct {
	Revision       string            `yaml:"revision" json:"revision"`
	Seed           string            `yaml:"seed" json:"seed"`
	WarmupQueries  int               `yaml:"warmup_queries" json:"warmup_queries"`
	Samples        int               `yaml:"samples" json:"samples"`
	Concurrency    int               `yaml:"concurrency" json:"concurrency"`
	QueryTimeoutMS int               `yaml:"query_timeout_ms" json:"query_timeout_ms"`
	Labels         map[string]string `yaml:"labels" json:"labels"`
	Queries        []QuerySpec       `yaml:"queries" json:"queries"`
	BusinessProbe  BusinessProbeSpec `yaml:"business_probe" json:"business_probe"`
	Candidates     []CandidateSpec   `yaml:"candidates" json:"candidates"`
	RuntimeBinding WorkloadBinding   `yaml:"-" json:"-"`
}

type WorkloadBinding struct {
	RunID             string
	TenantID          string
	IncidentID        string
	ScenarioID        string
	IdempotencyKey    string
	TargetFingerprint string
}

type QuerySpec struct {
	Name string `yaml:"name" json:"name"`
	SQL  string `yaml:"sql" json:"sql"`
}

type BusinessProbeSpec struct {
	Name            string `yaml:"name" json:"name"`
	Query           string `yaml:"query" json:"query"`
	ExpectedMinimum int64  `yaml:"expected_minimum" json:"expected_minimum"`
}

type CandidateSpec struct {
	CandidateID   string `yaml:"candidate_id" json:"candidate_id"`
	Name          string `yaml:"name" json:"name"`
	Kind          string `yaml:"kind" json:"kind"`
	Action        string `yaml:"action" json:"action"`
	ChangeSummary string `yaml:"change_summary" json:"change_summary"`
}

func LoadWorkload(data []byte) (WorkloadSpec, error) {
	var spec WorkloadSpec
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return WorkloadSpec{}, fmt.Errorf("decode repair preview workload: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return WorkloadSpec{}, err
	}
	return spec, nil
}

func (spec WorkloadSpec) Validate() error {
	if spec.Revision == "" {
		return errors.New("repair preview workload: revision is required")
	}
	if spec.Seed == "" {
		return errors.New("repair preview workload: seed is required")
	}
	if spec.Samples <= 0 {
		return errors.New("repair preview workload: samples must be positive")
	}
	if spec.Concurrency <= 0 || spec.Concurrency > spec.Samples {
		return errors.New("repair preview workload: concurrency must be positive and no greater than samples")
	}
	if spec.QueryTimeoutMS <= 0 {
		return errors.New("repair preview workload: query timeout must be positive")
	}
	if spec.WarmupQueries < 0 {
		return errors.New("repair preview workload: warmup queries cannot be negative")
	}
	if len(spec.Queries) == 0 {
		return errors.New("repair preview workload: at least one query is required")
	}
	for name := range spec.Labels {
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, "dsn") || strings.Contains(lowerName, "password") ||
			strings.Contains(lowerName, "token") || strings.Contains(lowerName, "secret") ||
			strings.Contains(lowerName, "api_key") {
			return errors.New("repair preview workload: credential labels are not allowed")
		}
	}
	for index, query := range spec.Queries {
		if query.Name == "" || query.SQL == "" {
			return fmt.Errorf("repair preview workload: query %d name and sql are required", index)
		}
	}
	if spec.BusinessProbe.Name == "" || spec.BusinessProbe.Query == "" || spec.BusinessProbe.ExpectedMinimum < 0 {
		return errors.New("repair preview workload: complete business probe is required")
	}
	if len(spec.Candidates) == 0 {
		return errors.New("repair preview workload: at least one candidate is required")
	}
	for index, candidate := range spec.Candidates {
		if candidate.CandidateID == "" || candidate.Name == "" || candidate.Kind == "" ||
			candidate.Action == "" || candidate.ChangeSummary == "" {
			return fmt.Errorf("repair preview workload: candidate %d contract is incomplete", index)
		}
	}
	return nil
}

type workloadFingerprintMaterial struct {
	Revision       string            `json:"revision"`
	Seed           string            `json:"seed"`
	WarmupQueries  int               `json:"warmup_queries"`
	Samples        int               `json:"samples"`
	Concurrency    int               `json:"concurrency"`
	QueryTimeoutMS int               `json:"query_timeout_ms"`
	Labels         map[string]string `json:"labels,omitempty"`
	Queries        []QuerySpec       `json:"queries"`
	BusinessProbe  BusinessProbeSpec `json:"business_probe"`
	Candidates     []CandidateSpec   `json:"candidates"`
}

func (spec WorkloadSpec) WorkloadFingerprint() string {
	material := workloadFingerprintMaterial{
		Revision: spec.Revision, Seed: spec.Seed, WarmupQueries: spec.WarmupQueries,
		Samples: spec.Samples, Concurrency: spec.Concurrency, QueryTimeoutMS: spec.QueryTimeoutMS,
		Labels: spec.Labels, Queries: spec.Queries, BusinessProbe: spec.BusinessProbe,
		Candidates: spec.Candidates,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "sha256:unavailable"
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (spec WorkloadSpec) SeedFingerprint() string {
	if strings.HasPrefix(spec.Seed, "sha256:") {
		return spec.Seed
	}
	sum := sha256.Sum256([]byte(spec.Seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}
