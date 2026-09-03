// Package agentteams plugin CRUD HTTP endpoints.
//
// 路由（绑定到 chi.Router，由 bearer-protected sub-router 保护）：
//
//	GET    /v1/plugins                — 列出所有已安装 plugin
//	GET    /v1/plugins/{id}           — 单个 plugin 详情（plugin.yaml）
//	POST   /v1/plugins/install        — 上传 multipart zip 安装；自动解压并校验 manifest
//	DELETE /v1/plugins/{id}           — 卸载（删目录）
//	POST   /v1/plugins/{id}/enable    — 标记为 enabled（写 .status）
//	POST   /v1/plugins/{id}/disable   — 标记为 disabled（写 .status）
//	POST   /v1/plugins/{id}/sync      — 触发 worker 端 reload（POST {worker_id} 透传）
//	POST   /v1/plugins/{id}/push      — 推送已注册 plugin zip 到 worker (qwenpaw plugin install)
//
// 与 http.go 的区别：本文件聚焦"plugin 生命周期管理"，不涉及 state / hitl / skill 读写。
package agentteams

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	internalagentteams "github.com/vincent-wuhan/opskeeper/internal/agentteams"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
)

// PluginSyncClient 在 internal/agentteams 包定义（本文件复用）
type PluginSyncClient = internalagentteams.PluginSyncClient

// PluginLimits 控制 plugin 安装时的资源约束（与 Dashboard 的 server-package.ts 对齐）。
//
// DefaultMaxPluginZipBytes 保留为常量作为"编译期默认值"；运行时实际限制走 PluginHandler.maxZipBytes
// 字段（可通过 OPSKEEPER_PLUGIN_MAX_ZIP_BYTES env 或 helm values 覆盖）。
// MaxPluginExpandedSize / MaxPluginFileCount 留作未来 quota 校验（zip bomb 防护）使用。
const (
	DefaultMaxPluginZipBytes = 10 * 1024 * 1024 // 10 MB
	MaxPluginExpandedSize    = 50 * 1024 * 1024 // 50 MB
	MaxPluginFileCount       = 1000
)

// PluginHandler 是 plugin CRUD endpoints 的聚合。
type PluginHandler struct {
	registry    *PluginRegistry
	sync        PluginSyncClient
	log         *slog.Logger
	maxZipBytes int64 // 单个 plugin zip 上限（HTTP body + 持久化 .payload.zip 复用）
}

// NewPluginHandler 构造 PluginHandler。
//
// maxZipBytes <= 0 时使用 DefaultMaxPluginZipBytes（10MB），可被
// OPSKEEPER_PLUGIN_MAX_ZIP_BYTES env（opskeeper 端）或 helm values 覆盖。
func NewPluginHandler(registry *PluginRegistry, sync PluginSyncClient, log *slog.Logger, maxZipBytes int64) *PluginHandler {
	if maxZipBytes <= 0 {
		maxZipBytes = DefaultMaxPluginZipBytes
	}
	if log == nil {
		log = slog.Default()
	}
	return &PluginHandler{registry: registry, sync: sync, log: log, maxZipBytes: maxZipBytes}
}

// MaxZipBytes 返回 handler 当前配置的 zip 上限（bytes）。
func (h *PluginHandler) MaxZipBytes() int64 { return h.maxZipBytes }

// Register 路由注册。
func (h *PluginHandler) Register(r chi.Router) {
	r.Get("/v1/plugins", h.listPlugins)
	r.Get("/v1/plugins/{id}", h.getPlugin)
	r.Post("/v1/plugins/install", h.installPlugin)
	r.Delete("/v1/plugins/{id}", h.uninstallPlugin)
	r.Post("/v1/plugins/{id}/enable", h.enablePlugin)
	r.Post("/v1/plugins/{id}/disable", h.disablePlugin)
	r.Post("/v1/plugins/{id}/sync", h.syncPlugin)
	r.Post("/v1/plugins/{id}/push", h.pushPlugin)
}

