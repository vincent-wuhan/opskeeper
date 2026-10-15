package knowledge

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/vecstore"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/knowledge"
)

// chromemVec adapts the embedded backend to pointCounter so the shared e2e
// assertions below can run against it unchanged.
type chromemVec struct{ *vecstore.Chromem }

func (c chromemVec) count() int {
	res, err := c.Scroll(context.Background(), "opskeeper_knowledge", qdrantx.ScrollOpts{Limit: 1000})
	if err != nil {
		return -1
	}
	return len(res.Points)
}

// newChromemE2E builds the knowledge stack over the real embedded engine —
// the same one a single-binary deployment runs — so these tests cover the
// backend the embedded mode actually ships, not a hand-written fake.
func newChromemE2E(t *testing.T) (http.Handler, pointCounter) {
	t.Helper()
	store, err := vecstore.NewChromem("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("vecstore.NewChromem: %v", err)
	}
	if err := store.EnsureCollection(context.Background(), "opskeeper_knowledge", 8); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	return newE2EWith(t, store), chromemVec{store}
}

// TestE2E_EmbeddedDocLifecycle runs the manual-doc create→list→get→search→
// edit→delete loop against the embedded backend, mirroring
// TestE2E_ManualDocLifecycle. The fake covers the biz layer's own logic;
// this covers the engine underneath it, which is what a single-binary
// deployment depends on.
func TestE2E_EmbeddedDocLifecycle(t *testing.T) {
	router, store := newChromemE2E(t)

	rec := jsonReq(t, router, http.MethodPost, "/v1/knowledge/docs", map[string]any{
		"title":   "nginx 重启 SOP",
		"content": "# 重启\n\nsystemctl restart nginx",
		"path":    "网络/HTTP",
		"tags":    []string{"nginx", "sop"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeDoc(t, rec)
	if created.SourceType != model.SourceManual || created.ID == 0 {
		t.Fatalf("create: %+v", created)
	}

	rec = req(t, router, http.MethodGet, "/v1/knowledge/docs", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	if docs := decodeDocs(t, rec); len(docs) != 1 || docs[0].ID != created.ID {
		t.Fatalf("list: want [%d], got %+v", created.ID, docs)
	}

	rec = req(t, router, http.MethodGet, "/v1/knowledge/docs/"+idStr(created.ID), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d", rec.Code)
	}
	if got := decodeDoc(t, rec); !strings.Contains(got.Content, "systemctl restart nginx") {
		t.Fatalf("get: content missing, got %q", got.Content)
	}

	rec = req(t, router, http.MethodGet, "/v1/knowledge/search?q=nginx", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nginx 重启 SOP") {
		t.Fatalf("search: doc not in results: %s", rec.Body.String())
	}

	// Edit rewrites the points in place. This is the case that exercises
	// SetPayloadByFilter on the embedded backend, which has to rewrite each
	// document because chromem has no partial update.
	rec = jsonReq(t, router, http.MethodPatch, "/v1/knowledge/docs/"+idStr(created.ID), map[string]any{
		"title":   "nginx 重启 SOP v2",
		"content": "# v2\n\nsystemctl reload nginx",
		"path":    "网络/HTTP",
		"tags":    []string{"nginx"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = req(t, router, http.MethodGet, "/v1/knowledge/docs/"+idStr(created.ID), "", nil)
	if got := decodeDoc(t, rec); got.Title != "nginx 重启 SOP v2" || !strings.Contains(got.Content, "reload") {
		t.Fatalf("edit not persisted: %+v", got)
	}

	// Search after the edit must see the new metadata, not the stale
	// payload the document was first written with.
	rec = req(t, router, http.MethodGet, "/v1/knowledge/search?q=nginx", "", nil)
	if !strings.Contains(rec.Body.String(), "nginx 重启 SOP v2") {
		t.Fatalf("search after edit returned the stale title: %s", rec.Body.String())
	}

	rec = req(t, router, http.MethodDelete, "/v1/knowledge/docs/"+idStr(created.ID), "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := store.count(); n != 0 {
		t.Fatalf("delete: store still has %d points", n)
	}
	if docs := decodeDocs(t, req(t, router, http.MethodGet, "/v1/knowledge/docs", "", nil)); len(docs) != 0 {
		t.Fatalf("after delete, list should be empty, got %+v", docs)
	}
}

// TestE2E_EmbeddedUploadLifecycle is the multi-chunk case against the
// embedded backend: an uploaded doc is split into several points, and the
// edit path has to sweep the stale chunks out of chromem as well.
func TestE2E_EmbeddedUploadLifecycle(t *testing.T) {
	router, store := newChromemE2E(t)

	longBody := "# 技术行业为什么总在发明新词\n\n" + strings.Repeat("观测性运维平台工程。", 600)
	ct, body := buildUpload(t, "技术行业为什么总在发明新词.md", longBody, "foo", "dns, resolv")
	rec := req(t, router, http.MethodPost, "/v1/knowledge/upload", ct, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	up := decodeDoc(t, rec)
	if up.SourceType != model.SourceUpload {
		t.Fatalf("upload: source=%s want upload", up.SourceType)
	}
	if n := store.count(); n < 2 {
		t.Fatalf("upload: long body should chunk into ≥2 points, got %d", n)
	}

	if docs := decodeDocs(t, req(t, router, http.MethodGet, "/v1/knowledge/docs", "", nil)); len(docs) != 1 {
		t.Fatalf("list: want one upload row, got %+v", docs)
	}

	rec = jsonReq(t, router, http.MethodPatch, "/v1/knowledge/docs/"+idStr(up.ID), map[string]any{
		"title":   "技术行业为什么总在发明新词",
		"content": "# 短内容\n\n精简后的正文",
		"path":    "foo",
		"tags":    []string{"dns", "resolv"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit upload: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := store.count(); n != 1 {
		t.Fatalf("edit upload: stale chunks not swept, store has %d points", n)
	}

	rec = req(t, router, http.MethodDelete, "/v1/knowledge/docs/"+idStr(up.ID), "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete upload: want 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := store.count(); n != 0 {
		t.Fatalf("delete upload: store still has %d points", n)
	}
}
