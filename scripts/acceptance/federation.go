package main

// The federation acceptance chain.
//
// Why this exists. E3 — "a second control plane plus one real policy
// delivery" — was the one external claim in the audit with no command
// behind it. The other three had one: E1 is this file's sibling stage0
// chain, E2 is `make test-e2e-live`, E4 needs a remote system this
// repository does not have. A claim whose closer is "本仓暂无" is a claim
// that stays open forever, because nothing in the tree is watching it and
// nobody can tell whether it is waiting for a deployment or was abandoned.
//
// What it can and cannot do, stated plainly. This chain verifies two
// different things and it is important not to blur them:
//
//   - the OFFLINE checks read this repository and assert that the
//     federation wiring's safety properties are still in the code. A
//     failure there is a defect here, and it is actionable by editing
//     this tree.
//
//   - the NEEDS-INPUT checks read the two control planes' own env files
//     and the child cluster's policy directory, and assert that a policy
//     actually arrived. That is real evidence of a real delivery: the
//     child keeps a `live` symlink pointing at the policy tree the root
//     sent, and the gate follows that link, so a link with a target behind
//     it is a delivery that reached the enforcement point.
//
// What it still does not do: start two managers, enrol, and watch a policy
// cross the wire. That remains a deployment step. This chain is what the
// deployment step is verified *with* — it turns "I believe it federated"
// into a command whose output says whether it did, which is the same move
// E1's chain made for stage 0.
//
// The two env files are separate because the two control planes are. In
// the common single-host trial both processes read the same environment,
// so both flags may be omitted and the process environment is used twice —
// that arrangement is itself a valid thing to verify (a root and a child
// on one host), and the chain does not pretend otherwise.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// chainChecks returns the check list for a named chain.
func chainChecks(chain, rootEnv, childEnv string) ([]check, error) {
	switch chain {
	case "stage0", "":
		return stage0Checks, nil
	case "federation":
		live, err := federationInputChecks(rootEnv, childEnv)
		if err != nil {
			return nil, err
		}
		return append(federationChecks(), live...), nil
	default:
		return nil, fmt.Errorf("unknown chain %q; this command has stage0 and federation", chain)
	}
}

// envSide is one control plane's configuration, read from a file when given
// and from the process environment otherwise.
type envSide struct {
	label string
	vars  map[string]string
	from  string
}

func loadSide(label, path string) (envSide, error) {
	side := envSide{label: label, vars: map[string]string{}, from: "process environment"}
	if path == "" {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				side.vars[k] = v
			}
		}
		return side, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return envSide{}, fmt.Errorf("read the %s env file: %w", label, err)
	}
	defer func() { _ = f.Close() }()
	side.from = path
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		side.vars[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		return envSide{}, fmt.Errorf("read the %s env file: %w", label, err)
	}
	return side, nil
}

func (e envSide) get(key string) string { return strings.TrimSpace(e.vars[key]) }

// needAll reports the first of keys this side left empty, or "".
func (e envSide) needAll(keys ...string) string {
	for _, k := range keys {
		if e.get(k) == "" {
			return k
		}
	}
	return ""
}

// federationChecks is the offline half: the properties of the wiring that
// live in this repository.
// needs-input checks can name a *file* in their reason: telling a
// deployer "the token is unset" when the variable they
// would set is in a file they have not pointed at is advice they cannot
// act on.
func federationChecks() []check {
	return []check{
		// ---- offline: this repository ----
		{
			id:      "F1",
			subject: "子集群不能同时持有签发策略的能力",
			run: func(root string) (bool, string) {
				body, err := read(root, "cmd/opskeeper/federation_child.go")
				if err != nil {
					return false, err.Error()
				}
				// The refusal, not the intent: a comment saying a child
				// must not sign policy is exactly the shape a later edit
				// can leave behind while the code stops refusing.
				if !strings.Contains(body, "but %s is set; a child receives policy and must not be able to sign it") {
					return false, "子集群与签发密钥并存的拒绝理由不在代码里；这道互斥检查可能被改没了"
				}
				return true, ""
			},
		},
		{
			id:      "F2",
			subject: "子集群的令牌 / 策略目录 / 信任库缺一即拒绝启动，而不是取默认值",
			run: func(root string) (bool, string) {
				body, err := read(root, "cmd/opskeeper/federation_child.go")
				if err != nil {
					return false, err.Error()
				}
				for _, want := range []string{
					"is required to enrol with a root",
					"is required; without a directory there is nowhere to put a policy",
					"is required; a child with no trusted key can enforce no policy",
				} {
					if !strings.Contains(body, want) {
						return false, fmt.Sprintf("拒绝理由 %q 不在了；子集群可能带着一个默认值起来", want)
					}
				}
				return true, ""
			},
		},
		{
			id:      "F3",
			subject: "根侧 artifact store 的令牌没有默认值",
			run: func(root string) (bool, string) {
				// The refusal lives in the sink, not in the wiring: the
				// wiring passes the env value straight through, and the
				// sink is the thing that refuses. Checking the wiring
				// for a default would be checking the wrong file.
				body, err := read(root, "core/domains/biz/federation/publish.go")
				if err != nil {
					return false, err.Error()
				}
				if !strings.Contains(body, "an http sink needs a token") {
					return false, "http sink 不再要求令牌；把签名策略树放进一个谁都能写的 store 会变成一个带 URL 的泄漏"
				}
				return true, ""
			},
		},
		{
			id:      "F4",
			subject: "摘要不一致被当作不可重试的冲突，而不是「还没发布」",
			run: func(root string) (bool, string) {
				body, err := read(root, "core/domains/biz/federation/published.go")
				if err != nil {
					return false, err.Error()
				}
				// Both halves matter and they are different claims: that
				// the error exists, and that it is the one error in the
				// file which is *not* retryable. A check that only looked
				// for the identifier would pass if someone made it
				// retryable, which is the change that matters.
				if !strings.Contains(body, "var ErrPublishedMismatch =") {
					return false, "ErrPublishedMismatch 不在了；冲突与「还没发布」又被混成一种答案"
				}
				if !strings.Contains(body, "deliberately NOT retryable") {
					return false, "摘要不一致不再被声明为不可重试；重试会永远冲突，而一个会永远冲突的答案不该伪装成可重试"
				}
				return true, ""
			},
		},
		{
			id:      "F5",
			subject: "两侧的必填项在 .env.example 里有条目，运维照着填得出来",
			run: func(root string) (bool, string) {
				body, err := read(root, "deploy/.env.example")
				if err != nil {
					return false, err.Error()
				}
				var missing []string
				for _, k := range []string{
					"OPSKEEPER_FEDERATION_ARTIFACT_DIR",
					"OPSKEEPER_FEDERATION_ARTIFACT_MANIFEST",
					"OPSKEEPER_FEDERATION_ARTIFACT_BASE_URL",
					"OPSKEEPER_FEDERATION_ARTIFACT_STORE_URL",
					"OPSKEEPER_FEDERATION_ARTIFACT_STORE_TOKEN",
				} {
					if !strings.Contains(body, k) {
						missing = append(missing, k)
					}
				}
				if len(missing) > 0 {
					return false, "env 模板里没有：" + strings.Join(missing, ", ")
				}
				return true, ""
			},
		},
	}
}

