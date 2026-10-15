// Command acceptance runs an acceptance chain: everything that has to be
// true before a claim about delivery can be said out loud.
//
// Two chains ship:
//
//	stage0     one edge, on a node, talks to a model
//	federation two control planes, one policy delivery
//
// They are separate because their evidence is separate. The stage-0 chain
// is about artefacts and one machine; the federation chain is about two
// processes that usually live on two hosts, and its inputs are therefore
// read from two env files rather than from the process environment.
//
// Why this exists. The delivery pieces were all built and separately
// asserted — the bundle stages pig, the image copies it, the installer
// self-checks its version, the node reads credentials through $VAR — but
// nobody had put them in one place, so the sentence "the node can hold a
// real conversation" had no command behind it. A claim with no command is a
// claim somebody has to believe.
//
// The shape is chosen so that a missing input is not a failure:
//
//	offline checks run anywhere and must pass. A failure here is a real
//	FAIL — something in this repository is wrong.
//
//	needs-input checks are skipped with a named reason when their input is
//	absent, and the command exits 3 rather than 0. That distinct exit code
//	is the whole point: CI has to be able to tell "the chain is broken" from
//	"the chain could not be run here", and a script that returns 1 for both
//	gets either switched off or ignored.
//
// Usage:
//
//	go run ./scripts/acceptance [repo-root]
//
// Exit 0 — every offline check passed and every needs-input check was met.
// Exit 3 — at least one input was missing; nothing failed.
// Exit 1 — at least one offline check failed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type status int

const (
	statusPass status = iota
	statusMissing
	statusFail
)

func (s status) String() string {
	switch s {
	case statusPass:
		return "pass"
	case statusMissing:
		return "MISSING"
	default:
		return "FAIL"
	}
}

type check struct {
	id      string
	subject string
	// needs names the input that has to exist for this check to run. Empty
	// means the check is offline and must pass.
	needs string
	// how tells a reader where to supply a missing input, which is the
	// difference between a skip somebody can act on and a skip that reads
	// as "not applicable".
	how string
	run func(root string) (bool, string)
}

