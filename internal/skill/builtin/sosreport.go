package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&Sosreport{}) }

// Sosreport 收集系统诊断信息 (sos report --batch).
// 容器内可能不可用, 返回明确错误. 大文件 (100MB+).
type Sosreport struct{}

func (Sosreport) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_sosreport",
		Name:        "系统取证打包",
		Description: "sos report --batch 收集系统诊断信息 (硬件 / 内核 / 网络 / 进程 / 配置). 输出 100MB+ tarball, 自动 spill 到 /var/tmp/. 容器内不可用. NOT for: 频繁调用 (重操作) / 单项数据 (各专项 skill).",
		Class:       skill.ClassSafe,
		Category:    "system",
		Params: skill.ParamSchema{
			{Name: "output_dir", Param: skill.Param{Type: "string", Default: "/var/tmp", Desc: "报告输出目录"}},
			{Name: "timeout_sec", Param: skill.Param{Type: "int", Default: 300, Desc: "总超时秒数, 默认 300 (5min)"}},
		},
		ResultPreview: "{report_path, size_bytes, duration_s, plugins: [...], error?}",
	}
}

type sosreportParams struct {
	OutputDir  string `json:"output_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

type sosreportResult struct {
	ReportPath string   `json:"report_path,omitempty"`
	SizeBytes  int64    `json:"size_bytes,omitempty"`
	DurationS  int      `json:"duration_s"`
	Plugins    []string `json:"plugins,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func (Sosreport) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p sosreportParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("sosreport: decode params: %w", err)
		}
	}
	if p.OutputDir == "" {
		p.OutputDir = "/var/tmp"
	}
	if p.TimeoutSec <= 0 {
		p.TimeoutSec = 300
	}
	if p.TimeoutSec > 900 {
		p.TimeoutSec = 900 // 硬上限 15min
	}
	if err := os.MkdirAll(p.OutputDir, 0o755); err != nil {
		return json.Marshal(sosreportResult{Error: fmt.Sprintf("mkdir %s: %v", p.OutputDir, err)})
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sos", "report", "--batch", "--tmp-dir", p.OutputDir)
	out, err := cmd.CombinedOutput()
	res := sosreportResult{DurationS: int(time.Since(start).Seconds())}
	if err != nil {
		res.Error = strings.TrimSpace(string(out))
		return json.Marshal(res)
	}
	// sos report 完成后 stdout 含报告路径
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, ".tar.xz") || strings.HasSuffix(line, ".tar.gz") {
			if info, statErr := os.Stat(line); statErr == nil {
				res.ReportPath = line
				res.SizeBytes = info.Size()
				res.Plugins = inferSosPlugins(filepath.Base(line))
				return json.Marshal(res)
			}
		}
	}
	// 没找到 tarball, 扫目录找最新
	res.ReportPath, res.SizeBytes, res.Plugins = findLatestSos(p.OutputDir)
	return json.Marshal(res)
}

// inferSosPlugins 从文件名推断 plugin (sosreport-xxxx-hash.tar.xz 形式)
func inferSosPlugins(name string) []string {
	// 真实 plugin 列表需 sos report --list-plugins; 简化返回 filename.
	return []string{name}
}

func findLatestSos(dir string) (string, int64, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, nil
	}
	var best os.DirEntry
	var bestMod time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "sosreport-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if best == nil || info.ModTime().After(bestMod) {
			best = e
			bestMod = info.ModTime()
		}
	}
	if best == nil {
		return "", 0, nil
	}
	path := filepath.Join(dir, best.Name())
	info, _ := os.Stat(path)
	var size int64
	if info != nil {
		size = info.Size()
	}
	return path, size, []string{best.Name()}
}
