package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/skill"
)

func init() { skill.Register(&GrepFile{}) }

// GrepFile 在文件中按正则搜索 (类似 `grep -nE`).
type GrepFile struct{}

func (GrepFile) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_grep_file",
		Name:        "文件正则搜索",
		Description: "在文件 / 目录树里按正则搜索 (grep -nE). 返回匹配的 line_num + line + 命中数. NOT for: 大文件全量读取 (tail_file) / 二进制 (strings).",
		Class:       skill.ClassSafe,
		Category:    "filesystem",
		Params: skill.ParamSchema{
			{Name: "path", Param: skill.Param{Type: "string", Required: true, Desc: "文件 / 目录绝对路径"}},
			{Name: "pattern", Param: skill.Param{Type: "string", Required: true, Desc: "正则表达式 (POSIX ERE)"}},
			{Name: "ignore_case", Param: skill.Param{Type: "bool", Default: false, Desc: "是否忽略大小写"}},
			{Name: "max_matches", Param: skill.Param{Type: "int", Default: 100, Desc: "最大返回命中数, 默认 100"}},
		},
		ResultPreview: "{path, matches: [{line_num, line}], total_matches, truncated, error?}",
	}
}

type grepFileParams struct {
	Path       string `json:"path"`
	Pattern    string `json:"pattern"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxMatches int    `json:"max_matches"`
}

type grepFileMatch struct {
	LineNum int    `json:"line_num"`
	Line    string `json:"line"`
}

type grepFileResult struct {
	Path         string          `json:"path"`
	Matches      []grepFileMatch `json:"matches"`
	TotalMatches int             `json:"total_matches"`
	Truncated    bool            `json:"truncated"`
	Error        string          `json:"error,omitempty"`
}

func (GrepFile) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p grepFileParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("grep_file: decode params: %w", err)
		}
	}
	if p.Path == "" || p.Pattern == "" {
		return nil, fmt.Errorf("grep_file: path and pattern required")
	}
	if !filepath.IsAbs(p.Path) {
		return nil, fmt.Errorf("grep_file: path must be absolute")
	}
	if strings.Contains(p.Path, "..") {
		return nil, fmt.Errorf("grep_file: path must not contain ..")
	}
	if p.MaxMatches <= 0 {
		p.MaxMatches = 100
	}

	args := []string{"-nE", "-m", fmt.Sprintf("%d", p.MaxMatches+1)}
	if p.IgnoreCase {
		args = append(args, "-i")
	}
	// -- 让 pattern 不被当成文件名
	args = append(args, "--", p.Pattern, p.Path)
	cmd := exec.CommandContext(ctx, "grep", args...)
	out, err := cmd.Output()
	// grep exit 1 = no match, 不算 error
	res := grepFileResult{Path: p.Path}
	if err != nil && err.Error() != "exit status 1" {
		res.Error = err.Error()
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		ln, _ := parseInt(line[:idx])
		if ln == 0 {
			continue
		}
		res.Matches = append(res.Matches, grepFileMatch{LineNum: ln, Line: line[idx+1:]})
		if len(res.Matches) > p.MaxMatches {
			res.Truncated = true
			res.Matches = res.Matches[:p.MaxMatches]
		}
	}
	res.TotalMatches = len(res.Matches)
	return json.Marshal(res)
}

func parseInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not int")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
