package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	throatPath  = "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	rowTypePath = "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	// The control plane is three modules away from here, not one directory
	// up. This package moved out to core/base so the domains resting on it
	// could be lifted after it (decision 221), and the audit ledger itself
	// moved to core/domains after that (decision 226) — it is depended upon
	// by four domains, so it could never have been part of the release
	// floor, but it does not depend on core/manager and that is the edge the
	// module system enforces. The test walks the tree, so the path has to
	// follow the tree and not the package.
	managerRoot = "../../../manager"
	domainsRoot = "../../../domains"
)

// controlPlaneRoots is walked in order. The control plane is two Go modules
// now, and a key in the tables below is qualified by which one it lives in —
// not because the directory names collide today (they do not) but because a
// single unqualified key would silently start matching a same-named directory
// in the other tree the day one appears, and this test reports on trust
// rather than on error.
//
// It is a var and not a third const because a []string is not a constant
// expression, which is the kind of thing the compiler says out loud.
var controlPlaneRoots = []string{managerRoot, domainsRoot}

// throatHolders are the packages allowed to hold the write path.
//
// The rule is not "the audit domain may write" — it is that exactly one
// throat writes, and everyone else asks to be remembered through the port
// above. A package on this list is on it because it is the writer, an
// adapter onto the writer, or a test that needs a real one behind it; the
// reason is not decoration, it is what the next person reads before
// adding themselves.
//
// Decision 35 put this throat in biz/audit so that every HLD-010 row
// passes through one place, and that remains true. What decision 109
// removed was the other half of the same decision leaking outward: naming
// a row required importing the writer, so six domains imported it to say
// "this happened". The list below is what is left after that, and it is
// short enough to read.
//
// Decision 272 removed three more of the six, and they are worth naming
// because they were the ones that looked most justified:
//
//   - chatdiagnose's AuditAdapter wrapped *audit.Usecase and called Emit
//   - agentkernel declared LedgerWriter and LedgerVerifier and then spelled
//     the argument in bizaudit.Event, which is an alias of auditport.Event
//   - frontierbound handed the ledger []NodeLedgerRow, so the call could not
//     be made without naming the row
//
// Each of those was a real dependency on the audit *domain* to say one
// thing, and each is now an interface in this package. The list below went
// from six entries to three, and two of the three that remain are tests.
// The throat did not move, did not weaken, and gained no second write path —
// what moved is the four callers' knowledge of where it lives, which is the
// only thing that was ever wrong.
// biz/audit is absent on purpose: it is the definition of the throat, not a
// holder of it, and a package never imports itself. If a file inside it
// ever does, that is a cycle and the compiler will say so before this test
// gets a chance to.
var throatHolders = map[string]string{
	"domains/server/middleware": "the audit middleware enriches the request (status, IP, request id) " +
		"and is the only thing that turns a handler's request into a call to the writer. " +
		"Decision 272 moved the middleware itself onto the port — AuditMiddleware now takes " +
		"an auditport.Sink and its production file imports core/domains/biz/audit no more. " +
		"What is left here is the test, which builds a real usecase to assert a row actually " +
		"lands; a test that ran against a fake would prove the middleware called something, " +
		"not that anything was written",
	"domains/server/audit": "the ledger's own reader: it lists rows, reports chain state and " +
		"surfaces ErrChainDisabled, so it holds the usecase rather than a copy of it",
	"domains/server/plugin": "its test builds a real usecase to assert a plugin release lands in " +
		"the chain; the production handler uses the port",
	// 决策 327 加进来的第四个测试持有者，与上面三个理由同形：生产代码走端口，
	// 只有测试需要真的写入器在后面。frontierbound 的 agent.audit.entries 此前
	// 从来没有端到端证据（决策 326 之前每一跳都止于一个假），而能证明「一行真的
	// 落进链里并接上了上一行」的写法只能是拿真的 Usecase。
	//
	// **加进来的是测试，不是生产依赖**——所以紧跟着下面那条断言要求它的非测试
	// 文件一个都不许 import 写入器。一张会随声明一起变宽的表等于没有表：这一行
	// 若只被当成「豁免」，那么下一次有人在 frontierbound 的 handler 里直接
	// Emit，闸门照样绿。
	"manager/service/frontierbound": "its e2e test builds a real usecase to assert a node's ledger " +
		"row lands in the real chain and links to the row before it (decision 326); every production " +
		"file in this package goes through auditport and imports the writer none — asserted below",
}