var stage0Checks = []check{
	{
		id:      "A1",
		subject: "edge bundle 的分发表里有 pig，且标记为 required",
		run: func(root string) (bool, string) {
			body, err := read(root, "deploy/install/edge/build-edge-bundle.sh")
			if err != nil {
				return false, err.Error()
			}
			// The table is the bundle's own contract: a line naming pig, the
			// install path it lands at, and "required" so a missing binary
			// fails the build rather than shipping an edge with no agent.
			for _, line := range strings.Split(body, "\n") {
				if !strings.Contains(line, "pig") || strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				// The table is one quoted string per row, so the fields
				// arrive with the quotes attached: `"pig 0755 /usr/local/...
				// required"`. Splitting on whitespace and comparing raw
				// would fail on the quotes and report a broken bundle that
				// is fine.
				fields := strings.Fields(line)
				if len(fields) < 2 {
					continue
				}
				// Split first, then strip the quotes: the row opens with two
				// spaces, so trimming quotes off the whole line first leaves
				// the opening one attached to the first field and every
				// comparison below misses.
				head := strings.Trim(fields[0], "\"")
				tail := strings.Trim(fields[len(fields)-1], "\"")
				if head == "pig" && tail == "required" {
					return true, ""
				}
			}
			return false, "build-edge-bundle.sh 的分发表里没有一行把 pig 标成 required；节点会装出一个没有 agent 的 edge"
		},
	},
	{
		id:      "A2",
		subject: "edge 镜像把 pig 拷进去，并把 agent 路径指到它",
		run: func(root string) (bool, string) {
			body, err := read(root, "deploy/Dockerfile.opskeeper-edge")
			if err != nil {
				return false, err.Error()
			}
			if !strings.Contains(body, "COPY --from=builder /out/pig") {
				return false, "镜像没有把构建出的 pig 拷进去"
			}
			if !strings.Contains(body, "OPSKEEPER_EDGE_AGENT_BIN") {
				return false, "镜像没有用 OPSKEEPER_EDGE_AGENT_BIN 指到 pig，节点会找不到 agent"
			}
			return true, ""
		},
	},
	{
		id:      "A3",
		subject: "安装脚本真的执行一次已装的 agent，而不是只写下那句话",
		run: func(root string) (bool, string) {
			body, err := read(root, "deploy/install/edge/install-edge.sh")
			if err != nil {
				return false, err.Error()
			}
			// The first version of this check looked for the string
			// "pig --version", which the file contains — in a COMMENT that
			// explains why the self-check matters. The real check runs the
			// installed binary through a variable. **A gate that greps a
			// comment is a gate that reads the author's intent**, so this
			// one only looks at lines that are not comments, and requires
			// the failure to be a hard exit rather than a warning.
			lines := strings.Split(body, "\n")
			ranAt := -1
			for i, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if !strings.Contains(line, "--version") {
					continue
				}
				if strings.Contains(line, "AGENT_BIN") || strings.Contains(line, "$AGENT") {
					ranAt = i
					break
				}
			}
			// The refusal is a separate line from the test in both the real
			// script and any sane copy of it, so the window after the
			// invocation is what has to contain the exit — looking for both
			// words on ONE line reported the healthy script as broken.
			hardFail := false
			if ranAt >= 0 {
				for i := ranAt; i < len(lines) && i <= ranAt+6; i++ {
					if strings.Contains(lines[i], "exit 1") {
						hardFail = true
						break
					}
				}
			}
			if ranAt < 0 {
				return false, "install-edge.sh 没有在注释之外执行过已安装的 agent；装不起来的 agent 会静默通过安装"
			}
			if !hardFail {
				return false, "安装脚本的失败路径不是硬失败，一个跑不起来的 agent 只会是一次警告"
			}
			return true, ""
		},
	},
	{
		id:      "A4",
		subject: "节点凭据走 $VAR 间接引用，不写进插件包",
		run: func(root string) (bool, string) {
			body, err := read(root, "core/floor/delivery/edge_agent_assets.go")
			if err == nil {
				if !strings.Contains(body, "$") {
					return false, "edge agent 资产里没有任何间接引用"
				}
			}
			body2, err2 := read(root, "deploy/install/edge/opskeeper-edge.env.example")
			if err2 != nil {
				return false, err2.Error()
			}
			if strings.Contains(body2, "sk-") {
				return false, "env 模板里出现了一个像真密钥的字面量"
			}
			return true, ""
		},
	},
	{
		id:      "A5",
		subject: "节点读不到云厂商凭据这道闸门在册",
		run: func(root string) (bool, string) {
			body, err := read(root, "Makefile")
			if err != nil {
				return false, err.Error()
			}
			if !strings.Contains(body, "edge-credential-check:") {
				return false, "Makefile 里没有 edge-credential-check；这一格就没人在管"
			}
			return true, ""
		},
	},
	{
		id:      "A6",
		subject: "本地构建出的 pig 二进制可执行",
		needs:   "先跑 make build-pig-all（或指定 OPSKEEPER_PIG_BIN 指向一个已构建的二进制）",
		how:     "OPSKEEPER_PIG_BIN=/path/to/pig go run ./scripts/acceptance .",
		run: func(root string) (bool, string) {
			bin := strings.TrimSpace(os.Getenv("OPSKEEPER_PIG_BIN"))
			if bin == "" {
				bin = pigBinaries(root)
			}
			if bin == "" {
				return false, ""
			}
			out, err := exec.Command(bin, "--version").CombinedOutput()
			if err != nil {
				return false, fmt.Sprintf("%s --version: %v (%s)", bin, err, strings.TrimSpace(string(out)))
			}
			return true, ""
		},
	},
	{
		id:      "A7",
		subject: "Docker daemon 可达（compose 起 edge 的前提）",
		needs:   "一个可用的 Docker daemon",
		how:     "启动 Docker Desktop 或 dockerd，然后重跑本命令",
		run: func(root string) (bool, string) {
			if _, err := exec.LookPath("docker"); err != nil {
				return false, ""
			}
			cmd := exec.Command("docker", "info")
			cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				return false, fmt.Sprintf("docker info: %s", strings.TrimSpace(string(out)))
			}
			return true, ""
		},
	},
	{
		id:      "A8",
		subject: "一次真实的模型推理",
		needs:   "一个真实 provider 的 API key（云厂商密钥不得进节点）",
		how: "导出 OPSKEEPER_ACCEPTANCE_PROVIDER_KEY（可选 OPSKEEPER_ACCEPTANCE_PROVIDER_BASE_URL / " +
			"…_MODEL / OPSKEEPER_ACCEPTANCE_BASE_URL）后重跑；本命令会真的发一次请求，不打印 key",
		run: func(root string) (bool, string) {
			key := strings.TrimSpace(os.Getenv(keyEnv))
			if key == "" {
				// No key is MISSING, not pass. The contract in run() turns
				// an empty reason plus a non-empty needs into MISSING, and
				// that is the honest reading: the chain could not be asked.
				return false, ""
			}
			// Present is not working. This is the whole reason the check
			// exists, so the call is made here rather than merely described
			// in the subject line.
			return realInference(key)
		},
	},
}

