package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 这一层管的是文档里的**命令行**：一个 flag 名写错了，命令会立刻以
// "flag provided but not defined" 退出，所以它比一个写错的端点更容易被读者
// 发现——但前提是读者真的照抄了那一条命令。
//
// 决策 293 之前没有东西看它。docs/integration-guide.md §4 的四条示例命令没有
// 一条能照抄运行（--tenant-mapping 的冒号形式、export 的 --rate、
// verify --source <URL>、以及一个不存在的二进制名），而 docs/harness-guide.md 与
// docs/operations-manual.md 里另有七个 opskeeper-eval 的 flag 从未定义过。
// apidoc 原本只读 docs/api/ 下的端点声明，命令行示例在它看不见的地方。
//
// 两条刻意的收窄，都是为了让这道闸门不可能被误报喂饱：
//
//  1. **只校验"子命令已知"的那一条命令的 flag。** 文档里写着
//     `opskeeper helm upgrade -f x.yaml` 这种把外部工具夹在中间的写法很多，
//     把它读成"opskeeper 的子命令叫 helm"会产出一批假阳性。子命令认不出来
//     就整条跳过，报告里单列一列“未识别的子命令”，不参与成败。
//  2. **二进制名必须是一个独立的词。** `opskeeper-llm-credentials` 不是
//     `opskeeper`，前者是 kubectl secret 的名字；不要求边界的话，
//     每一个 secret 名都会被读成一次 opskeeper 调用。

// flagVarRE 读出一个 FlagSet 上真实定义过的 flag 名（Var 形式）。
var flagVarRE = regexp.MustCompile(`\.\w*Var(?:f|s)?\(\s*&[^,]+,\s*"([^"]+)"`)

// flagSetRE 读出这一行里新建的 FlagSet 变量名。
//
// 只有在 flag.NewFlagSet 返回值的接收者上认 flag 名，是因为"任何方法调用的第一个
// 字符串参数"是一个远宽的形状：errors.New("boom")、fmt.Errorf("x")、time.Parse
// 都会被读成一个叫 boom 的 flag，一个文档写 --boom 的地方会因此通过。这道闸门
// 的全部价值在于它抓得到幻影，宁可漏也不能放行。
var flagSetRE = regexp.MustCompile(`(\w+)\s*:?=\s*(?:flag|fs|pflag|spf13flag)\.NewFlagSet`)

// caseRE2 读出一个 switch 里 dispatch 的子命令。
var caseRE2 = regexp.MustCompile(`(?m)^\s*case\s+"([a-z][a-z0-9-]*)"\s*:`)

// dispatcherRE 认出一个二进制**真的**按子命令分发。
//
// 这是必须的收窄，不是锦上添花。`case "..."` 在 Go 里同时是子命令分发和
// 类型 switch 的分支：cmd/opskeeper/main.go 一个文件里就有三十多个
// `case "AgentTool"`、`case "array"`、`case "logs"` —— 那是 eino 的工具名和
// JSON 的 kind，不是命令行子命令。把它们当成词表，文档里每一个
// `opskeeper helm upgrade` 都会变成一条"子命令叫 helm"的假阳性。
//
// 真正的分发器有一处假货识别不了的地方：它的 default 分支会打印
// "unknown subcommand" 并退出。没有这个 default 的 switch 不是一个 CLI 的
// 命令表，只是一个 switch。
var dispatcherRE = regexp.MustCompile(`(?i)unknown subcommand|未知子命令`)

// cliCommand 是这个仓库里一个二进制真实支持的东西。
type cliCommand struct {
	// Flags 是它定义过的全部 flag 名。
	Flags map[string]bool
	// Subcommands 是它 dispatch 的子命令；仅当 dispatcherRE 命中时才有意义。
	Subcommands map[string]bool
	// Dispatcher 为真时 Subcommands 才是这个二进制的完整命令表。
	Dispatcher bool
}

