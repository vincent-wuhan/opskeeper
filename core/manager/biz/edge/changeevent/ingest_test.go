package changeevent_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// TestIngestEncodesTheLabelsMapIntoTheTextColumn exists because a mutation
// proved nothing was checking it.
//
// The labels column is TEXT and the wire type carries map[string]string, so
// somebody has to JSON-encode on the way in — and until decision 281 that
// somebody was the tunnel handler, three packages away. Replacing
// MarshalLabels(e.Labels) with the empty string made every test in both
// packages pass. Not one of them had ever put a non-empty map on the wire:
// the one helper that builds an event in this file writes Labels: "{}" by
// hand, so it was asserting its own literal back.
//
// This test pins the conversion instead: a map in, the same map out of the
// column. Replacing the encoder with "" makes it red.
//
// What it does NOT establish is that labels are useful. parseLabelsJSON, the
// read side of the pair, has no production caller — the column is written and
// never read, which is also why the mutation was invisible. So this is a
// contract on the encoding, not evidence of a working feature, and the
// ledger records the missing reader as an open item.
func TestIngestEncodesTheLabelsMapIntoTheTextColumn(t *testing.T) {
	uc, db := newDB(t)
	want := map[string]string{
		"unit":          "nginx.service",
		"user":          "deploy",
		"_SYSTEMD_UNIT": "nginx.service",
	}
	if _, err := uc.Ingest(context.Background(), []domain.ChangeEventInput{{
		EdgeID:    7,
		Source:    "journald",
		Kind:      "service_restart",
		Subject:   "nginx.service",
		Action:    "restart",
		Timestamp: time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC),
		Severity:  "notice",
		Labels:    want,
	}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	var row edgemodel.ChangeEventRow
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := map[string]string{}
	if err := json.Unmarshal([]byte(row.Labels), &got); err != nil {
		t.Fatalf("the labels column %q is not a JSON object: %v", row.Labels, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels round-tripped to %v, want %v", got, want)
	}
}

// TestIngestStoresAnAbsentMapAsTheEmptyString pins the other half of the
// encoding, and it exists because the first version of this test asserted the
// opposite and was wrong.
//
// An event with no labels must land as the empty string, not as "{}" and not as
// "null". The column is TEXT and the two JSON spellings are both worse: "{}"
// claims a decode happened when none did, and "null" decodes to the same nil
// map that json.Unmarshal leaves behind on a genuine failure — so a reader
// could not tell a missing map from a corrupt column. labels.go already says
// this, and it says it deliberately: the column is best-effort metadata and
// the row is still valid without it.
//
// So the honest assertion is the one the code already implements, plus the
// reason it is defensible: a reader that must distinguish the two cases cannot,
// and the type says so out loud rather than leaving the next reader to
// discover it.
func TestIngestStoresAnAbsentMapAsTheEmptyString(t *testing.T) {
	uc, db := newDB(t)
	if _, err := uc.Ingest(context.Background(), []domain.ChangeEventInput{{
		EdgeID:    8,
		Source:    "dockerd",
		Kind:      "container_start",
		Subject:   "web",
		Action:    "start",
		Timestamp: time.Date(2026, 4, 23, 3, 12, 0, 0, time.UTC),
		Severity:  "info",
	}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	var row edgemodel.ChangeEventRow
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if row.Labels != "" {
		t.Errorf("labels column = %q, want the empty string: the column is TEXT and labels.go "+
			"declares empty and undecodable to be the same answer on purpose, so writing \"{}\" "+
			"or \"null\" would claim a decode that never happened", row.Labels)
	}
}
