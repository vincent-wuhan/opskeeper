package migrate

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SnapshotVersion 当前快照格式版本。
const SnapshotVersion = "v1"

// SnapshotHeader 描述一次导出的元信息。
type SnapshotHeader struct {
	Version       string      `json:"version"`                  // 快照格式版本
	ExportedAt    time.Time   `json:"exported_at"`              // 导出时间（UTC）
	Source        string      `json:"source"`                   // ops-keeper URL
	OpsKeeperVer  string      `json:"opskeeper_ver"`            // ops-keeper 版本（可选）
	TenantMapping []TenantMap `json:"tenant_mapping,omitempty"` // 显式 tenant 映射
}

// TenantMap 显式 ops-keeper project → opskeeper tenant 映射。
type TenantMap struct {
	OpsKeeperProjectID int64 `json:"opskeeper_project_id"`
	OpskeeperTenantID  int64 `json:"opskeeper_tenant_id"`
}

// Snapshot 一次导出的完整内容（header + 9 类实体数据）。
type Snapshot struct {
	Header   SnapshotHeader                  `json:"header"`
	Entities map[EntityType][]map[string]any `json:"entities"`
}

// SnapshotMeta 描述一个已存在的快照文件（用于回滚等场景）。
type SnapshotMeta struct {
	Path       string
	Header     SnapshotHeader
	Size       int64
	IsRollback bool
}

// NewSnapshot 创建空 snapshot。
func NewSnapshot(source, opsKeeperVer string, tenantMapping []TenantMap) *Snapshot {
	return &Snapshot{
		Header: SnapshotHeader{
			Version:       SnapshotVersion,
			ExportedAt:    time.Now().UTC(),
			Source:        source,
			OpsKeeperVer:  opsKeeperVer,
			TenantMapping: tenantMapping,
		},
		Entities: make(map[EntityType][]map[string]any),
	}
}

// PutEntity 写入一类实体的全部记录。
func (s *Snapshot) PutEntity(t EntityType, rows []map[string]any) {
	s.Entities[t] = rows
}

// GetEntity 读出一类实体的全部记录。
func (s *Snapshot) GetEntity(t EntityType) []map[string]any {
	return s.Entities[t]
}

// TotalRows 全部记录总数。
func (s *Snapshot) TotalRows() int {
	n := 0
	for _, rows := range s.Entities {
		n += len(rows)
	}
	return n
}

// WriteTo 把 snapshot 写到目标 writer（支持 .gz 自动压缩）。
func (s *Snapshot) WriteTo(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 snapshot 失败: %w", err)
	}

	// 自动 .gz 压缩（若路径以 .gz 结尾）
	var w io.Writer
	if filepath.Ext(path) == ".gz" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		gz := gzip.NewWriter(f)
		defer gz.Close()
		bw := bufio.NewWriter(gz)
		defer bw.Flush()
		w = bw
	} else {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		bw := bufio.NewWriter(f)
		defer bw.Flush()
		w = bw
	}

	if _, err := w.Write(raw); err != nil {
		return err
	}
	return nil
}

// ReadSnapshot 从文件读取 snapshot（自动识别 .gz）。
func ReadSnapshot(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 snapshot 失败: %w", err)
	}
	defer f.Close()

	var r io.Reader = f
	if filepath.Ext(path) == ".gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("解压 snapshot 失败: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	dec := json.NewDecoder(r)
	snap := &Snapshot{}
	if err := dec.Decode(snap); err != nil {
		return nil, fmt.Errorf("解析 snapshot 失败: %w", err)
	}
	if snap.Header.Version != SnapshotVersion {
		return nil, fmt.Errorf("snapshot 版本不兼容: got=%s want=%s", snap.Header.Version, SnapshotVersion)
	}
	return snap, nil
}

// InspectSnapshot 读取 snapshot 的 header 不解析全量数据（用于 list 命令）。
//
// 实现：用 json.RawMessage 跳过 entities 字段。
func InspectSnapshot(path string) (*SnapshotMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	var r io.Reader = f
	if filepath.Ext(path) == ".gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}

	// 解析为 envelope（header + 剩余）
	var envelope struct {
		Header   SnapshotHeader  `json:"header"`
		Entities json.RawMessage `json:"entities"`
	}
	if err := json.NewDecoder(r).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("解析 snapshot header 失败: %w", err)
	}

	return &SnapshotMeta{
		Path:   path,
		Header: envelope.Header,
		Size:   stat.Size(),
	}, nil
}

// FindRollbackSnapshot 在同目录查找匹配导出时间的 rollback 快照。
// 命名约定：rollback-snapshot-{YYYY-MM-DDTHH-MM-SS}.json
func FindRollbackSnapshot(dir string, after time.Time) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !stringsHasPrefix(name, "rollback-snapshot-") {
			continue
		}
		if !after.IsZero() {
			info, ierr := e.Info()
			if ierr == nil && info.ModTime().Before(after) {
				continue
			}
		}
		return filepath.Join(dir, name), nil
	}
	return "", fmt.Errorf("未找到 rollback snapshot（目录: %s）", dir)
}

// stringsHasPrefix 是 strings.HasPrefix 的本地封装（避免 import cycle）。
func stringsHasPrefix(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	return s[:len(prefix)] == prefix
}