// repoCLIs 读出 cmd/ 下每个二进制的 flag 与子命令。
func repoCLIs(root string) (map[string]*cliCommand, error) {
	out := map[string]*cliCommand{}
	cmdDir := filepath.Join(root, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cmd := &cliCommand{Flags: map[string]bool{}, Subcommands: map[string]bool{}}
		found := false
		err := filepath.WalkDir(filepath.Join(cmdDir, entry.Name()),
			func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				raw, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				text := string(raw)
				if strings.HasSuffix(path, "main.go") {
					found = true
					// 子命令表只在 main.go 里：分发器是 main 的职责，
					// 子命令自己的实现文件里的 case 是业务分支。
					if dispatcherRE.MatchString(text) {
						cmd.Dispatcher = true
					}
					for _, m := range caseRE2.FindAllStringSubmatch(text, -1) {
						cmd.Subcommands[m[1]] = true
					}
				}
				for _, m := range flagVarRE.FindAllStringSubmatch(text, -1) {
					if m[1] != "" {
						cmd.Flags[m[1]] = true
					}
				}
				// 非 Var 形式按文件逐个 FlagSet 变量解析。
				for _, set := range flagSetRE.FindAllStringSubmatch(text, -1) {
					plain := regexp.MustCompile(`\b` + regexp.QuoteMeta(set[1]) + `\.\w+\(\s*"([a-z][a-z0-9-]*)"`)
					for _, m := range plain.FindAllStringSubmatch(text, -1) {
						cmd.Flags[m[1]] = true
					}
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
		if found {
			out[entry.Name()] = cmd
		}
	}
	return out, nil
}

// cliInvocationRE matches the shape of a command line: a word, then a
// subcommand, then flags.
//
// The binary name is captured as [\w-]+ rather than spelled out as an
// alternation of the real names, and the real name is compared in Go code
// below. Two reasons, and the first is the one that cost a rewrite:
//
//  1. Go's regexp is RE2, which has no lookahead. The boundary has to be
//     enforced by what the pattern *captures*, not by a (?!...) after it.
//  2. Capturing the whole word is what makes `opskeeper-llm-credentials` work.
//     That string is a kubectl secret name, not a command, and a pattern that
//     matched `opskeeper` inside it would read every secret in the docs as an
//     invocation of a binary that does not exist.
func cliInvocationRE() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[\s|;&(./` + "`" + `])([\w-]+)\s+([a-z][a-z0-9-]*)([^\n]*)`)
}

// flagRE 从一条命令的尾巴里读出每个 --flag。
var flagRE = regexp.MustCompile(`--([a-z0-9][a-z0-9-]*)`)

// joinContinuations closes a shell line continuation and keeps the offset map
// back to the original text.
//
// A command written across lines ("cmd \\\n  --flag x") is one command, and
// checking only the first half of it would read a real flag as absent. Closing
// the break is easy; the part that is easy to get wrong is the line number a
// finding is reported at, which is why the mapping is built here rather than
// re-derived at each call site with a substring search — that search finds the
// first "run" in the block, not the one on the line being reported.
func joinContinuations(text string) (string, []int) {
	var out strings.Builder
	origin := make([]int, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && text[i+1] == '\n' {
			out.WriteByte(' ')
			origin = append(origin, i)
			i++
			continue
		}
		out.WriteByte(text[i])
		origin = append(origin, i)
	}
	return out.String(), origin
}

// checkCLIFlags 是这条命令的第二问：文档里每条命令的每个 flag，
// 那个二进制真的定义过吗。
func checkCLIFlags(root string, docs []string, readFile func(string) ([]byte, error)) ([]Missing, int, map[string]int, error) {
	cmds, err := repoCLIs(root)
	if err != nil {
		return nil, 0, nil, err
	}
	invocation := cliInvocationRE()

	var missing []Missing
	checked := 0
	unknownSubs := map[string]int{}

	for _, doc := range docs {
		body, rerr := readFile(doc)
		if rerr != nil {
			return nil, 0, nil, rerr
		}
		rel, _ := filepath.Rel(root, doc)
		for _, block := range fencedBlocks(string(body), string(body)) {
			joined, origin := joinContinuations(block.body)
			for _, m := range invocation.FindAllStringSubmatchIndex(joined, -1) {
				bin := joined[m[2]:m[3]]
				sub := joined[m[4]:m[5]]
				cmd := cmds[bin]
				if cmd == nil {
					// A word shaped like a command that is not one of this
					// repository's binaries: an external tool, or a name that
					// merely starts with one. Nothing to check it against.
					continue
				}
				if !cmd.Subcommands[sub] {
					if !cmd.Dispatcher {
						// 没有可判定的词表。文档里 `opskeeper helm upgrade` 这类
						// 把外部工具夹在中间的写法很多，在没有词表的情况下把它
						// 读成"opskeeper 的子命令叫 helm"会产出一批假阳性。
						// 记下来让人看，但不参与成败。
						unknownSubs[bin+" "+sub]++
						continue
					}
					// 有词表：这条命令会打印 unknown subcommand 然后退出 2。
					missing = append(missing, Missing{
						Doc:  rel,
						Line: lineOf(string(body), block.offset+origin[m[4]]),
						Claim: fmt.Sprintf("subcommand %s %s (the binary dispatches %d others and exits 2 on this one)",
							bin, sub, len(cmd.Subcommands)),
					})
					continue
				}
				for _, fm := range flagRE.FindAllStringSubmatchIndex(joined[m[6]:m[7]], -1) {
					checked++
					flag := joined[m[6]+fm[2] : m[6]+fm[3]]
					if cmd.Flags[flag] {
						continue
					}
					missing = append(missing, Missing{
						Doc:   rel,
						Line:  lineOf(string(body), block.offset+origin[m[6]+fm[0]]),
						Claim: fmt.Sprintf("%s %s --%s (that flag is not defined)", bin, sub, flag),
					})
				}
			}
		}
	}
	return missing, checked, unknownSubs, nil
}
