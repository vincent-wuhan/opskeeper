package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&Strace{}) }

// Strace 跟踪进程的系统调用摘要 (strace -c -p <pid>).
// 注意: 需要 CAP_SYS_PTRACE; 容器内可能受限.
type Strace struct{}

func (Strace) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_strace",
		Name:        "进程系统调用追踪",
		Description: "跟踪指定 PID 的系统调用摘要 (strace -c -p <pid>). 报告 syscall / calls / errors / time. 需要 CAP_SYS_PTRACE. NOT for: 长时间跟踪 (>duration_sec) / 内核追踪 (bpftrace).",
		Class:       skill.ClassSafe,
		Category:    "process",
		Params: skill.ParamSchema{
			{Name: "pid", Param: skill.Param{Type: "int", Required: true, Desc: "目标进程 PID"}},
			{Name: "duration_sec", Param: skill.Param{Type: "int", Default: 5, Desc: "跟踪时长秒数, 默认 5"}},
			{Name: "events", Param: skill.Param{Type: "string", Desc: "事件过滤, 如 'network,file,desc' (默认 all)"}},
		},
		ResultPreview: "{pid, syscalls: [{name, calls, errors, time_s}], duration_s, error?}",
	}
}

type straceParams struct {
	PID         int    `json:"pid"`
	DurationSec int    `json:"duration_sec"`
	Events      string `json:"events"`
}

type straceSyscall struct {
	Name   string  `json:"name"`
	Calls  int     `json:"calls"`
	Errors int     `json:"errors"`
	TimeS  float64 `json:"time_s"`
}

type straceResult struct {
	PID       int             `json:"pid"`
	Syscalls  []straceSyscall `json:"syscalls"`
	DurationS int             `json:"duration_s"`
	Error     string          `json:"error,omitempty"`
}

func (Strace) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p straceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("strace: decode params: %w", err)
		}
	}
	if p.PID <= 0 {
		return nil, fmt.Errorf("strace: pid required (positive)")
	}
	if p.DurationSec <= 0 {
		p.DurationSec = 5
	}
	if p.DurationSec > 60 {
		p.DurationSec = 60 // 硬上限
	}

	// 检查 PID 是否存在
	if _, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/%d", p.PID)); err != nil {
		return json.Marshal(straceResult{PID: p.PID, Error: fmt.Sprintf("pid %d not found", p.PID)})
	}

	args := []string{"-c", "-p", strconv.Itoa(p.PID), "-f", "-tt"}
	if p.Events != "" {
		args = append(args, "-e", "trace="+p.Events)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.DurationSec+2)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "strace", args...)
	out, err := cmd.CombinedOutput()
	res := straceResult{PID: p.PID, DurationS: p.DurationSec}
	if err != nil {
		// ptrace 失败常见: "Operation not permitted" / "Permission denied"
		res.Error = err.Error() + ": " + strings.TrimSpace(string(out))[:min(200, len(out))]
		return json.Marshal(res)
	}
	parseStraceSummary(string(out), &res)
	return json.Marshal(res)
}

// parseStraceSummary 解析 strace -c 输出:
// % time     seconds  usecs/call     calls    errors syscall
//
//	50.00    0.001000          10        100        10 read
func parseStraceSummary(out string, res *straceResult) {
	lines := splitLines(out)
	inTable := false
	for _, line := range lines {
		if strings.Contains(line, "usecs/call") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		// 最后一列是 syscall name; 倒数第二列是 errors
		name := fields[len(fields)-1]
		// strace -c 列顺序: %time | seconds | usecs/call | calls | errors | syscall
		// fields[len-2] = errors (倒数第二), fields[len-3] = calls
		errors, _ := strconv.Atoi(fields[len(fields)-2])
		calls, _ := strconv.Atoi(fields[len(fields)-3])
		time, _ := strconv.ParseFloat(fields[1], 64)
		res.Syscalls = append(res.Syscalls, straceSyscall{
			Name: name, Calls: calls, Errors: errors, TimeS: time,
		})
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
