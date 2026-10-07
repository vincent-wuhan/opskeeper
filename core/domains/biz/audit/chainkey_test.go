package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 决策 327：「节点没有链的密钥」这句话此前没有任何东西守着。
//
// 它是本仓关于审计最硬的一句断言，也是最该被人惦记的一句：节点是唯一一台
// 「跑着高权限工具、还自己记账」的机器，而它的行最终要进控制面那条链。
// 如果节点能拿到链的密钥，它就能给自己的行盖章——**一个能给自己签名的证人
// 不需要被采信**。这句话在台账里写着，但一句散文拦不住任何人 import 什么。
//
// 这三组用例把它变成可判的：哪些环境变量是链的钥匙、通往盖章器的门只有哪几扇、
// 以及节点那一侧必须**够不到**其中任何一样。最后一条是本刀的正身。

// repoRoot is four levels up from core/domains/biz/audit. The gate walks
// the whole tree rather than its own module because the property is about
// the repository: a node that could reach the key does not do it by
// importing anything this module can see.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "routeaudit", "main.go")); err != nil {
		t.Fatalf("%s does not look like the repository root: %v", root, err)
	}
	return root
}

// productionFiles walks the tree and returns every non-test .go file.
//
// The gate directories are skipped because they name the constructors as
// strings on purpose — that is how they find them, and counting their own
// tables would make this gate report itself.
func productionFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			switch rel {
			case "scripts", ".git", "docs", "deploy", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkDeclared matches what the tree contains against what a table
// declares, in both directions.
//
// Both directions matter for the reason decisions 314 and 315 were about: a
// table that only catches additions stops describing the tree the day
// something is removed, and the row that lies about a removal is the one
// that makes a reader stop believing the whole table.
func checkDeclared(t *testing.T, what string, hits []string, declared map[string]string) {
	t.Helper()
	for _, rel := range hits {
		if _, ok := declared[rel]; !ok {
			t.Errorf("%s: %s is not one of the declared holders (%s)", what, rel, strings.Join(sortedKeys(declared), ", "))
		}
	}
	for rel, why := range declared {
		found := false
		for _, h := range hits {
			if h == rel {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s is declared but does none: %s", what, rel, why)
		}
	}
}

// chainKeyEnvRE finds a chain key being read out of the environment. The
// name has to contain both halves: AUDIT alone is the retention setting and
// HMAC alone could be any other subsystem's, and a gate matching either
// would be measuring the wrong thing.
var chainKeyEnvRE = regexp.MustCompile(`os\.Getenv\("([A-Z0-9_]*(?:AUDIT|CHAIN)[A-Z0-9_]*HMAC[A-Z0-9_]*)"\)`)

// 1. 每一个读链钥匙的环境变量都在表里，而且表里写的进程与代码读到的那一处对得上。
type chainKeyReader struct {
	// Process is the cmd/ directory the tree says reads it. The gate
	// compares this against the file the read was actually found in, which
	// is the only way the row stays a statement about the tree rather than
	// a sentence somebody wrote once.
	Process string
	// Why is what that process may hold a chain key.
	Why string
}

var declaredChainKeyReaders = map[string]chainKeyReader{
	"OPSKEEPER_AUDIT_HMAC_KEY": {
		Process: "cmd/opskeeper",
		Why:     "控制面自己的链。只有它盖章，控制台的每一次点击与节点上报的每一行才在同一条链上（§4.259 用一条 e2e 把这件事钉住了）",
	},
	"OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY": {
		Process: "cmd/higress-console",
		Why:     "网关自己的链。决策 324 给它单独立链：它持有 OPSKEEPER_JWT_SECRET，能写控制面链就等于能伪造控制面审计",
	},
}

func TestEveryChainKeyEnvVarIsDeclaredWithItsProcess(t *testing.T) {
	root := repoRoot(t)
	found := map[string][]string{}
	for _, rel := range productionFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, m := range chainKeyEnvRE.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], rel)
		}
	}
	if len(found) == 0 {
		t.Fatal("no chain key is read anywhere; the constructors moved and this gate is looking at nothing")
	}
	for name, readers := range found {
		decl, ok := declaredChainKeyReaders[name]
		if !ok {
			t.Errorf("%s is read by %v but is not in the declared table; a process that can read a chain key can forge that chain", name, readers)
			continue
		}
		if len(readers) > 1 {
			t.Errorf("%s is read by %d files (%v); one key, one reader", name, len(readers), readers)
			continue
		}
		if got := filepath.ToSlash(filepath.Dir(readers[0])); got != decl.Process {
			t.Errorf("%s is read by %s, but the table declares %s: %s", name, got, decl.Process, decl.Why)
		}
	}
	for name := range declaredChainKeyReaders {
		if len(found[name]) == 0 {
			t.Errorf("%s is declared but nothing reads it; drop the row or fix the reading code", name)
		}
	}
}