// testOnlyThroatHolders are the entries whose grant covers their **test**
// files and nothing else. A grant that is not scoped stops being a judgement
// and becomes a note.
var testOnlyThroatHolders = map[string]bool{
	"manager/service/frontierbound": true,
}

// TestTheTestOnlyThroatHoldersReachTheWriterOnlyFromTests is what keeps the
// grant above honest. Without it, "it is only a test" is a sentence; with
// it, adding a production import to that package turns the tree red.
func TestTheTestOnlyThroatHoldersReachTheWriterOnlyFromTests(t *testing.T) {
	files := walkControlPlane(t)
	for _, pf := range files {
		if !testOnlyThroatHolders[pf.dir] || strings.HasSuffix(pf.path, "_test.go") {
			continue
		}
		for _, imp := range pf.file.Imports {
			if target, _ := strconv.Unquote(imp.Path.Value); target == throatPath {
				t.Errorf("%s reaches the writer from a production file; its grant covers tests only: %s",
					pf.path, throatHolders[pf.dir])
			}
		}
	}
}

// rowTypeReaders are the packages allowed to name the persisted row.
//
// model/audit holds two things now that the vocabulary moved out (decision
// 109): the GORM entities, and re-exported constants. The entities are
// storage, and storage has to be readable by exactly the code that already
// understood the table.
//
// That sentence used to end with a fourth reader: query_change_events, the
// RCA tool that joins a configuration change to the operator who authorised
// it. It read the entity because its seam's return type was `[]auditmodel.Log`
// — an interface whose signature named somebody else's struct. Decision 273
// published the nine fields it actually needs as core/base/pkg/audit.ChangeRow
// and the tool reads the projection, so it is off this list.
//
// **Six entries became five: three production readers and two tests.** What is
// left in production is the store that persists, the view that lists, and the
// writer that maps an Event onto a row; the other two are tests that assert a
// row actually landed. That is the shape this list was supposed to reach: the
// entity belongs to the code that writes it, and a reader outside that set now
// has to say which fields it wants rather than which struct it may hold.
var rowTypeReaders = map[string]string{
	"domains/data/audit/store":  "persistence: the entity, the chain head, the migration",
	"domains/server/audit":      "the ledger view lists and filters rows",
	"domains/biz/audit":         "the writer maps an Event onto the entity",
	"domains/server/plugin":     "its test asserts on persisted rows",
	"domains/server/middleware": "its test asserts on the row the middleware emitted",
	// 决策 326/327：frontierbound 的 e2e 要断言的不是「Usecase 被调用了」，
	// 而是**链上的列真的接上了**——PrevHash 等于上一行的 Hash。这两个字段只长在
	// 持久化的行上，端口里没有它们，所以它确实是个读者而不是命名者。
	"manager/service/frontierbound": "its e2e test reads the chained rows back and asserts the node's " +
		"rows link to each other; the chain columns exist only on the persisted entity",
}