// The environment this check reads.
//
// The four names are separate on purpose. A deployment that wants the
// evidence to be about the *gateway* rather than about the vendor points
// OPSKEEPER_ACCEPTANCE_BASE_URL at its own /v1 and supplies the node token —
// that is the stronger claim, because it exercises the path the node takes.
// Pointing it straight at the vendor is the weaker claim and the default.
//
// The provider defaults are named separately so that a deployment reading
// this file can tell which of the two it is running: same variable name for
// both would make the evidence ambiguous.
const (
	keyEnv           = "OPSKEEPER_ACCEPTANCE_PROVIDER_KEY"
	providerURLEnv   = "OPSKEEPER_ACCEPTANCE_PROVIDER_BASE_URL"
	providerModelEnv = "OPSKEEPER_ACCEPTANCE_PROVIDER_MODEL"
	baseURLEnv       = "OPSKEEPER_ACCEPTANCE_BASE_URL"
	modelEnv         = "OPSKEEPER_ACCEPTANCE_MODEL"

	defaultProviderBaseURL = "https://api.openai.com/v1"
	defaultProviderModel   = "gpt-4o-mini"
)

// inferenceTimeout bounds the call.
//
// It is generous because a cold TLS handshake plus a real provider queue is
// genuinely slow, and tight enough that a hung endpoint ends the chain
// rather than the afternoon. The one token this asks for is the cheapest
// thing a provider will sell; the point is that an answer came back, not how
// much of it there was.
const inferenceTimeout = 45 * time.Second

// realInference makes one minimal streamed completion and reports whether it
// arrived.
//
// Why streamed when the claim is only "an answer came back": stage 0's
// acceptance is explicitly about 流式输出, and a non-streaming probe would
// pass while the SSE frames the console renders are broken. Asking for a
// stream and reading the first data line tests both halves for the price of
// one token.
//
// Why the key is scrubbed from every message. Providers echo the credential
// back in their errors — "Incorrect API key provided: sk-…" is the standard
// one — and this command's output is the thing a person pastes into a
// ticket. A check that prints the secret it was given while reporting that
// the secret did not work is worse than no check.
func realInference(key string) (bool, string) {
	base, model := inferenceTarget()
	endpoint := strings.TrimSuffix(base, "/") + "/chat/completions"

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"stream":     true,
		"messages": []map[string]string{
			{"role": "user", "content": "ok"},
		},
	})
	if err != nil {
		return false, fmt.Sprintf("encoding the request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), inferenceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Sprintf("%s is not a usable url: %v", redact(base, key), err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", redact(base, key), redact(err.Error(), key))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Sprintf("%s returned %d: %s", redact(base, key), resp.StatusCode,
			strings.TrimSpace(redact(string(snippet), key)))
	}

	// Read enough of the stream to prove frames arrived, then stop. The
	// LimitReader is what keeps a working endpoint from making this check
	// cost a full response.
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false, fmt.Sprintf("%s: reading the stream: %v", redact(base, key), redact(err.Error(), key))
	}
	if !strings.Contains(string(prefix), "data:") {
		return false, fmt.Sprintf("%s answered 200 but sent no stream frames; the first bytes were: %s",
			redact(base, key), strings.TrimSpace(redact(string(prefix), key)))
	}
	return true, ""
}

// inferenceTarget resolves which endpoint and model this chain should call.
//
// Gateway settings win over provider settings when both are present, because
// the gateway is the path the node takes and evidence about the node is worth
// more than evidence about the vendor.
func inferenceTarget() (base, model string) {
	if v := strings.TrimSpace(os.Getenv(baseURLEnv)); v != "" {
		base = v
	} else {
		base = strings.TrimSpace(os.Getenv(providerURLEnv))
		if base == "" {
			base = defaultProviderBaseURL
		}
	}
	if v := strings.TrimSpace(os.Getenv(modelEnv)); v != "" {
		model = v
	} else {
		model = strings.TrimSpace(os.Getenv(providerModelEnv))
		if model == "" {
			model = defaultProviderModel
		}
	}
	return base, model
}

// redact removes the credential from text that is about to be printed.
//
// A substring replace rather than a mask, because the useful part of a
// provider's error is the sentence around the key, and replacing the whole
// line with asterisks throws that away. An empty or very short key is left
// alone: replacing a one-character string everywhere would corrupt the
// message without protecting anything.
func redact(s, key string) string {
	if len(key) < 8 {
		return s
	}
	return strings.ReplaceAll(s, key, "<redacted>")
}

// result is one check's outcome, kept so the tests can assert on the shape
// rather than on the printed text.
type result struct {
	id      string
	subject string
	st      status
	reason  string
}

