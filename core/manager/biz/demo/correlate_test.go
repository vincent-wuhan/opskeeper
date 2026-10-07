package demo

import (
	"context"
	"errors"
	"testing"

	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

type fakeCorrelations struct {
	calls       int
	fingerprint string
	labels      map[string]string
	incident    *alertmodel.Incident
	matched     bool
	err         error
}

func (f *fakeCorrelations) CorrelateFiring(_ context.Context, fingerprint string, labels map[string]string) (*alertmodel.Incident, bool, error) {
	f.calls++
	f.fingerprint = fingerprint
	f.labels = labels
	return f.incident, f.matched, f.err
}

// TestCorrelateFiringWithoutStorageIsANoOp pins the unwired answer. The
// alert ingest path calls this on every firing, so "no scenario storage
// wired" has to be indistinguishable from "no scenario owns this firing" —
// returning an error here would make the whole alert webhook fail on a
// platform that never configured the demo.
func TestCorrelateFiringWithoutStorageIsANoOp(t *testing.T) {
	uc := NewUsecase(nil, nil, nil)
	got, matched, err := uc.CorrelateFiring(context.Background(), "abc", map[string]string{"alertname": "X"})
	if err != nil {
		t.Fatalf("CorrelateFiring err = %v; want nil", err)
	}
	if matched || got != nil {
		t.Fatalf("unwired correlator claimed a firing: incident=%v matched=%t", got, matched)
	}
}

func TestCorrelateFiringDelegatesToScenarioStorage(t *testing.T) {
	want := &alertmodel.Incident{ID: 42, DedupeKey: "demo-scenario:abc"}
	store := &fakeCorrelations{incident: want, matched: true}
	uc := NewUsecase(nil, nil, nil)
	uc.SetFiringCorrelationRepository(store)

	labels := map[string]string{"pool_manifest_id": "manifest-1"}
	got, matched, err := uc.CorrelateFiring(context.Background(), "abc", labels)
	if err != nil {
		t.Fatalf("CorrelateFiring: %v", err)
	}
	if !matched || got != want {
		t.Fatalf("correlation = (%v, %t); want incident %d", got, matched, want.ID)
	}
	if store.calls != 1 || store.fingerprint != "abc" {
		t.Fatalf("store saw calls=%d fingerprint=%q; want 1, abc", store.calls, store.fingerprint)
	}
	// The labels have to arrive untouched: the demo recognises its own
	// firing by pool_manifest_id, so a copy would silently stop matching.
	if store.labels["pool_manifest_id"] != "manifest-1" {
		t.Fatalf("labels = %v; want the caller's map", store.labels)
	}
}

func TestCorrelateFiringPropagatesStorageError(t *testing.T) {
	wantErr := errors.New("scenario store unreachable")
	uc := NewUsecase(nil, nil, nil)
	uc.SetFiringCorrelationRepository(&fakeCorrelations{err: wantErr})

	if _, _, err := uc.CorrelateFiring(context.Background(), "abc", nil); !errors.Is(err, wantErr) {
		t.Fatalf("CorrelateFiring err = %v; want %v", err, wantErr)
	}
}