// auditPluginEvent 写一条结构化审计日志。
//
// 关键字段：
//   - event=plugin_<action>           (install/uninstall/enable/disable/sync/list/get)
//   - plugin_id=<id>
//   - consumer=<from Bearer auth context, may be empty>
//   - + 任意 attrs (k/v 对)
//
// 日志走 slog.Warn 通道（opskeeper 既有 audit 约定），level 可由外部 logger 改写。
// 与 middleware/adapter/decorator/audit.go 的 AuditSink 是两套接口：
//   - 那个是 tool call 级别
//   - 这个是 plugin 生命周期级别
//
// 两者不冲突。
func (h *PluginHandler) auditPluginEvent(r *http.Request, action string, id string, attrs ...slog.Attr) {
	consumer := ""
	if ident, ok := mcpauth.FromContext(r.Context()); ok {
		consumer = ident.ConsumerName
	}
	all := make([]slog.Attr, 0, 3+len(attrs))
	all = append(all,
		slog.String("event", "plugin_"+action),
		slog.String("plugin_id", id),
		slog.String("consumer", consumer),
	)
	all = append(all, attrs...)
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "plugin_audit", all...)
}

// ----------------------------------------------------------------------
// GET /v1/plugins
// ----------------------------------------------------------------------

func (h *PluginHandler) listPlugins(w http.ResponseWriter, r *http.Request) {
	infos, err := h.registry.List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plugins": infos,
		"count":   len(infos),
	})
}

// ----------------------------------------------------------------------
// GET /v1/plugins/{id}
// ----------------------------------------------------------------------

func (h *PluginHandler) getPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	info, err := h.registry.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			writeJSONError(w, http.StatusNotFound, "plugin not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// ----------------------------------------------------------------------
// POST /v1/plugins/install (multipart/form-data: file=<zip>)
// ----------------------------------------------------------------------

func (h *PluginHandler) installPlugin(w http.ResponseWriter, r *http.Request) {
	// ?auto_push=true|false (default true) — install 成功后自动触发 worker push
	// 评审价值:用户上传 zip → 一次调用即可完成"安装 + 推 worker"
	// push 失败不回退 (用户可单独重试 /v1/plugins/{id}/push)
	autoPush := true
	if v := r.URL.Query().Get("auto_push"); v != "" {
		vv := strings.ToLower(strings.TrimSpace(v))
		if vv == "false" || vv == "0" || vv == "no" {
			autoPush = false
		}
	}

	// 10 MB 上限
	r.Body = http.MaxBytesReader(w, r.Body, h.maxZipBytes)
	if err := r.ParseMultipartForm(h.maxZipBytes); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing 'file' field: "+err.Error())
		return
	}
	defer file.Close()

	if header.Size > h.maxZipBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("zip too large: %d > %d", header.Size, h.maxZipBytes))
		return
	}

	zipBytes, err := io.ReadAll(file)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}

	pluginYAML, replace, err := extractPluginYAMLFromZip(zipBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 透传 caller identity 到 audit
	if id, ok := mcpauth.FromContext(r.Context()); ok {
		h.log.Info("plugin install requested", "consumer", id.ConsumerName, "filename", header.Filename, "replace", replace)
	}

	source := "upload:" + header.Filename
	var info PluginManifestInfo
	if replace {
		info, err = h.registry.Replace(r.Context(), InstallParams{
			Source:     source,
			PluginYAML: pluginYAML,
			ZipPayload: zipBytes,
		})
	} else {
		info, err = h.registry.Install(r.Context(), InstallParams{
			Source:     source,
			PluginYAML: pluginYAML,
			ZipPayload: zipBytes,
		})
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrPluginAlreadyExists):
			writeJSONError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrInvalidManifest):
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// Auto-push: install 成功后立即触发 worker push (retry-aware, 仅日志记错)
	var pushResult *installPushResult
	if autoPush {
		pushResult = h.maybeAutoPushAfterInstall(r.Context(), info.ID, zipBytes, r)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	h.auditPluginEvent(r, "install", info.ID,
		slog.String("source", source),
		slog.String("version", info.Version),
		slog.Bool("auto_push", autoPush),
		slog.Bool("pushed", pushResult != nil && pushResult.Pushed),
	)
	_ = json.NewEncoder(w).Encode(installResponse{
		PluginManifestInfo: info,
		AutoPush:           autoPush,
		PushResult:         pushResult,
	})
}