// run evaluates every check against root.
//
// A check whose input is missing is reported MISSING with the input's name,
// never as a pass: a chain that silently skips three of its steps and exits
// zero is a chain that reports success it did not verify.
func run(root string, checks []check) []result {
	out := make([]result, 0, len(checks))
	for _, c := range checks {
		ok, reason := c.run(root)
		res := result{id: c.id, subject: c.subject, reason: reason}
		switch {
		case ok:
			res.st = statusPass
		case reason != "":
			res.st = statusFail
		case c.needs != "":
			res.st = statusMissing
			res.reason = c.needs
		default:
			res.st = statusFail
			res.reason = "检查没有通过，也没有说明原因"
		}
		out = append(out, res)
	}
	return out
}

func read(root, rel string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	return string(raw), nil
}

func main() {
	chain := "stage0"
	var rootEnv, childEnv string
	var positional []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case arg == "--chain=federation":
			chain = "federation"
		case strings.HasPrefix(arg, "--root-env="):
			rootEnv = strings.TrimPrefix(arg, "--root-env=")
		case strings.HasPrefix(arg, "--child-env="):
			childEnv = strings.TrimPrefix(arg, "--child-env=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(os.Stderr, "acceptance: unknown flag %q\n", arg)
			os.Exit(2)
		default:
			positional = append(positional, arg)
		}
	}
	root := "."
	if len(positional) > 0 {
		root = positional[0]
	}

	// A flag that the chosen chain does not read is a usage error, not
	// something to accept and drop: an operator who passed --child-env
	// and got a clean stage-0 run would conclude the federation chain had
	// been checked, which is the opposite of what happened.
	if chain != "federation" && (rootEnv != "" || childEnv != "") {
		fmt.Fprintf(os.Stderr, "acceptance: --root-env / --child-env only apply to --chain=federation (chain is %q)\n", chain)
		os.Exit(2)
	}

	checks, err := chainChecks(chain, rootEnv, childEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "acceptance: %v\n", err)
		os.Exit(2)
	}
	results := run(root, checks)
	failed, missing := 0, 0
	for _, r := range results {
		line := fmt.Sprintf("  %-7s %-3s %s", r.st, r.id, r.subject)
		switch r.st {
		case statusPass:
		case statusMissing:
			missing++
			line += "\n          needs: " + r.reason
		default:
			failed++
			line += "\n          " + r.reason
		}
		fmt.Println(line)
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].id < results[j].id })
	fmt.Printf("\nacceptance[%s]: %d passed, %d failed, %d need an input this machine may not have\n",
		chain, len(results)-failed-missing, failed, missing)
	switch {
	case failed > 0:
		// Deliberately not "这是本仓的缺陷". A needs-input check that ran and
		// failed — a credential that is present but does not work, a daemon
		// that is installed but not running — is an environment fact, and a
		// message that calls every red an in-repository defect teaches the
		// reader to discount the sentence that matters.
		fmt.Println("            有检查红了：离线检查红是本仓的缺陷；needs-input 检查红是这台机器的输入不成立——上面那条 reason 写的就是哪一种。")
		os.Exit(1)
	case missing > 0:
		fmt.Println("            缺失的输入已逐条点名。补上再跑一次，才算验收通过。")
		os.Exit(3)
	default:
		fmt.Println("            交付链上的每一项都在这里成立。")
	}
}

// pigBinaries reports where a locally built node agent can be found on this
// machine, or "" when there is none.
//
// It derives the cross-compiled location from the same shape the Makefile's
// build-pig-* rules write to — bin/<goos>-<goarch>/pig — rather than keeping
// its own list of paths. That list is what this function replaced: it held
// bin/pig and core/pig/bin/pig, and no build rule in this repository writes
// either one. So on a tree where `make build-pig-all` had been run and four
// working binaries were sitting in bin/, this check still reported MISSING
// and named a build command as the remedy — advice that was already
// satisfied. A checker whose search path is a hand-maintained copy of a build
// rule's output path is a second source of truth about where the binary
// lands, and that copy was wrong.
//
// The two historical locations are still tried, after the derived one, so a
// tree that puts the binary somewhere else by hand is not newly broken by
// this. What is gone is the claim that those two are where it goes.
func pigBinaries(root string) string {
	// The derived path comes first: it is the one the build rules write.
	for _, candidate := range []string{
		filepath.Join(root, "bin", runtime.GOOS+"-"+runtime.GOARCH, "pig"),
		filepath.Join(root, "bin", "pig"),
		filepath.Join(root, "core", "pig", "bin", "pig"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
