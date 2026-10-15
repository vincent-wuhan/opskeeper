package marketplace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The two halves of a registry, across a process boundary.
//
// Decision 465 shipped a producer (scripts/registryindex) and a consumer
// (RegistryIndexes) and proved they agree — but it proved it from the
// producer's own test, in the producer's process, by handing bytes straight
// to pluginmanifest.ParseIndex. That is a unit boundary check: it says the
// two pieces fit, and it says nothing about whether the command runs, exits
// zero, and writes a document a server can hand back unchanged.
//
// Those are three different failure modes with one interesting property:
// each of them produces a document that is *nearly* right. A producer that
// wrote to stderr instead of stdout still type-checks. A consumer that
// appended a newline still parses a hand-built Index. Only running the real
// command and serving its real output shows the seam.

func TestTheProducerCommandAndTheConsumerAgreeAcrossAProcess(t *testing.T) {
	doc := runRegistryIndexCommand(t)

	// Served exactly as the producer emitted it. No re-encoding, no
	// re-marshalling: a round trip through a Go struct would forgive a
	// producer that emitted something only a struct-shaped reader accepts.
	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(served))
	}))
	t.Cleanup(srv.Close)
	served = doc

	uc, _, _, _, _, _ := newTestUC(t)
	uc.cfg.RegistryIndexes = []RegistryIndex{{Name: "opskeeper-official", URL: srv.URL + "/index.json"}}

	entries, err := uc.Catalog(context.Background(), Caller{UserID: 1})
	if err != nil {
		t.Fatalf("Catalog over the producer's real output: %v", err)
	}

	// The packages this repository ships, by name. Asserting a count alone
	// would pass on a document that listed the right number of the wrong
	// things, and a catalogue of the wrong things is the failure this
	// exists to catch.
	want := map[string]string{
		"opskeeper-sre-autonomy":      "0.2.0",
		"opskeeper-sre-middleware":    "0.2.0",
		"opskeeper-sre-observability": "0.2.0",
		"opskeeper-sre-readonly":      "0.2.0",
		"opskeeper-sre-repair":        "0.2.0",
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.Version
	}
	for name, version := range want {
		if got[name] != version {
			t.Errorf("catalogue lists %s at %q, want %q (it came from the command's own output)",
				name, got[name], version)
		}
	}
	if len(entries) != len(want) {
		t.Errorf("catalogue has %d rows, want %d: %v", len(entries), len(want), got)
	}
}

// Every row must carry a digest, because the install path refuses a row
// without one and a catalogue whose rows cannot be installed is the state
// decision 465's author should have noticed and did not.
//
// The digest itself is checked here rather than in the producer's test
// because this is the first place the value has travelled anywhere.
func TestEveryRowTheProducerEmitsCarriesADigest(t *testing.T) {
	doc := runRegistryIndexCommand(t)

	var idx struct {
		Items []struct {
			Name   string `json:"name"`
			SHA256 string `json:"sha256"`
			URL    string `json:"url"`
		} `json:"items"`
	}
	if err := json.NewDecoder(bytes.NewReader([]byte(doc))).Decode(&idx); err != nil {
		t.Fatalf("the producer's output is not the document it claims to write: %v", err)
	}
	if len(idx.Items) == 0 {
		t.Fatal("the producer emitted no rows for a tree that ships five packages")
	}
	for _, item := range idx.Items {
		if item.SHA256 == "" {
			t.Errorf("%s carries no digest, so the install path would refuse it", item.Name)
		}
	}
}

// runRegistryIndexCommand runs the real producer and returns its stdout.
//
// -base-url is deliberately left off: the emitted document then has no urls,
// which is the shape a registry publishes before it knows its own address,
// and it is the shape that keeps this test from depending on a hostname.
func runRegistryIndexCommand(t *testing.T) string {
	t.Helper()
	repo := repoRootForTest(t)
	cmd := exec.Command("go", "run", "./scripts/registryindex")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./scripts/registryindex: %v\n%s", err, out)
	}
	doc := string(out)
	if !strings.Contains(doc, `"kind": "PluginIndex"`) {
		t.Fatalf("the command succeeded but did not write an index to stdout:\n%s", truncate(doc))
	}
	return doc
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// core/manager/biz/marketplace -> repo root
	return filepath.Clean(filepath.Join(wd, "..", "..", "..", ".."))
}

func truncate(s string) string {
	if len(s) > 800 {
		return s[:800] + "…"
	}
	return s
}
