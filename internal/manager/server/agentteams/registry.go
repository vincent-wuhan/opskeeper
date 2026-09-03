// Package agentteams plugin registry — filesystem-based plugin index.
//
// 目录约定：${OPSKEEPER_PLUGINS_DIR}/<id>/
//
//	├── plugin.yaml          // 原 v1alpha1 manifest（用户上传的 zip 里的）
//	├── manifest.json        // 抽出的 metadata + 安装时间（便于快速 list）
//	├── content/             // 整个 v1alpha1 plugin tree 展开（可选）
//	├── .installed_at        // RFC3339 时间戳
//	└── .status              // enabled | disabled | error:<msg>
//
// Dashboard 通过 /v1/plugins/* endpoint 读写此目录；Worker 端不感知此 dir。
package agentteams

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// PluginManifestInfo 是 list endpoint 返回的 metadata 摘要。
type PluginManifestInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	APIVersion  string    `json:"apiVersion,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	ToolCount   int       `json:"tool_count"`
	SkillCount  int       `json:"skill_count"`
	PromptCount int       `json:"prompt_count"`
	AdapterIDs  []string  `json:"adapter_ids"`
	Status      string    `json:"status"` // installed | enabled | disabled | error
	InstalledAt time.Time `json:"installed_at"`
	Source      string    `json:"source,omitempty"`
	ContentDir  string    `json:"content_dir"`
	Checksum    string    `json:"checksum"`
}

// ErrPluginNotFound 由 Get/Uninstall/SetStatus 在缺失时返回。
var ErrPluginNotFound = errors.New("plugin not found")

// ErrPluginAlreadyExists 由 Install 在同名已存在时返回。
var ErrPluginAlreadyExists = errors.New("plugin already exists")

// ErrInvalidManifest 由 Install 在 manifest 不合法时返回。
var ErrInvalidManifest = errors.New("invalid plugin manifest")

var pluginIDRe = regexp.MustCompile(`^[a-z][a-z0-9-_]{0,63}$`)

// v1alpha1ManifestRaw 是 Install 时解析 plugin.yaml 的最小子集。
type v1alpha1ManifestRaw struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string `yaml:"name"`
		Version     string `yaml:"version"`
		Description string `yaml:"description"`
	} `yaml:"metadata"`
	MCP      map[string]any `yaml:"mcp"`
	Skills   map[string]any `yaml:"skills"`
	Prompts  map[string]any `yaml:"prompts"`
	Adapters []struct {
		ID string `yaml:"id"`
	} `yaml:"adapters"`
}

// PluginRegistry 操作 ${pluginsDir}/<id>/ 的文件树。
type PluginRegistry struct {
	pluginsDir string
	mu         sync.Mutex
}

// NewPluginRegistry 构造；pluginsDir 为空时使用 /var/lib/opskeeper/plugins。
func NewPluginRegistry(pluginsDir string) *PluginRegistry {
	if pluginsDir == "" {
		pluginsDir = "/var/lib/opskeeper/plugins"
	}
	return &PluginRegistry{pluginsDir: pluginsDir}
}

// PluginsDir 返回 plugins 根目录。
func (r *PluginRegistry) PluginsDir() string { return r.pluginsDir }

// List 返回所有已安装 plugin 的 metadata 摘要（按 id 排序）。
func (r *PluginRegistry) List(_ context.Context) ([]PluginManifestInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir plugins: %w", err)
	}
	entries, err := os.ReadDir(r.pluginsDir)
	if err != nil {
		return nil, err
	}
	var out []PluginManifestInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := r.readInfo(filepath.Join(r.pluginsDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get 返回单个 plugin 详情；不存在返回 ErrPluginNotFound。
func (r *PluginRegistry) Get(_ context.Context, id string) (PluginManifestInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !pluginIDRe.MatchString(id) {
		return PluginManifestInfo{}, fmt.Errorf("%w: invalid id %q", ErrPluginNotFound, id)
	}
	return r.readInfo(filepath.Join(r.pluginsDir, id))
}

// Uninstall 删除整个 <id> 目录。
func (r *PluginRegistry) Uninstall(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !pluginIDRe.MatchString(id) {
		return ErrPluginNotFound
	}
	dir := filepath.Join(r.pluginsDir, id)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return ErrPluginNotFound
	}
	return os.RemoveAll(dir)
}

// SetStatus 写一个 .status 文件。
func (r *PluginRegistry) SetStatus(_ context.Context, id, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.pluginsDir, id)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return ErrPluginNotFound
	}
	return os.WriteFile(filepath.Join(dir, ".status"), []byte(status), 0o644)
}

// InstallParams 是 Install 的输入。
type InstallParams struct {
	ID         string // 强制使用此 id（必须与 manifest.metadata.name 一致）
	Source     string // 描述来源（上传的 zip 路径 / url），写入 manifest.json
	PluginYAML []byte // plugin.yaml 内容（v1alpha1 manifest）
	ZipPayload []byte // 原始 zip 内容（用于后续 push 到 worker；可空）
}

// ReadInstallPayload 返回该 plugin 持久化的原始 zip 字节（用于 push 到 worker）。
//
// 当 plugin 是从本地目录 / URL 安装而非 zip 上传时（ZipPayload 为空），返回
// (nil, nil) — 调用方应跳过 push 而改用其它途径（例如 baked-in image）。
func (r *PluginRegistry) ReadInstallPayload(_ context.Context, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := filepath.Join(r.pluginsDir, id, ".payload.zip")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// Install 校验 + 写入一个新的 plugin entry。已存在返回 ErrPluginAlreadyExists。
func (r *PluginRegistry) Install(_ context.Context, p InstallParams) (PluginManifestInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	manifest, err := parseV1alpha1Manifest(p.PluginYAML)
	if err != nil {
		return PluginManifestInfo{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if p.ID != "" && p.ID != manifest.Metadata.Name {
		return PluginManifestInfo{}, fmt.Errorf("%w: requested id=%q but manifest says %q",
			ErrInvalidManifest, p.ID, manifest.Metadata.Name)
	}
	if !pluginIDRe.MatchString(manifest.Metadata.Name) {
		return PluginManifestInfo{}, fmt.Errorf("%w: invalid id %q", ErrInvalidManifest, manifest.Metadata.Name)
	}
	id := manifest.Metadata.Name
	dir := filepath.Join(r.pluginsDir, id)
	if _, err := os.Stat(dir); err == nil {
		return PluginManifestInfo{}, ErrPluginAlreadyExists
	}
	// P2-1 事务回滚：defer 在任何错误路径自动清理半成品目录（包括 panic）
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PluginManifestInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), p.PluginYAML, 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return PluginManifestInfo{}, err
	}
	info := manifestInfoFromV1alpha1(manifest)
	info.InstalledAt = time.Now().UTC()
	info.Status = "installed"
	info.Source = p.Source
	info.ContentDir = filepath.Join(dir, "content")
	info.Checksum = sha256Hex(p.PluginYAML)
	if err := writeManifestJSON(filepath.Join(dir, "manifest.json"), info); err != nil {
		_ = os.RemoveAll(dir)
		return PluginManifestInfo{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ".installed_at"),
		[]byte(info.InstalledAt.Format(time.RFC3339)), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return PluginManifestInfo{}, err
	}
	// 写 content sha256 marker（用于启动期幂等比对）
	cs, csErr := ContentChecksum(dir)
	if csErr == nil {
		if err := writeContentMarker(dir, cs); err != nil {
			return PluginManifestInfo{}, err
		}
	} else {
		return PluginManifestInfo{}, csErr
	}

	// 持久化原始 zip（用于后续 /v1/plugins/{id}/push 推到 worker）。
	// 写到 pluginDir/.payload.zip，与 manifest.json 同级；卸载时由 Uninstall 清理。
	if len(p.ZipPayload) > 0 {
		payloadPath := filepath.Join(dir, ".payload.zip")
		if err := os.WriteFile(payloadPath, p.ZipPayload, 0o644); err != nil {
			return PluginManifestInfo{}, err
		}
	}

	committed = true
	return info, nil
}

// Replace 强制覆盖（uninstall 后再 install）。
func (r *PluginRegistry) Replace(ctx context.Context, p InstallParams) (PluginManifestInfo, error) {
	if p.ID == "" {
		manifest, err := parseV1alpha1Manifest(p.PluginYAML)
		if err != nil {
			return PluginManifestInfo{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
		}
		p.ID = manifest.Metadata.Name
	}
	_ = r.Uninstall(ctx, p.ID)
	return r.Install(ctx, p)
}

// readInfo 从 <dir>/manifest.json 读；缺失则从 <dir>/plugin.yaml 重新解析。
func (r *PluginRegistry) readInfo(dir string) (PluginManifestInfo, error) {
	mfPath := filepath.Join(dir, "manifest.json")
	if data, err := os.ReadFile(mfPath); err == nil {
		var info PluginManifestInfo
		if err := json.Unmarshal(data, &info); err == nil {
			if s, err := os.ReadFile(filepath.Join(dir, ".status")); err == nil {
				info.Status = strings.TrimSpace(string(s))
			}
			return info, nil
		}
	}
	yamlBytes, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return PluginManifestInfo{}, ErrPluginNotFound
	}
	manifest, err := parseV1alpha1Manifest(yamlBytes)
	if err != nil {
		return PluginManifestInfo{}, err
	}
	info := manifestInfoFromV1alpha1(manifest)
	info.ContentDir = filepath.Join(dir, "content")
	info.Checksum = sha256Hex(yamlBytes)
	if ts, err := os.ReadFile(filepath.Join(dir, ".installed_at")); err == nil {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(ts)))
		if err == nil {
			info.InstalledAt = t
		}
	}
	return info, nil
}

func parseV1alpha1Manifest(b []byte) (v1alpha1ManifestRaw, error) {
	var m v1alpha1ManifestRaw
	if err := yaml.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("yaml parse: %w", err)
	}
	if m.APIVersion != "agentteams.agentteam/v1alpha1" {
		return m, fmt.Errorf("apiVersion must be agentteams.agentteam/v1alpha1, got %q", m.APIVersion)
	}
	if m.Kind != "AgentTeamPlugin" {
		return m, fmt.Errorf("kind must be AgentTeamPlugin, got %q", m.Kind)
	}
	if m.Metadata.Name == "" {
		return m, fmt.Errorf("metadata.name is required")
	}
	if m.Metadata.Version == "" {
		return m, fmt.Errorf("metadata.version is required")
	}
	return m, nil
}

func manifestInfoFromV1alpha1(m v1alpha1ManifestRaw) PluginManifestInfo {
	info := PluginManifestInfo{
		ID:          m.Metadata.Name,
		Name:        m.Metadata.Name,
		Version:     m.Metadata.Version,
		Description: m.Metadata.Description,
		APIVersion:  m.APIVersion,
		Kind:        m.Kind,
	}
	if servers, ok := m.MCP["servers"].([]any); ok {
		for _, s := range servers {
			if sm, ok := s.(map[string]any); ok {
				if tools, ok := sm["tools"].([]any); ok {
					info.ToolCount += len(tools)
				}
			}
		}
	}
	for _, v := range m.Skills {
		if arr, ok := v.([]any); ok {
			info.SkillCount += len(arr)
		}
	}
	for _, v := range m.Prompts {
		switch vv := v.(type) {
		case []any:
			info.PromptCount += len(vv)
		case map[string]any:
			info.PromptCount += len(vv)
		default:
			// scalar string (e.g. prompts.team: prompts/team/T.md) 仍计 1
			info.PromptCount++
		}
	}
	for _, a := range m.Adapters {
		if a.ID != "" {
			info.AdapterIDs = append(info.AdapterIDs, a.ID)
		}
	}
	return info
}

func writeManifestJSON(path string, info PluginManifestInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// PluginChecksumAlgo 版本号：当前 sha256 of (plugin.yaml + 所有 file content)
const PluginChecksumAlgo = "sha256:yaml+files"

// ContentChecksum 计算 <dir>/ 整个 plugin tree 的 sha256:
//
//	sha256(<plugin.yaml bytes>) || sha256(<filename>||\0||<file bytes>) for each file
//
// file walk 顺序按 sorted relative path,确保可重现。
// 排除 .installed_at / .status / .content.sha256 / manifest.json(由 registry 维护)。
func ContentChecksum(dir string) (string, error) {
	pluginYAML, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("plugin.yaml\x00"))
	h.Write(pluginYAML)

	exclude := map[string]bool{
		".installed_at":   true,
		".status":         true,
		".content.sha256": true,
		"manifest.json":   true,
	}
	var paths []string
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if exclude[rel] || exclude[filepath.Base(rel)] {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(paths)
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		h.Write([]byte("\x00"))
		h.Write([]byte(rel))
		h.Write([]byte("\x00"))
		h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// IsContentCurrent 检查 <dir>/.content.sha256 与当前 ContentChecksum 是否一致。
// 返回 (current bool, expected string, actual string, err error)。
func IsContentCurrent(dir string) (bool, string, string, error) {
	markerPath := filepath.Join(dir, ".content.sha256")
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		return false, "", "", err
	}
	expected := strings.TrimSpace(string(markerBytes))
	actual, err := ContentChecksum(dir)
	if err != nil {
		return false, expected, "", err
	}
	return expected == actual, expected, actual, nil
}

// writeContentMarker 写 <dir>/.content.sha256。
func writeContentMarker(dir string, checksum string) error {
	return os.WriteFile(filepath.Join(dir, ".content.sha256"), []byte(checksum), 0o644)
}