// 2. 通往盖章器的门有两扇，都得有名有姓。
//
// **第一版这把量具量错了东西**：它去找 `NewChainStamper(`，然后发现
// cmd/opskeeper 与 cmd/higress-console 都不调它——它们调的是导出的门
// `WithChain(`，盖章器在门里面被造出来。表里那两行于是当场变成「声明了却
// 没有」，而一个量具在自己第一版就指认错了对象，之后每一条结论都要重新怀疑。
// 所以两扇门一起量，且**只有内扇才是真正的构造点**：
//
//   - 外扇 WithChain：装配根用它把一把 key 换成一条链。跨模块，公开。
//   - 内扇 NewChainStamper：只应出现在 WithChain 的实现里。
//
// 这也说明边界不在类型上而在这些行上：一个进程只要拿到 key 就能自己造一个
// 盖章器，type system 管不着——这是「谁能持有咽喉」那张表（决策 110）的另一半，
// 连咽喉是谁造出来的都要有名字。
var stamperCtorRE = regexp.MustCompile(`\bNewChainStamper\(`)
var chainOptionRE = regexp.MustCompile(`\b(?:managerbizaudit|audit|domainaudit)\.WithChain\(`)

// stamperCtorHolders is every file where that constructor may appear —
// which is two, and the split is the point.
//
//	chain.go is where the constructor is **defined**. A definition has to
//	live somewhere, and pretending otherwise would mean the gate had to
//	tell a definition from a call by reading syntax, which is exactly the
//	kind of cleverness that makes a gate wrong in a new way. Naming it is
//	cheaper and says the same thing.
//	usecase.go is the only **caller**, because WithChain is the only door.
//
// The key itself is an unexported field, so "defined here" is not "usable
// from here": a package can name the constructor and still be unable to
// stamp anything, which is the state this file is in.
var stamperCtorHolders = map[string]string{
	"core/domains/biz/audit/chain.go":   "定义所在处。key 是非导出字段，「定义在这里」不等于「能用」：本包能命名这个构造器，仍然造不出一个盖章器",
	"core/domains/biz/audit/usecase.go": "WithChain 的实现。它是这条路上唯一调用这个构造的地方",
}

// chainOptionCallers are the only files allowed to open the outer door.
var chainOptionCallers = map[string]string{
	"cmd/opskeeper/main.go":            "控制面装配：读 OPSKEEPER_AUDIT_HMAC_KEY，装配出控制面那条链",
	"cmd/higress-console/auditsink.go": "网关装配：读它自己那把 key，装配出网关那条链（决策 324）",
}

func TestOnlyTheDeclaredHoldersOpenTheChainDoors(t *testing.T) {
	root := repoRoot(t)
	var ctors, callers []string
	for _, rel := range productionFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if stamperCtorRE.Match(src) {
			ctors = append(ctors, rel)
		}
		if chainOptionRE.Match(src) {
			callers = append(callers, rel)
		}
	}
	checkDeclared(t, "builds a chain stamper", ctors, stamperCtorHolders)
	checkDeclared(t, "opens the chain option", callers, chainOptionCallers)
}

// 3. **本刀的正身**：节点那一侧够不到链。
//
// 不是「不 import」，是**够不到**：不构造盖章器、不打开链的选项、不读任何一把
// 链的钥匙、也不 import 那个持链的域。这四条里任何一条破了，节点就能给自己的
// 行盖章，而这条链的全部价值就是「行由中心盖章」。
var nodeSidePrefixes = []string{"core/edge/", "cmd/opskeeper-edge/"}

