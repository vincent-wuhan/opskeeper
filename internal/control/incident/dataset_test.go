package incident

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIncidentDatasets_FourPrototypes_AreComplete(t *testing.T) {
	datasets := loadScenarioFiles(t)
	require.Len(t, datasets, 4)

	expected := []string{
		"pg-connection-pool-exhaustion",
		"pg-disk-io-saturation",
		"pg-lock-wait-long-transaction",
		"pg-replica-replay-lag",
	}
	ids := make([]string, 0, len(datasets))
	for _, dataset := range datasets {
		require.NoError(t, dataset.Validate(), dataset.ID)
		require.NotEmpty(t, dataset.Input.Alert.TraceID)
		require.GreaterOrEqual(t, len(dataset.Input.MetricWindow), 3)
		require.NotEmpty(t, dataset.Input.SessionSnapshot.Summary)
		require.NotEmpty(t, dataset.Input.LockSnapshot.Summary)
		require.NotEmpty(t, dataset.Input.Topology.Fingerprint)
		require.NotEmpty(t, dataset.Input.Versions.Engine)
		ids = append(ids, dataset.ID)
	}
	sort.Strings(ids)
	require.Equal(t, expected, ids)
}

func TestIncidentDatasetTimelines_CanReplayAndComputeMetrics(t *testing.T) {
	datasets := loadScenarioFiles(t)
	for _, dataset := range datasets {
		timelinePath := filepath.Join("..", "..", "..", "deploy", "incident-events", dataset.ID+".timeline.jsonl")
		events := readTimelineFile(t, timelinePath)
		require.NotEmpty(t, events)

		report, err := ComputeReport(events)
		require.NoError(t, err, dataset.ID)
		require.Equal(t, 1, report.IncidentCount, dataset.ID)
		require.Equal(t, 0, report.WrongClosureCount, dataset.ID)
		require.Equal(t, 0, report.RepeatedActionCount, dataset.ID)
		require.Equal(t, float64(1), report.RecommendationSuccessRate, dataset.ID)
		require.Equal(t, float64(1), report.AuditEvidenceCompleteness, dataset.ID)

		var recovery *Event
		var closed *Event
		for index := range events {
			switch events[index].EventType {
			case EventRecovery:
				recovery = &events[index]
			case EventClosed:
				closed = &events[index]
			}
		}
		require.NotNil(t, recovery, dataset.ID)
		require.NotNil(t, closed, dataset.ID)
		require.True(t, recovery.RecoverySignal, dataset.ID)
		require.True(t, recovery.OccurredAt.Before(closed.OccurredAt), dataset.ID)
	}
}

func TestIncidentDatasetPostmortems_CanPersistAndAuditRRFRecall(t *testing.T) {
	repository, _ := setupRepository(t)
	datasets := loadScenarioFiles(t)
	recalledAt := time.Date(2026, 8, 29, 19, 0, 0, 0, time.UTC)
	for _, dataset := range datasets {
		require.NoError(t, repository.SaveRunbook(context.Background(), dataset.Postmortem), dataset.ID)
		stored, err := repository.ListRunbooks(
			context.Background(), dataset.Postmortem.TenantID,
			dataset.Postmortem.DatabaseType, dataset.Postmortem.FaultFingerprint,
		)
		require.NoError(t, err, dataset.ID)
		require.Len(t, stored, 1, dataset.ID)

		candidates, err := SelectTopRRF([]RankedCandidate{
			{CandidateRef: "runbook:" + dataset.Postmortem.IncidentID, VectorRank: 1, KeywordRank: 2},
			{CandidateRef: "runbook:generic-database", VectorRank: 3, KeywordRank: 1},
		})
		require.NoError(t, err, dataset.ID)
		logs := make([]RecallLog, 0, len(candidates))
		for _, candidate := range candidates {
			logs = append(logs, RecallLog{
				TenantID: dataset.Postmortem.TenantID, IncidentID: "RECALL-" + dataset.Postmortem.IncidentID,
				QueryText: dataset.Input.Alert.Summary, CandidateRef: candidate.CandidateRef,
				VectorRank: candidate.VectorRank, KeywordRank: candidate.KeywordRank,
				RRFScore: candidate.RRFScore, Selected: candidate.Selected,
				RejectedReason: candidate.RejectedReason, RecalledAt: recalledAt,
			})
		}
		require.NoError(t, repository.AppendRecallLogs(context.Background(), logs), dataset.ID)
		storedLogs, err := repository.ListRecallLogs(
			context.Background(), dataset.Postmortem.TenantID, "RECALL-"+dataset.Postmortem.IncidentID,
		)
		require.NoError(t, err, dataset.ID)
		require.Len(t, storedLogs, 2, dataset.ID)
		require.True(t, storedLogs[0].Selected, dataset.ID)
		require.Equal(t, "runbook:"+dataset.Postmortem.IncidentID, storedLogs[0].CandidateRef, dataset.ID)
	}
}

func loadScenarioFiles(t *testing.T) []Dataset {
	t.Helper()
	directory := filepath.Join("..", "..", "..", "deploy", "incident-events")
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)

	datasets := make([]Dataset, 0, len(entries))
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		require.NoError(t, err)
		var dataset Dataset
		require.NoError(t, json.Unmarshal(content, &dataset), entry.Name())
		datasets = append(datasets, dataset)
	}
	return datasets
}

func readTimelineFile(t *testing.T, path string) []Event {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := splitLines(content)
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		var event Event
		require.NoError(t, json.Unmarshal(line, &event), path)
		events = append(events, event)
	}
	return events
}

func splitLines(content []byte) [][]byte {
	lines := make([][]byte, 0)
	start := 0
	for index, character := range content {
		if character == '\n' {
			if index > start {
				lines = append(lines, content[start:index])
			}
			start = index + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

func TestDatasetValidation_MissingInput_IsRejected(t *testing.T) {
	dataset := Dataset{ID: "invalid", Title: "invalid", FaultFamily: "invalid", SourceBasis: "mock", Assumptions: []string{"test"}, CreatedAt: mustTime()}
	err := dataset.Validate()
	require.ErrorContains(t, err, "alert")
}

func mustTime() time.Time {
	return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
}
