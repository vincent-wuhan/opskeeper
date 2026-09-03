package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	incident "github.com/vincent-wuhan/opskeeper/internal/control/incident"
)

func main() {
	input := flag.String("in", "", "incident timeline JSONL file (default stdin)")
	flag.Parse()

	reader := os.Stdin
	if *input != "" {
		file, err := os.Open(*input)
		if err != nil {
			fatal(err)
		}
		defer file.Close()
		reader = file
	}

	events, err := readEvents(reader)
	if err != nil {
		fatal(err)
	}
	report, err := incident.ComputeReport(events)
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fatal(err)
	}
}

func readEvents(reader io.Reader) ([]incident.Event, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	events := make([]incident.Event, 0)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event incident.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode timeline event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read timeline: %w", err)
	}
	return events, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "incident-metrics: %v\n", err)
	os.Exit(1)
}