func TestTheNodeSideCannotReachTheChainKey(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, rel := range productionFiles(t, root) {
		onNodeSide := false
		for _, p := range nodeSidePrefixes {
			if strings.HasPrefix(rel, p) {
				onNodeSide = true
			}
		}
		if !onNodeSide {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(src)
		if stamperCtorRE.MatchString(body) {
			offenders = append(offenders, rel+": builds a chain stamper")
		}
		if chainOptionRE.MatchString(body) {
			offenders = append(offenders, rel+": opens the chain option")
		}
		for _, m := range chainKeyEnvRE.FindAllStringSubmatch(body, -1) {
			offenders = append(offenders, rel+": reads chain key "+m[1])
		}
		if strings.Contains(body, "domains/biz/audit") {
			offenders = append(offenders, rel+": imports the audit domain, which owns the stamper")
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("the node side can reach the chain, and a node that can stamp its own rows makes every row it files unfalsifiable:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// 4. 决策 328：**每条链都必须有验证者。**
//
// 这一刀就是从违反这条规则开始的：决策 324 给网关立了一条链，写它的注释说
// 「Two chains, two keys, two verifiers」，而全仓 VerifyChain 的生产调用方
// 只有一个，在控制面那侧。**一条只写不验的链是装饰品**——它给每行算一次
// HMAC，什么也没换回来，因为能改数据库的人可以把改过的那一行的摘要重算成
// 自洽的样子，只要没有人去走一遍。
//
// 所以这道闸门与 4.260 的那三道是同一个思路的另一半：那三道问「谁能**拿到**钥匙」，
// 这一道问「拿到之后有没有人**用**它」。**一把只有锁没有人的钥匙，和没有锁一样。**
var verifierCallRE = regexp.MustCompile(`\.VerifyChain\(`)

// declaredChainVerifiers says, for each chain, which files are allowed to
// verify it — and therefore which chains are required to have a row here.
var declaredChainVerifiers = map[string]struct {
	// Env is the key whose holder owns this chain.
	Env string
	// Callers are the files allowed to walk this chain.
	Callers map[string]string
}{
	"OPSKEEPER_AUDIT_HMAC_KEY": {
		Env: "OPSKEEPER_AUDIT_HMAC_KEY",
		Callers: map[string]string{
			"core/domains/server/audit/http.go": "GET /v1/admin/audit-logs/chain —— 运维问「这份记录被改过吗」的那一端",
			// 这一行是**这道闸门自己抓出来的**，此前从来没有出现在任何表里：
			// agent kernel 早就拿着 verifier 在验链了，而且它的注释写得比谁都清楚
			// （「内核的条目只在它们所在的那条链还可信时才可信」），只是没有任何
			// 闸门要求它被登记。一个已经做对了的事被查出来时，先登记再问它对不对
			// ——而不是因为它不在表上就当它没有发生过。
			"core/manager/biz/aiops/agentkernel/audit.go": "agent kernel 在回答「这次工具调用被篡改过吗」。它的 Verify 在没有 verifier 时返回 ErrNoChainVerifier 而不是 nil——**一个因为没人给它检查办法就报「链完整」的账本，正是这个方法要防的事**，而它比本仓任何一版文档都先写下了这句",
		},
	},
	"OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY": {
		Env: "OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY",
		Callers: map[string]string{
			"core/manager/higress/chain.go": "GET /admin/audit-chain —— 决策 328 补上的那一端，与控制面逐字同义",
		},
	},
}

func TestEveryDeclaredChainHasAVerifierThatSomebodyCalls(t *testing.T) {
	root := repoRoot(t)
	var callers []string
	for _, rel := range productionFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if verifierCallRE.Match(src) {
			callers = append(callers, rel)
		}
	}
	// 每一把钥匙都必须有一份「谁来验它」，而且那份名单不许是空的：
	// 一条没有验证者的链不是「暂时没人关心」，是它根本没有存在的理由。
	declaredCallers := map[string]string{}
	for name, spec := range declaredChainVerifiers {
		if len(spec.Callers) == 0 {
			t.Errorf("the chain held by %s has no declared verifier; a chain nobody walks is decoration", name)
			continue
		}
		for rel, why := range spec.Callers {
			declaredCallers[rel] = why
		}
	}
	checkDeclared(t, "walks a chain", callers, declaredCallers)
}

// 5. 立了链还得把它接到能被问到的那一端。
//
// 上一条问「有没有人验」，这一条问「验的那一端在不在这个进程里」。网关那条链
// 之所以曾经无人验证，是因为它虽然立了，却根本没有被交给任何路由——
// **一个只被构造出来、没有被任何人拿到的对象，和不存在是同一个东西**。
func TestTheGatewayProcessHandsItsChainToSomethingThatCanBeAsked(t *testing.T) {
	root := repoRoot(t)
	rel := "cmd/higress-console/main.go"
	src, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	body := string(src)
	// Both halves: the sink is built (the chain exists) AND it is handed to
	// the server (something can ask about it). A buildAuditSink that never
	// reaches higress.Config is the exact shape decision 324 shipped.
	if !strings.Contains(body, "buildAuditSink(") {
		t.Errorf("%s no longer builds the gateway's chain", rel)
	}
	// 第一版这条判据是 `Chain:\s*\w`，而变异把 main.go 改成 `Chain: nil` 之后
	// 它照样通过——**一个能匹配 nil 的「有没有交出去」判据，量的不是有没有交出去**。
	// 现在它必须指出那个变量名，并且那个变量必须是从 buildAuditSink 得到的那个。
	sinkVar := gatewaySinkVarRE.FindStringSubmatch(body)
	if len(sinkVar) == 0 {
		t.Errorf("%s no longer captures the sink built by buildAuditSink; the handover check below has nothing to check", rel)
		return
	}
	handover := regexp.MustCompile(`Chain:\s*` + regexp.QuoteMeta(sinkVar[1]) + `\b`)
	if !handover.MatchString(body) {
		t.Errorf("%s builds the gateway's chain into %s and never hands it to the server; a chain nothing can ask about is not a chain (决策 328)",
			rel, sinkVar[1])
	}
}

// gatewaySinkHandoverRE captures the variable buildAuditSink's result lands in.
var gatewaySinkVarRE = regexp.MustCompile(`(\w+),\s*err\s*:?=\s*buildAuditSink\(`)
