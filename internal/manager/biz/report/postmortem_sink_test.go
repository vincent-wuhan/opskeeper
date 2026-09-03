package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	gitastore "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

func TestGitArtifactSink_Save_BuildsArtifactAndReturnsSHA(t *testing.T) {
	store := gitastore.NewMemoryStore()
	sink, err := NewGitArtifactSink(store)
	if err != nil {
		t.Fatalf("NewGitArtifactSink: %v", err)
	}
	sink.Now = func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }

	doc := &loop.PostmortemDoc{
		SchemaVersion: loop.ContractSchemaV1,
		IncidentID:    "INC-SINK-001",
		Markdown:      "# Test Postmortem\n\nBody here.\n",
		GeneratedAt:   time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		Sources:       []string{"RootCauseJSON", "CritiqueScore"},
	}
	sha, err := sink.Save(context.Background(), doc)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("SHA length = %d, want 40 (got %q)", len(sha), sha)
	}
	// 40 hex chars
	for _, c := range sha {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("SHA contains non-hex char %q in %q", c, sha)
		}
	}
	// Determinism: same input → same SHA.
	sha2, err := sink.Save(context.Background(), doc)
	if err != nil {
		t.Fatalf("Save2: %v", err)
	}
	if sha != sha2 {
		t.Errorf("SHA not deterministic: %q vs %q", sha, sha2)
	}
	// Different body → different SHA.
	doc.Markdown = "# Different\n\nBody."
	sha3, _ := sink.Save(context.Background(), doc)
	if sha3 == sha {
		t.Errorf("SHA collided for different bodies: %q == %q", sha, sha3)
	}
	// Store should have the artifact (the latest save wins).
	artifact, err := store.Get(context.Background(), sink.PublicIDPrefix+doc.IncidentID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if artifact.Commit != sha3 {
		t.Errorf("artifact.Commit = %q, want %q", artifact.Commit, sha3)
	}
	if artifact.Meta["incident_id"] != "INC-SINK-001" {
		t.Errorf("meta.incident_id = %v", artifact.Meta["incident_id"])
	}
	if artifact.Meta["schema_version"] != "v1" {
		t.Errorf("meta.schema_version = %v", artifact.Meta["schema_version"])
	}
	if artifact.Meta["sources"] == nil {
		t.Error("meta.sources missing")
	}
}

func TestGitArtifactSink_Save_RejectsNilDoc(t *testing.T) {
	store := gitastore.NewMemoryStore()
	sink, _ := NewGitArtifactSink(store)
	_, err := sink.Save(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil doc")
	}
}