// federationInputChecks are the ones that read a deployment. They are
// separate so that a missing env file is an error about the command's own
// arguments (exit 2) rather than a MISSING on a check — telling an
// operator their check could not run because they passed a bad flag is
// more useful than telling them a variable is unset.
func federationInputChecks(rootEnvPath, childEnvPath string) ([]check, error) {
	child, err := loadSide("child", childEnvPath)
	if err != nil {
		return nil, err
	}
	rootSide, err := loadSide("root", rootEnvPath)
	if err != nil {
		return nil, err
	}

	return []check{
		{
			id:      "F6",
			subject: "子集群这一侧配齐了（角色 / 令牌 / 策略目录 / 信任库）",
			needs:   "子集群的 env 文件——用 --child-env=<path> 指给本命令",
			how:     "OPSKEEPER_FEDERATION_ROLE=child 且 TOKEN / POLICY_DIR / TRUST_STORE 三项非空；本命令只读不打印值",
			run: func(string) (bool, string) {
				if role := child.get("OPSKEEPER_FEDERATION_ROLE"); role != "child" {
					return false, ""
				}
				if missing := child.needAll(
					"OPSKEEPER_FEDERATION_TOKEN",
					"OPSKEEPER_FEDERATION_POLICY_DIR",
					"OPSKEEPER_FEDERATION_TRUST_STORE",
				); missing != "" {
					return false, ""
				}
				return true, ""
			},
		},
		{
			id:      "F7",
			subject: "根这一侧配齐了（签发密钥 / 产物目录 / 摘要清单 / store）",
			needs:   "根的 env 文件——用 --root-env=<path> 指给本命令",
			how:     "RELEASE_KEY / ARTIFACT_DIR / ARTIFACT_MANIFEST 非空，且 store 的 URL 与 token 成对（只读形状可以不填 store）",
			run: func(string) (bool, string) {
				if missing := rootSide.needAll(
					"OPSKEEPER_FEDERATION_RELEASE_KEY",
					"OPSKEEPER_FEDERATION_ARTIFACT_DIR",
					"OPSKEEPER_FEDERATION_ARTIFACT_MANIFEST",
				); missing != "" {
					return false, ""
				}
				// A store URL without a token is a half-configured
				// writable store, and the process refuses to start on
				// it — so it is a FAIL, not a MISSING: the deployer
				// supplied something and it was wrong.
				base, storeURL, storeToken := rootSide.get("OPSKEEPER_FEDERATION_ARTIFACT_BASE_URL"),
					rootSide.get("OPSKEEPER_FEDERATION_ARTIFACT_STORE_URL"),
					rootSide.get("OPSKEEPER_FEDERATION_ARTIFACT_STORE_TOKEN")
				if base == "" && storeURL == "" {
					return false, ""
				}
				if storeURL != "" && storeToken == "" {
					return false, "store URL 配了而 token 是空的：这是一个谁都能写的 store，进程会拒绝启动"
				}
				return true, ""
			},
		},
		{
			id:      "F8",
			subject: "策略确实到了子集群（live 链接指向一棵存在的策略树）",
			needs:   "一次真实的策略投递之后，子集群的 POLICY_DIR",
			how:     "先让根与子集群跑起来并投递一次；本命令读子集群的 live 符号链接，这是投递抵达执行点的证据",
			run: func(string) (bool, string) {
				dir := child.get("OPSKEEPER_FEDERATION_POLICY_DIR")
				if dir == "" {
					return false, ""
				}
				link := filepath.Join(dir, "live")
				target, err := os.Readlink(link)
				if err != nil {
					return false, ""
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				info, err := os.Stat(target)
				if err != nil {
					return false, fmt.Sprintf("live 链接指向 %s，而那里没有东西：链接建了但策略树不在", target)
				}
				if !info.IsDir() {
					return false, fmt.Sprintf("live 链接指向 %s，那不是一个策略目录", target)
				}
				return true, ""
			},
		},
	}, nil
}
