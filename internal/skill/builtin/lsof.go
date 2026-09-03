package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&Lsof{}) }

// Lsof 列出进程打开的文件 / 路径上的句柄 (lsof -p <pid> | lsof <path>).
type Lsof struct{}

func (Lsof) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_lsof",
		Name:        "打开文件/句柄查询",
		Description: "列出进程打开的文件 / 路径上的句柄 (lsof -p <pid> 或 lsof <path>). 诊断文件锁 / 句柄泄漏 / 删除但未释放的文件. NOT for: 进程列表 (probe_process).",
		Class:       skill.ClassSafe,
		Category:    "process",
		Params: skill.ParamSchema{
			{Name: "pid", Param: skill.Param{Type: "int", Desc: "目标进程 PID (与 path 二选一)"}},
			{Name: "path", Param: skill.Param{Type: "string", Desc: "目标路径 (与 pid 二选一)"}},
		},
		ResultPreview: "{entries: [{command, pid, user, fd, type, name}], total, error?}",
	}
}

type lsofParams struct {
	PID  int    `json:"pid"`
	Path string `json:"path"`
}

type lsofEntry struct {
	Command string `json:"command"`
	PID     int    `json:"pid"`
	User    string `json:"user"`
	FD      string `json:"fd"`
	Type    string `json:"type"`
	Name    string `json:"name"`
}

type lsofResult struct {
	Entries []lsofEntry `json:"entries"`
	Total   int         `json:"total"`
	Error   string      `json:"error,omitempty"`
}

func (Lsof) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p lsofParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("lsof: decode params: %w", err)
		}
	}
	if p.PID <= 0 && p.Path == "" {
		return nil, fmt.Errorf("lsof: either pid or path required")
	}

	var args []string
	if p.PID > 0 {
		args = []string{"-p", strconv.Itoa(p.PID), "-F", "0"}
	} else {
		args = []string{p.Path, "-F", "0"}
	}
	cmd := exec.CommandContext(ctx, "lsof", args...)
	out, err := cmd.Output()
	// lsof exit 1 = no match, 不算 error
	res := lsofResult{}
	if err != nil && !strings.Contains(err.Error(), "exit status 1") {
		res.Error = err.Error()
	}
	parseLsof(string(out), &res)
	res.Total = len(res.Entries)
	return json.Marshal(res)
}

// parseLsof 解析 lsof -F 0 (machine-readable) 输出.
func parseLsof(out string, res *lsofResult) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	var cur lsofEntry
	flush := func() {
		if cur.PID != 0 || cur.Name != "" {
			res.Entries = append(res.Entries, cur)
		}
		cur = lsofEntry{}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 1 {
			continue
		}
		tag := line[0]
		val := line[1:]
		switch tag {
		case 'p':
			flush()
			pid, _ := strconv.Atoi(val)
			cur.PID = pid
		case 'c':
			cur.Command = val
		case 'u':
			cur.User = val
		case 'f':
			cur.FD = val
		case 't':
			cur.Type = val
		case 'n':
			cur.Name = val
		case '0':
			flush()
		}
	}
	flush()
}