func TestGitArtifactSink_Save_RejectsInvalidDoc(t *testing.T) {
	store := gitastore.NewMemoryStore()
	sink, _ := NewGitArtifactSink(store)
	doc := &loop.PostmortemDoc{IncidentID: "INC-002"} // invalid (empty markdown, no sources)
	_, err := sink.Save(context.Background(), doc)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGitArtifactSink_Save_RecordsRedactionInMeta(t *testing.T) {
	store := gitastore.NewMemoryStore()
	sink, _ := NewGitArtifactSink(store)
	doc := &loop.PostmortemDoc{
		SchemaVersion: loop.ContractSchemaV1,
		IncidentID:    "INC-REDACTED-001",
		Markdown:      "# Test\n\nuser_password=<redacted:password> email=<redacted:email>\n",
		GeneratedAt:   time.Now().UTC(),
		Sources:       []string{"RootCauseJSON"},
	}
	sha, err := sink.Save(context.Background(), doc)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	artifact, _ := store.Get(context.Background(), sink.PublicIDPrefix+doc.IncidentID)
	if artifact.Meta["redacted"] != true {
		t.Errorf("meta.redacted = %v, want true", artifact.Meta["redacted"])
	}
	if artifact.Meta["redaction_notice"] != "[REDACTED]" {
		t.Errorf("meta.redaction_notice = %v, want [REDACTED]", artifact.Meta["redaction_notice"])
	}
	if sha == "" {
		t.Error("SHA empty")
	}
}

func TestGitArtifactSink_NewRequiresStore(t *testing.T) {
	_, err := NewGitArtifactSink(nil)
	if err == nil {
		t.Error("expected error for nil store")
	}
}

// --- GitArtifactSourceResolver tests ---

func TestGitArtifactSourceResolver_Resolve_K8sHit(t *testing.T) {
	r := gitartifact.NewLinkerRegistry()
	l := gitartifact.NewK8sImageLinker()
	l.AddIndex("registry.example.com/order-svc:v1.2.3", &gitartifact.LinkResult{
		Commit:     "deadbeef5678901234deadbeef56789012345678",
		Repo:       "git@github.com:opskeeper/order-svc.git",
		FilePath:   "src/orders/svc.go",
		LineStart:  10,
		LineEnd:    20,
		Author:     "alice@opskeeper.io",
		Confidence: 0.95,
		TenantID:   1,
	})
	if err := r.Register(l); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewGitArtifactSourceResolver(r)
	if err != nil {
		t.Fatalf("NewGitArtifactSourceResolver: %v", err)
	}
	resolved, unmatched, err := resolver.Resolve(gitartifact.WithTenant(context.Background(), 1), 1, []SourceSelector{
		{Type: "k8s_image", Image: "registry.example.com/order-svc:v1.2.3", Tag: "v1.2.3"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved, got %d", len(resolved))
	}
	got := resolved[0]
	if got.CommitSHA != "deadbeef5678901234deadbeef56789012345678" {
		t.Errorf("CommitSHA = %q", got.CommitSHA)
	}
	if got.FilePath != "src/orders/svc.go" {
		t.Errorf("FilePath = %q", got.FilePath)
	}
	if got.BlameAuthor != "alice@opskeeper.io" {
		t.Errorf("BlameAuthor = %q", got.BlameAuthor)
	}
	if got.RuntimeKey == "" {
		t.Error("RuntimeKey empty")
	}
	if len(unmatched) != 0 {
		t.Errorf("expected 0 unmatched, got %d", len(unmatched))
	}
}

func TestGitArtifactSourceResolver_Resolve_NoHit(t *testing.T) {
	r := gitartifact.NewLinkerRegistry()
	resolver, _ := NewGitArtifactSourceResolver(r)
	_, unmatched, err := resolver.Resolve(context.Background(), 1, []SourceSelector{
		{Type: "k8s_image", Image: "registry/nonexistent:v1"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(unmatched) != 1 {
		t.Errorf("expected 1 unmatched, got %d", len(unmatched))
	}
}

func TestGitArtifactSourceResolver_Resolve_EmptyInput(t *testing.T) {
	r := gitartifact.NewLinkerRegistry()
	resolver, _ := NewGitArtifactSourceResolver(r)
	resolved, unmatched, err := resolver.Resolve(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 0 || len(unmatched) != 0 {
		t.Errorf("expected empty, got %d/%d", len(resolved), len(unmatched))
	}
}

func TestGitArtifactSourceResolver_Resolve_UnsupportedType(t *testing.T) {
	// Unsupported type is silently dropped by the resolver (the
	// gitartifact bridge does not know about it). The caller
	// (PostmortemService.Run) treats the absence of a resolved
	// row as "no attribution" without surfacing the selector.
	r := gitartifact.NewLinkerRegistry()
	resolver, _ := NewGitArtifactSourceResolver(r)
	resolved, unmatched, err := resolver.Resolve(context.Background(), 1, []SourceSelector{
		{Type: "unknown_type", Image: "x"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d", len(resolved))
	}
	if len(unmatched) != 0 {
		t.Errorf("expected 0 unmatched, got %d", len(unmatched))
	}
}

func TestGitArtifactSourceResolver_Resolve_NilRegistry(t *testing.T) {
	resolver, err := NewGitArtifactSourceResolver(nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	_ = resolver
}

func TestGitArtifactSourceResolver_Resolve_MultiSelectorMixedHit(t *testing.T) {
	r := gitartifact.NewLinkerRegistry()
	l := gitartifact.NewK8sImageLinker()
	l.AddIndex("registry/known:v1", &gitartifact.LinkResult{
		Commit:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FilePath:   "src/a.go",
		LineStart:  1,
		LineEnd:    2,
		Author:     "a@a",
		Confidence: 0.9,
		TenantID:   1,
	})
	if err := r.Register(l); err != nil {
		t.Fatal(err)
	}
	resolver, _ := NewGitArtifactSourceResolver(r)
	resolved, unmatched, err := resolver.Resolve(gitartifact.WithTenant(context.Background(), 1), 1, []SourceSelector{
		{Type: "k8s_image", Image: "registry/known:v1"},
		{Type: "k8s_image", Image: "registry/unknown:v1"},
		{Type: "pg_query", Query: "SELECT 1"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 {
		t.Errorf("expected 1 resolved, got %d", len(resolved))
	}
	if len(unmatched) != 2 {
		t.Errorf("expected 2 unmatched, got %d", len(unmatched))
	}
}

func TestSourceSelectorKey(t *testing.T) {
	cases := []struct {
		name string
		sel  SourceSelector
		want string
	}{
		{"k8s with tag", SourceSelector{Type: "k8s_image", Image: "r/x:v1", Tag: "v1"}, "k8s_image:r/x:v1:v1"},
		{"k8s no tag", SourceSelector{Type: "k8s_image", Image: "r/x:v1"}, "k8s_image:r/x:v1"},
		{"pg", SourceSelector{Type: "pg_query", Query: "SELECT 1"}, "pg_query:SELECT 1"},
		{"redis", SourceSelector{Type: "redis_cmd", Cmd: "SET", Key: "k"}, "redis_cmd:SET:k"},
		{"http", SourceSelector{Type: "http_route", Method: "GET", Path: "/x"}, "http_route:GET /x"},
		{"unknown", SourceSelector{Type: "foo"}, "foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceSelectorKey(tc.sel); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTenantIDFromString(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"", 0},
		{"0", 0},
		{"1", 1},
		{"123", 123},
		{"abc", 0}, // non-digit → 0
		{"12a", 0}, // mixed → 0
		{"9999999999", 9999999999},
	}
	for _, tc := range cases {
		if got := tenantIDFromString(tc.in); got != tc.want {
			t.Errorf("tenantIDFromString(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSeverityFromRootCause(t *testing.T) {
	cases := []struct {
		name string
		rc   *loop.RootCauseJSON
		want string
	}{
		{"nil", nil, "unknown"},
		{"no options", &loop.RootCauseJSON{}, "unknown"},
		{"safe", &loop.RootCauseJSON{RemediationOptions: []loop.RemediationOption{{Risk: "safe"}}}, "safe"},
		{"mutating", &loop.RootCauseJSON{RemediationOptions: []loop.RemediationOption{{Risk: "mutating"}}}, "mutating"},
		{"dangerous", &loop.RootCauseJSON{RemediationOptions: []loop.RemediationOption{{Risk: "dangerous"}}}, "dangerous"},
		{"max of mixed", &loop.RootCauseJSON{RemediationOptions: []loop.RemediationOption{{Risk: "safe"}, {Risk: "mutating"}, {Risk: "dangerous"}}}, "dangerous"},
		{"empty risk", &loop.RootCauseJSON{RemediationOptions: []loop.RemediationOption{{Risk: ""}}}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := severityFromRootCause(tc.rc); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abcdef1234567890"); got != "abcdef123456" {
		t.Errorf("shortSHA = %q", got)
	}
	if got := shortSHA("short"); got != "short" {
		t.Errorf("shortSHA = %q", got)
	}
	if got := shortSHA(""); got != "" {
		t.Errorf("shortSHA empty = %q", got)
	}
}

func TestBuildSelectorsFromRootCause_AllTypes(t *testing.T) {
	rc := &loop.RootCauseJSON{
		EvidenceChain: []loop.EvidenceItem{
			{Tool: "k8s_pod_image", Query: "registry/svc:v1"},
			{Tool: "pg_query", Query: "SELECT 1"},
			{Tool: "redis_cmd", Query: "SET key"},
			{Tool: "http_route", Query: "GET /orders"},
			{Tool: "unknown_tool", Query: "x"}, // ignored
		},
	}
	got := buildSelectorsFromRootCause(rc)
	if len(got) != 4 {
		t.Fatalf("expected 4 selectors, got %d: %+v", len(got), got)
	}
	// Verify each type.
	wantTypes := map[string]bool{"k8s_image": false, "pg_query": false, "redis_cmd": false, "http_route": false}
	for _, s := range got {
		if _, ok := wantTypes[s.Type]; ok {
			wantTypes[s.Type] = true
		}
	}
	for tp, found := range wantTypes {
		if !found {
			t.Errorf("missing type %q in selectors", tp)
		}
	}
}

func TestBuildSelectorsFromRootCause_NilAndEmpty(t *testing.T) {
	if got := buildSelectorsFromRootCause(nil); len(got) != 0 {
		t.Errorf("nil → %d", len(got))
	}
	rc := &loop.RootCauseJSON{EvidenceChain: nil}
	if got := buildSelectorsFromRootCause(rc); len(got) != 0 {
		t.Errorf("nil evidence → %d", len(got))
	}
	rc = &loop.RootCauseJSON{EvidenceChain: []loop.EvidenceItem{{Tool: ""}}}
	if got := buildSelectorsFromRootCause(rc); len(got) != 0 {
		t.Errorf("empty tool → %d", len(got))
	}
}

func TestFormatTime(t *testing.T) {
	if got := formatTime(time.Time{}); got != "(zero)" {
		t.Errorf("zero time = %q", got)
	}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if got := formatTime(now); !strings.Contains(got, "2026-08-10T12:00:00Z") {
		t.Errorf("formatted time = %q", got)
	}
}

func TestContainsAndIndexOf(t *testing.T) {
	if !contains("hello world", "world") {
		t.Error("contains should find 'world'")
	}
	if contains("hello", "") == false {
		t.Error("contains with empty sub should be true")
	}
	if contains("hello", "xyz") {
		t.Error("contains should not find 'xyz'")
	}
	if indexOf("hello", "ll") != 2 {
		t.Errorf("indexOf = %d", indexOf("hello", "ll"))
	}
	if indexOf("hello", "xyz") != -1 {
		t.Errorf("indexOf not found = %d", indexOf("hello", "xyz"))
	}
	if indexOf("hello", "") != 0 {
		t.Errorf("indexOf empty = %d", indexOf("hello", ""))
	}
}

func TestScanMarkdownField(t *testing.T) {
	md := "| **Severity** | mutating |\n| **Foo** | bar |\n"
	if got := scanMarkdownField(md, "Severity"); got != "mutating" {
		t.Errorf("got %q, want mutating", got)
	}
	if got := scanMarkdownField(md, "Foo"); got != "bar" {
		t.Errorf("got %q, want bar", got)
	}
	if got := scanMarkdownField(md, "Missing"); got != "" {
		t.Errorf("missing key = %q", got)
	}
}