// installPushResult 描述 auto_push 触发的 worker push 结果。
//
// Pushed=true 表示至少一个 worker 收到 zip 并返回 2xx;
// Pushed=false 表示 0 worker / PluginSyncClient 未配置 / 所有 worker 都失败（partial / total）;
// Error 含失败原因字符串（仅当 Pushed=false 时有值）.
type installPushResult struct {
	Pushed bool   `json:"pushed"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

// installResponse 是 POST /v1/plugins/install 的 JSON 返回结构。
//
// PluginManifestInfo 含插件元信息（id/version/source/checksum/...）;
// AutoPush 镜像用户传入的 ?auto_push= 参数;
// PushResult 仅在 auto_push=true 时非空，含 push 成功与否。
type installResponse struct {
	PluginManifestInfo
	AutoPush   bool               `json:"auto_push"`
	PushResult *installPushResult `json:"push_result,omitempty"`
}

// maybeAutoPushAfterInstall 在 install 成功后触发 worker push，返回结果。
//
// 行为：
//   - h.sync 为空 → PluginSyncClient 未配置（stub 模式），pushed=false + reason
//   - PluginSyncClient.InstallPlugin 返回 err → pushed=false + error
//   - 成功 → pushed=true
//
// 失败不回退 install 结果（registry 已成功写入），用户可重试 /v1/plugins/{id}/push。
func (h *PluginHandler) maybeAutoPushAfterInstall(ctx context.Context, pluginID string, zipBytes []byte, r *http.Request) *installPushResult {
	if h.sync == nil {
		return &installPushResult{Pushed: false, Reason: "no PluginSyncClient configured"}
	}
	if err := h.sync.InstallPlugin(ctx, pluginID, zipBytes, pluginID+".zip"); err != nil {
		h.log.Warn("auto-push after install failed",
			"plugin", pluginID, "err", err.Error())
		h.auditPluginEvent(r, "push", pluginID,
			slog.Bool("pushed", false),
			slog.String("trigger", "auto_push"),
			slog.String("err", err.Error()),
		)
		return &installPushResult{Pushed: false, Error: err.Error()}
	}
	h.auditPluginEvent(r, "push", pluginID,
		slog.Bool("pushed", true),
		slog.String("trigger", "auto_push"),
	)
	return &installPushResult{Pushed: true}
}

// extractPluginYAMLFromZip 从 zip 内定位 plugin.yaml 并返回其字节。
// 校验：zip 合法、entry 数 ≤ MaxPluginFileCount、累计 uncompressed size ≤ MaxPluginExpandedSize。
func extractPluginYAMLFromZip(zipBytes []byte) ([]byte, bool, error) {
	if len(zipBytes) == 0 {
		return nil, false, errors.New("empty upload")
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, false, fmt.Errorf("invalid zip: %w", err)
	}
	if len(zr.File) > MaxPluginFileCount {
		return nil, false, fmt.Errorf("zip contains too many files: %d > %d", len(zr.File), MaxPluginFileCount)
	}

	var pluginYAML []byte
	var found bool
	var totalSize int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// zip-slip 防护
		clean := filepath.Clean(f.Name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "\\") {
			return nil, false, fmt.Errorf("unsafe entry path: %q", f.Name)
		}
		totalSize += int64(f.UncompressedSize64)
		if totalSize > MaxPluginExpandedSize {
			return nil, false, fmt.Errorf("expanded zip too large: %d > %d", totalSize, MaxPluginExpandedSize)
		}
		base := filepath.Base(f.Name)
		if base == "plugin.yaml" || base == "plugin.yml" {
			// 优先顶层目录中的 plugin.yaml（避免嵌套误读）
			dir := filepath.Dir(f.Name)
			if dir == "." || strings.Count(dir, "/") <= 1 {
				rc, err := f.Open()
				if err != nil {
					return nil, false, err
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return nil, false, err
				}
				pluginYAML = data
				found = true
				break
			}
		}
	}
	if !found {
		return nil, false, errors.New("plugin.yaml not found in zip (must be at top level or one level deep)")
	}

	// 解析 manifest；如果包含 # Installer metadata 则读取 replace 标志
	replace := false
	replaceRe := regexp.MustCompile(`(?m)^#\s*opskeeper-install:\s*replace\s*$`)
	if replaceRe.Match(pluginYAML) {
		replace = true
	}
	return pluginYAML, replace, nil
}

// ----------------------------------------------------------------------
// DELETE /v1/plugins/{id}
// ----------------------------------------------------------------------

