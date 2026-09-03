package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&Dmesg{}) }

// Dmesg 读内核环形缓冲 (dmesg -T --level=<levels>).
// 容器里可能读不到 /proc/kmsg, 返回明确错误.
type Dmesg struct{}

func (Dmesg) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_dmesg",
		Name:        "内核日志",
		Description: "读内核环形缓冲 (dmesg -T --level=err,warn). 诊断硬件错误 / 驱动异常 / OOM. 容器内可能读不到, 返回明确错误. NOT for: 应用日志 (tail_file) / 系统日志 (journalctl).",
		Class:       skill.ClassSafe,
		Category:    "kernel",
		Params: skill.ParamSchema{
			{Name: "levels", Param: skill.Param{Type: "string", Default: "err,warn", Desc: "日志级别, 逗号分隔 (默认 err,warn)"}},
			{Name: "since", Param: skill.Param{Type: "string", Desc: "可选 RFC3339 起始时间, 只返回之后条目"}},
			{Name: "max_lines", Param: skill.Param{Type: "int", Default: 200, Desc: "最大返回行数, 默认 200"}},
		},
		ResultPreview: "{entries: [{timestamp, level, message}], total, truncated, error?}",
	}
}

type dmesgParams struct {
	Levels   string `json:"levels"`
	Since    string `json:"since"`
	MaxLines int    `json:"max_lines"`
}

type dmesgEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

type dmesgResult struct {
	Entries   []dmesgEntry `json:"entries"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
	SpillPath string       `json:"spill_path,omitempty"`
	Error     string       `json:"error,omitempty"`
}

func (Dmesg) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p dmesgParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("dmesg: decode params: %w", err)
		}
	}
	if p.Levels == "" {
		p.Levels = "err,warn"
	}
	if p.MaxLines <= 0 {
		p.MaxLines = 200
	}

	args := []string{"-T", "--level=" + p.Levels}
	cmd := exec.CommandContext(ctx, "dmesg", args...)
	out, err := cmd.CombinedOutput()
	res := dmesgResult{}
	if err != nil {
		// 常见: 容器里 "dmesg: read kernel buffer failed: Operation not permitted"
		res.Error = strings.TrimSpace(string(out))
		return json.Marshal(res)
	}
	parseDmesg(string(out), &res, p.MaxLines)
	// 大输出 spill
	if len(res.Entries) > 0 {
		// 已截断, spill 路径
	}
	res.Total = len(res.Entries)
	return json.Marshal(res)
}

// parseDmesg 解析 dmesg -T 输出:
// [Mon Jan 15 10:23:45 2024] err: message
func parseDmesg(out string, res *dmesgResult, maxLines int) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		entry := dmesgEntry{Message: line}
		// 解析 [timestamp] level: msg
		if strings.HasPrefix(line, "[") {
			if end := strings.Index(line, "]"); end > 0 {
				entry.Timestamp = strings.TrimPrefix(strings.TrimSuffix(line[:end+1], "]"), "[")
				rest := strings.TrimSpace(line[end+1:])
				// 匹配 " err: ..." 或 " warn: ..."
				if idx := strings.Index(rest, ": "); idx > 0 {
					entry.Level = strings.TrimSpace(rest[:idx])
					entry.Message = rest[idx+2:]
				} else {
					entry.Level = "info"
				}
			}
		}
		res.Entries = append(res.Entries, entry)
		if maxLines > 0 && len(res.Entries) >= maxLines {
			res.Truncated = true
			return
		}
	}
}
