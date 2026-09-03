package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	gitartifactapi "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/api"
	gitartifactstore "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"
	aiopstools "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	gitadapter "github.com/vincent-wuhan/opskeeper/internal/middleware/adapter/git"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

const runtimeLinkToolName = "git.find_runtime_link"

var runtimeLinkToolSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "symbol_type": {
      "type": "string",
      "enum": ["pg_query", "redis_cmd", "k8s_image", "http_route"]
    },
    "input": {
      "type": "object",
      "description": "Symbol-specific fields: query/database, cmd/key, image/tag, or method/path/handler.",
      "additionalProperties": true
    }
  },
  "required": ["symbol_type", "input"],
  "additionalProperties": false
}`)

// gitArtifactRuntime owns the shared store/index/linker graph used by both
// the HTTP protocol and the Agent BaseTool. The conversion lives in the
// composition root to avoid a manager -> middleware bounded-context import.
type gitArtifactRuntime struct {
	server *gitartifact.Server
	store  gitartifactstore.Store
	tool   aiopstoolsbase.BaseTool
}

// Store 返回底层 artifact store，供 chatdiagnose KB 等跨域消费者使用。
// chatdiagnose.DBGitArtifactLinker 通过此 store 做反查。
func (r *gitArtifactRuntime) Store() gitartifactstore.Store {
	return r.store
}

func newGitArtifactRuntime(storePath string, logger *slog.Logger) (*gitArtifactRuntime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	linkers := gitartifact.NewLinkerRegistry()
	for _, linker := range []gitartifact.Linker{
		gitartifact.NewPGQueryLinker(),
		gitartifact.NewRedisCmdLinker(),
		gitartifact.NewK8sImageLinker(),
		gitartifact.NewHTTPRouteLinker(),
	} {
		if err := linkers.Register(linker); err != nil {
			return nil, fmt.Errorf("register git-artifact linker %s: %w", linker.Type(), err)
		}
	}

	artifactStore, err := openGitArtifactStore(storePath)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*gitArtifactRuntime, error) {
		if closeErr := artifactStore.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (store close: %v)", cause, closeErr)
		}
		return nil, cause
	}

	indexer := gitartifactapi.NewIndexer(gitartifactapi.IndexerConfig{
		LinkerRegistry: linkers,
		Store:          artifactStore,
		Logger:         logger.With(slog.String("comp", "git-artifact-indexer")),
	})
	server := gitartifact.NewServer(linkers, logger.With(slog.String("comp", "git-artifact"))).
		WithStore(artifactStore).
		WithIndexer(indexer)

	adapterRegistry := middlewareregistry.NewRegistry()
	if err := gitadapter.RegisterTools(adapterRegistry, gitadapter.New(linkers)); err != nil {
		return fail(fmt.Errorf("register git adapter tools: %w", err))
	}
	runtimeLink, ok := adapterRegistry.GetTool(runtimeLinkToolName)
	if !ok {
		return fail(fmt.Errorf("git adapter missing %s", runtimeLinkToolName))
	}
	tool, err := aiopstools.NewCallbackTool(
		runtimeLink.Name,
		runtimeLink.Description,
		"A database query, Redis command, Kubernetes image, or HTTP route must be linked to its source commit and file line.",
		runtimeLinkToolSchema,
		"read",
		runtimeLink.Handler,
	)
	if err != nil {
		return fail(fmt.Errorf("build runtime link tool: %w", err))
	}
	return &gitArtifactRuntime{server: server, store: artifactStore, tool: tool}, nil
}

func openGitArtifactStore(path string) (gitartifactstore.Store, error) {
	if path == "" {
		return gitartifactstore.NewMemoryStore(), nil
	}
	store, err := gitartifactstore.NewJSONFileStore(path)
	if err != nil {
		return nil, fmt.Errorf("open git-artifact store %q: %w", path, err)
	}
	return store, nil
}

func (r *gitArtifactRuntime) RegisterProtected(router chi.Router) {
	if r == nil || r.server == nil {
		return
	}
	handler := r.server.Handler()
	router.Handle("/v1/git-artifacts", handler)
	router.Handle("/v1/git-artifacts/*", handler)
	router.Handle("/v1/runtime-link", handler)
}

func (r *gitArtifactRuntime) Close() error {
	if r == nil || r.store == nil {
		return nil
	}
	if err := r.store.Close(); err != nil {
		return fmt.Errorf("close git-artifact store: %w", err)
	}
	return nil
}