func (h *PluginHandler) uninstallPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.auditPluginEvent(r, "uninstall", id)
	if err := h.registry.Uninstall(r.Context(), id); err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			writeJSONError(w, http.StatusNotFound, "plugin not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ident, ok := mcpauth.FromContext(r.Context()); ok {
		h.log.Info("plugin uninstalled", "consumer", ident.ConsumerName, "plugin", id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ----------------------------------------------------------------------
// POST /v1/plugins/{id}/enable | disable
// ----------------------------------------------------------------------

func (h *PluginHandler) enablePlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.auditPluginEvent(r, "enable", id)
	h.setStatusEndpoint(w, r, "enabled")
}

func (h *PluginHandler) disablePlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h.auditPluginEvent(r, "disable", id)
	h.setStatusEndpoint(w, r, "disabled")
}

func (h *PluginHandler) setStatusEndpoint(w http.ResponseWriter, r *http.Request, status string) {
	id := chi.URLParam(r, "id")
	if err := h.registry.SetStatus(r.Context(), id, status); err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			writeJSONError(w, http.StatusNotFound, "plugin not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	info, _ := h.registry.Get(r.Context(), id)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// ----------------------------------------------------------------------
// POST /v1/plugins/{id}/sync
// ----------------------------------------------------------------------

func (h *PluginHandler) syncPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.registry.Get(r.Context(), id); err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			writeJSONError(w, http.StatusNotFound, "plugin not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.sync == nil {
		// 没有 sync client（单机模式） — 仍然标记成功 + 写 status
		_ = h.registry.SetStatus(r.Context(), id, "enabled")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugin_id": id,
			"synced":    false,
			"reason":    "no PluginSyncClient configured",
		})
		return
	}
	if err := h.sync.SyncPlugin(r.Context(), id); err != nil {
		_ = h.registry.SetStatus(r.Context(), id, "error: "+err.Error())
		writeJSONError(w, http.StatusBadGateway, "sync failed: "+err.Error())
		return
	}
	_ = h.registry.SetStatus(r.Context(), id, "enabled")
	h.auditPluginEvent(r, "sync", id, slog.Bool("synced", true))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plugin_id": id,
		"synced":    true,
	})
}

// ----------------------------------------------------------------------
// POST /v1/plugins/{id}/push
//
// 把已注册的 plugin (id) 推送到所有 worker — 通过 PluginSyncClient.InstallPlugin
// 把 zip 内容 multipart 上传到 worker 的 qwenpaw install-plugin 端点。
//
// 与 syncPlugin 的区别: sync 是配置热重载（已装过的 plugin），push 是实际
// 把 zip 包分发到 worker 让 qwenpaw plugin install --force 执行。
//
// 错误聚合：任一 worker 失败返回 502，成功返回 200 + per-worker 状态。
// ----------------------------------------------------------------------

func (h *PluginHandler) pushPlugin(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	info, err := h.registry.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrPluginNotFound) {
			writeJSONError(w, http.StatusNotFound, "plugin not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	zipBytes, err := h.registry.ReadInstallPayload(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "plugin payload unavailable: "+err.Error())
		h.auditPluginEvent(r, "push", id, slog.Bool("pushed", false), slog.String("err", err.Error()))
		return
	}

	if h.sync == nil {
		h.auditPluginEvent(r, "push", id, slog.Bool("pushed", false), slog.String("reason", "no PluginSyncClient"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugin_id": id,
			"pushed":    false,
			"reason":    "no PluginSyncClient configured",
		})
		return
	}

	filename := info.ID + ".zip"
	if err := h.sync.InstallPlugin(r.Context(), id, zipBytes, filename); err != nil {
		_ = h.registry.SetStatus(r.Context(), id, "error: "+err.Error())
		h.auditPluginEvent(r, "push", id, slog.Bool("pushed", false), slog.String("err", err.Error()))
		writeJSONError(w, http.StatusBadGateway, "push failed: "+err.Error())
		return
	}

	_ = h.registry.SetStatus(r.Context(), id, "enabled")
	h.auditPluginEvent(r, "push", id, slog.Bool("pushed", true), slog.Int("bytes", len(zipBytes)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plugin_id": id,
		"pushed":    true,
		"bytes":     len(zipBytes),
	})
}

// 注：multipart 包保留以便未来扩展（当前用 r.FormFile）
var _ = multipart.ErrMessageTooLarge