// TestOnlyTheThroatHoldsTheWriter is the manager-wide form of the rule
// decision 109 applied to iam.
//
// iam was fixed one domain at a time, and the other five were still
// importing the writer to name a row: alert, knowledge, setting, plugin and
// the MCP surface. Each of those edges looked harmless — a handler builds
// an Event and hands it to SetAuditEvent — and together they meant six
// domains could reach the one component whose entire value is that it is
// the only way a row gets written. A boundary that six packages lean on is
// a boundary with six people's changes behind it.
//
// The check walks the whole module rather than one context, because the
// defect was never context-local: each domain looked fine on its own.
func TestOnlyTheThroatHoldsTheWriter(t *testing.T) {
	files := walkControlPlane(t)
	if len(files) == 0 {
		t.Fatal("no files were parsed; the walk is broken, not the boundary")
	}

	seenThroat := map[string]bool{}
	seenRow := map[string]bool{}
	for _, pf := range files {
		dir := pf.dir
		for _, imp := range pf.file.Imports {
			target, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: unquote import: %v", pf.path, err)
				continue
			}
			switch target {
			case throatPath:
				seenThroat[dir] = true
				if why, ok := throatHolders[dir]; !ok {
					t.Errorf("%s imports the audit throat (%s). Name the row through "+
						"core/base/pkg/audit instead; the writer does not move, only the shape does",
						pf.path, target)
				} else if testing.Verbose() {
					t.Logf("%s may hold the writer: %s", dir, why)
				}
			case rowTypePath:
				seenRow[dir] = true
				if _, ok := rowTypeReaders[dir]; !ok {
					t.Errorf("%s imports the row entity (%s). If it only needs to name a "+
						"row, use core/base/pkg/audit; if it genuinely persists or lists rows, "+
						"add the package to rowTypeReaders with the reason",
						pf.path, target)
				}
			}
		}
	}

	// The tables are only trustworthy if they are not quietly shrinking: a
	// reason that stopped being true should be deleted, and a package that
	// stopped reaching for the writer should be removed from the list.
	for dir := range throatHolders {
		if !seenThroat[dir] {
			t.Errorf("%s is listed as a throat holder but no longer imports it; delete the entry "+
				"and the reason with it", dir)
		}
	}
	for dir := range rowTypeReaders {
		if !seenRow[dir] {
			t.Errorf("%s is listed as a row reader but no longer imports the entity; delete the entry", dir)
		}
	}
}

// TestNoDomainOutsideTheListsReachesTheWriter is the same rule stated as a
// count, so the number is visible in a test log and in a review diff.
//
// Six domains reached for the writer to name a row before decision 110.
// The list above is the whole remaining set; if this count grows, a domain
// has started depending on the audit implementation again.
func TestNoDomainOutsideTheListsReachesTheWriter(t *testing.T) {
	files := walkControlPlane(t)
	domains := map[string]bool{}
	for _, pf := range files {
		dir := pf.dir
		if _, ok := throatHolders[dir]; ok {
			continue
		}
		for _, imp := range pf.file.Imports {
			target, _ := strconv.Unquote(imp.Path.Value)
			if target == throatPath {
				domains[dir] = true
			}
		}
	}
	if len(domains) > 0 {
		names := make([]string, 0, len(domains))
		for d := range domains {
			names = append(names, d)
		}
		t.Errorf("%d package(s) outside the throat holder list import the writer: %s",
			len(domains), strings.Join(names, ", "))
	}
}

// rel turns a walked path into a manager-relative package directory, which
// is how the two tables above are keyed. The walk yields "../.."-prefixed
// paths; the tables read like paths in the repository, and a table that
// says "../../../.." is a table nobody can check against a file path.
// rel names the package that holds a file, qualified by the module it lives
// in. The qualification is the point: a table entry reads "domains/server/
// middleware" rather than "server/middleware", so moving a package between
// the two control-plane modules makes the entry wrong loudly instead of
// making it quietly describe a different directory.
func rel(root, path string) string {
	dir := filepath.Dir(path)
	trimmed := strings.TrimPrefix(filepath.ToSlash(dir), filepath.ToSlash(root)+"/")
	return filepath.Base(root) + "/" + trimmed
}

type walkedFile struct {
	path string
	file *ast.File
	// dir is the tree-qualified package key, e.g. "domains/server/plugin".
	dir string
}

// walkControlPlane parses every Go file of both control-plane modules.
// parser.ParseDir is not recursive and these trees are four levels deep at
// minimum. The two roots are walked as one list on purpose: the rule being
// checked — only the throat writes an audit row — is a property of the
// control plane, and checking half of it would report the same green whether
// or not the other half had started reaching for the writer.
func walkControlPlane(t *testing.T) []walkedFile {
	t.Helper()
	fset := token.NewFileSet()
	var out []walkedFile
	for _, root := range controlPlaneRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			out = append(out, walkedFile{path: path, file: file, dir: rel(root, path)})
			return nil
		})
		if err != nil {
			t.Fatalf("walk the %s module: %v", filepath.Base(root), err)
		}
	}
	return out
}
