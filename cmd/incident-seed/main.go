package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	incident "github.com/vincent-wuhan/opskeeper/internal/control/incident"
)

type seedResult struct {
	Datasets         int `json:"datasets"`
	TimelineEvents   int `json:"timeline_events"`
	TimelineInserted int `json:"timeline_inserted"`
	TimelineSkipped  int `json:"timeline_skipped"`
	RunbooksSaved    int `json:"runbooks_saved"`
}

func main() {
	dsn := flag.String("dsn", "", "PostgreSQL DSN (required)")
	dir := flag.String("dir", "deploy/incident-events", "incident dataset directory")
	dryRun := flag.Bool("dry-run", false, "validate files without writing to PostgreSQL")
	flag.Parse()

	if *dsn == "" && !*dryRun {
		fatal(fmt.Errorf("--dsn is required unless --dry-run is set"))
	}
	datasets, events, err := loadSeed(*dir)
	if err != nil {
		fatal(err)
	}
	result := seedResult{Datasets: len(datasets), TimelineEvents: len(events), RunbooksSaved: len(datasets)}
	if *dryRun {
		writeResult(result)
		return
	}

	db, err := gorm.Open(postgres.Open(*dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		fatal(fmt.Errorf("open postgres: %w", err))
	}
	repository := incident.NewSQLRepository(db)
	ctx := context.Background()
	existing, err := repository.ListTenant(ctx, seedTenant(datasets))
	if err != nil {
		fatal(fmt.Errorf("read existing timeline: %w", err))
	}
	existingIDs := make(map[string]bool, len(existing))
	for _, event := range existing {
		existingIDs[event.ID] = true
	}
	for _, event := range events {
		if existingIDs[event.ID] {
			result.TimelineSkipped++
			continue
		}
		if err := repository.Append(ctx, event); err != nil {
			fatal(fmt.Errorf("append event %s: %w", event.ID, err))
		}
		result.TimelineInserted++
	}
	for _, dataset := range datasets {
		if err := repository.SaveRunbook(ctx, dataset.Postmortem); err != nil {
			fatal(fmt.Errorf("save runbook %s: %w", dataset.ID, err))
		}
	}
	writeResult(result)
}

func loadSeed(dir string) ([]incident.Dataset, []incident.Event, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read dataset directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	datasets := make([]incident.Dataset, 0, len(names))
	events := make([]incident.Event, 0)
	for _, name := range names {
		dataset, err := readDataset(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		datasets = append(datasets, dataset)
		timelinePath := filepath.Join(dir, strings.TrimSuffix(name, ".json")+".timeline.jsonl")
		timeline, err := readTimeline(timelinePath)
		if err != nil {
			return nil, nil, err
		}
		events = append(events, timeline...)
	}
	return datasets, events, nil
}

func readDataset(path string) (incident.Dataset, error) {
	file, err := os.Open(path)
	if err != nil {
		return incident.Dataset{}, fmt.Errorf("open dataset: %w", err)
	}
	defer file.Close()
	var dataset incident.Dataset
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&dataset); err != nil {
		return incident.Dataset{}, fmt.Errorf("decode dataset %s: %w", path, err)
	}
	if err := dataset.Validate(); err != nil {
		return incident.Dataset{}, fmt.Errorf("validate dataset %s: %w", path, err)
	}
	return dataset, nil
}

func readTimeline(path string) ([]incident.Event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open timeline: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	events := make([]incident.Event, 0)
	line := 0
	decoder := json.NewDecoder(reader)
	for {
		line++
		var event incident.Event
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode timeline %s event %d: %w", path, line, err)
		}
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("validate timeline %s event %d: %w", path, line, err)
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("timeline %s is empty", path)
	}
	return events, nil
}

func seedTenant(datasets []incident.Dataset) string {
	if len(datasets) == 0 {
		return ""
	}
	tenant := datasets[0].Postmortem.TenantID
	for _, dataset := range datasets {
		if dataset.Postmortem.TenantID != tenant {
			return ""
		}
	}
	return tenant
}

func writeResult(result seedResult) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "incident-seed: %v\n", err)
	os.Exit(1)
}
