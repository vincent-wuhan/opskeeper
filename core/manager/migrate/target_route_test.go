package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 一条迁移的目标端点必须真实存在，而且它请求体里那些字段必须真的被那个
// 端点接收。否则这个工具会为每一行数据记一条 "失败: 404" 或一条 400，而
// 两者读起来都像数据错误，不像"这个工具写错了地方"。
//
// 决策 291 之前这件事没有任何东西拦着。core/manager/migrate/entity.go 只有
// 一个自由文本 Target 字段，import / verify / rollback 三个命令一律把它拼进
// URL，而五个 Target 里有四个在 manager 的路由表里没有对应物。集成测试也
// 抓不到，因为它的 mock 会给任何路径、任何请求体回 201。
//
// 所以这道闸门不去跑 HTTP，它去读源码，两问：
//
//  1. 注册表里的 TargetRoute 在 manager 里注册了吗？
//  2. FieldMap 的目标字段，是那个端点的 handler 真正解码的请求结构的
//     json tag 吗？
//
// 第二问是接上第一问之后立刻暴露的：端点找到了，字段依然对不上。

// routeTree 是从源码读出来的一份路由 → 请求字段的对照表。
type routeTree struct {
	routes     map[string]map[string]bool // "/v1/users" -> 该端点接受的 json 字段
	verbs      map[string]string          // "/v1/users" -> 写入用的动词
	writeVerbs map[string]bool
}

// registration 是一条 r.<Verb>("/v1/x", h.method) 与该方法解码的请求
// 结构体之间的对应关系。
type registration struct{ route, recv, method, verb, structName string }

// repoRoot 从 core/manager/migrate 回到仓库根。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func skipSourceDir(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", "vendor", "openspec", "testdata":
		return true
	}
	return false
}

func readTree(t *testing.T) routeTree {
	t.Helper()
	root := repoRoot(t)
	tree := routeTree{
		routes:     map[string]map[string]bool{},
		verbs:      map[string]string{},
		writeVerbs: map[string]bool{"Post": true, "Put": true, "Patch": true, "Delete": true},
	}

	// 先按包收齐"结构体 → json tag"与"路由 → 结构体"，最后才合并。
	//
	// 分包是必须的，不是讲究：注册在 iam/server/http.go 里的 r.Post 与
	// 解码请求体的 createUser 定义在同目录的另一个文件 orgs.go 里。第一版
	// 在单个文件内解析方法体，于是三个目标端点的 accepted 字段集全是空的，
	// 而第二道闸门对"空集合"直接跳过——它当时是绿的，而且是绿给了一个
	// 它根本没检查到的东西。这正是把"读到了 0 条"当成"通过了"的形状。
	// 按目录而不是按包名分组。包名会重复——这个树里有好几个 server、
	// 好几个 store——按包名分组会让后一个目录的 resolveStructNames 用着
	// 前一个目录的路径，于是字段集读成空集、闸门判成"读不出请求结构"。
	dirTags := map[string]map[string]map[string]bool{}
	dirRegs := map[string][]registration{}
	dirPkgs := map[string]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipSourceDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return nil
		}
		dir := filepath.Dir(path)
		if dirTags[dir] == nil {
			dirTags[dir] = map[string]map[string]bool{}
		}
		collectTags(file, dirTags[dir])
		dirRegs[dir] = append(dirRegs[dir], collectRegistrations(file)...)
		dirPkgs[dir] = file.Name.Name
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	for dir, regs := range dirRegs {
		pkg := dirPkgs[dir]
		resolveStructNames(dir, pkg, regs)
		for _, rg := range regs {
			if !strings.HasPrefix(rg.route, "/v1/") {
				continue
			}
			if _, seen := tree.routes[rg.route]; !seen {
				tree.routes[rg.route] = map[string]bool{}
			}
			// 同一条路由注册多次时（GET+POST 同一个 /v1/x），保留写入用的
			// 那个动词：导入只发 POST 与 DELETE，读的那一半不决定请求体。
			// 同一条路由既注册了读也注册了写时，留下写的那一半：导入只发
			// POST，而请求体由写入的 handler 决定，不是读的那一半。
			if cur, seen := tree.verbs[rg.route]; !seen ||
				(!tree.writeVerbs[cur] && tree.writeVerbs[rg.verb]) {
				tree.verbs[rg.route] = rg.verb
			}
			for tag := range dirTags[dir][rg.structName] {
				tree.routes[rg.route][tag] = true
			}
		}
	}
	return tree
}

// jsonTagName 从一个字段的 tag 字面量里取出 json 名字。
//
// tag 的内容是 `json:"email,omitempty"`——反引号里的字符串去掉引号之后是
// `json:"email,omitempty"`，它不是 JSON，用 json.Unmarshal 解析会失败。
// 第一版就是这么写的，于是所有结构体的 tag 集都是空的，第二道闸门读到的
// accepted 永远是 0，于是它对每个端点都报"读不出请求结构"——一个看上去在
//
//	complaining 的失败，实际上是解析器自己坏了。这里按 key:"value" 读。
func jsonTagName(tag *ast.BasicLit) (string, bool) {
	if tag == nil || tag.Kind != token.STRING {
		return "", false
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return "", false
	}
	for _, part := range strings.Split(raw, " ") {
		if !strings.HasPrefix(part, "json:") {
			continue
		}
		value := strings.Trim(strings.TrimPrefix(part, "json:"), `"`)
		if comma := strings.Index(value, ","); comma >= 0 {
			value = value[:comma]
		}
		if value == "" || value == "-" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// collectTags 记录每个结构体声明的 json tag。
func collectTags(file *ast.File, out map[string]map[string]bool) {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			tags := map[string]bool{}
			for _, field := range st.Fields.List {
				if name, ok := jsonTagName(field.Tag); ok {
					tags[name] = true
				}
			}
			if len(tags) > 0 {
				out[ts.Name.Name] = tags
			}
		}
	}
}

// collectRegistrations 记录每个 r.<Verb>("/v1/x", h.method)。
//
// "该方法解码哪个结构体"在同包内解析（见 readTree 的说明），而不是按方法名
// 猜——按名字猜就得维护一张 "createXxx 处理 CreateXxxReq" 的对照表，而那张
// 表本身就是这个检查要抓的那类断言。
func collectRegistrations(file *ast.File) []registration {
	var out []registration
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			verb := ""
			switch sel.Sel.Name {
			case "Get", "Post", "Put", "Delete", "Patch":
				verb = sel.Sel.Name
			default:
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			route, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			h, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := h.X.(*ast.Ident)
			if !ok {
				return true
			}
			out = append(out, registration{
				route: route, recv: recv.Name, method: h.Sel.Name, verb: verb,
			})
			return true
		})
	}
	return out
}

// resolveStructNames 在一个包里把路由上的 handler 方法解析成请求结构体名。
//
// 注册与解码常常不在同一个文件里（iam/server/http.go 注册，orgs.go 解码），
// 所以这一步按包做，不按文件做。第一版在文件内解析，于是三个目标端点的
// 字段集全是空的，而闸门对空集合直接跳过——它当时是绿的，而且绿给了一个
// 它根本没检查到的东西。
//
// 按方法名在包里找定义，而不是拿注册处的接收者变量名去比对接收者类型名：
// r.Post("/v1/orgs", h.createOrg) 里的 h 是变量，createOrg 的接收者是
// *Handler，两者本来就不相等。名字在包里唯一时用它；不唯一时不猜，返回空
// 并让第二道闸门显式报出"没读到这个端点的请求结构"，而不是安静地跳过。
func resolveStructNames(dir, pkg string, regs []registration) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return
	}
	pkgAST := pkgs[pkg]
	if pkgAST == nil {
		return
	}
	for i := range regs {
		var found []string
		for _, file := range pkgAST.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Name.Name != regs[i].method {
					continue
				}
				if got := scanDecode(fn); got != "" {
					found = append(found, got)
				}
			}
		}
		if len(found) == 1 {
			regs[i].structName = found[0]
		}
	}
}

// scanDecode 读一个方法体，返回它解码的请求结构体名。
//
// 方法体里同时出现变量名与类型名，而遍历顺序是声明在前、调用在后：
// var in createUserReq 先被看到，decode(r, &in) 后被看到。所以两个来源要
// 分开收齐再对上——边走边覆盖会返回 "in"，一个不是类型的名字，于是字段集
// 读成空集，第二道闸门判成"读不出请求结构"。第一版正是这样绿的。
func scanDecode(fn *ast.FuncDecl) string {
	type decl struct {
		varName, typeName string
	}
	var decls []decl
	var decoded []string

	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Decode" &&
				len(node.Args) == 1 {
				if name, ok := unaryIdentName(node.Args[0]); ok {
					decoded = append(decoded, name)
				}
			}
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "decode" && len(node.Args) == 2 {
				if name, ok := unaryIdentName(node.Args[1]); ok {
					decoded = append(decoded, name)
				}
			}
		case *ast.ValueSpec:
			id, ok := node.Type.(*ast.Ident)
			if !ok {
				return true
			}
			for _, name := range node.Names {
				decls = append(decls, decl{varName: name.Name, typeName: id.Name})
			}
		}
		return true
	})

	for _, name := range decoded {
		for _, d := range decls {
			if d.varName == name {
				return d.typeName
			}
		}
	}
	// 只有一个局部结构体声明时，来源就是它——解码那一步可能写在别的包
	// 的辅助函数里（如 svc.RuleCondition），但请求体本身在本地声明。
	if len(decls) == 1 {
		return decls[0].typeName
	}
	return ""
}

func unaryIdentName(expr ast.Expr) (string, bool) {
	u, ok := expr.(*ast.UnaryExpr)
	if !ok {
		return "", false
	}
	id, ok := u.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// TestEveryMigrationTargetRouteIsRegistered 是闸门的第一问。
func TestEveryMigrationTargetRouteIsRegistered(t *testing.T) {
	tree := readTree(t)
	if len(tree.routes) == 0 {
		t.Fatal("no route found in the tree; this test would pass for the wrong reason")
	}
	for _, et := range AllEntityTypes() {
		meta := GetEntityMeta(et)
		if !meta.IsImportable() {
			continue
		}
		if _, ok := tree.routes[meta.TargetRoute]; !ok {
			t.Errorf("%s 的 TargetRoute=%q 在 manager 里没有注册。"+
				"这条实体的导入会在每一行上记一条 404，而 404 读起来像数据错误。"+
				"要么改指到一个真实端点，要么把它标成 TargetMissing。", et, meta.TargetRoute)
		}
	}
}

// TestEveryMappedFieldIsAcceptedByTheEndpoint 是闸门的第二问，也是接上第一问
// 之后立刻暴露的那一层：端点找对了，字段依然可能对不上。
func TestEveryMappedFieldIsAcceptedByTheEndpoint(t *testing.T) {
	tree := readTree(t)
	for _, et := range AllEntityTypes() {
		meta := GetEntityMeta(et)
		if !meta.IsImportable() {
			continue
		}
		accepted, ok := tree.routes[meta.TargetRoute]
		if !ok || len(accepted) == 0 {
			t.Errorf("%s 写向 %s，但本工具读不出那个端点解码的请求结构，"+
				"所以无法核对 FieldMap。端点存在不等于字段对得上——把解析补上，"+
				"或者先确认这个端点的请求体确实接受这些字段。", et, meta.TargetRoute)
			continue
		}
		targets := make([]string, 0, len(meta.FieldMap))
		for _, to := range meta.FieldMap {
			targets = append(targets, to)
		}
		sort.Strings(targets)
		var bad []string
		for _, to := range targets {
			if !accepted[to] {
				bad = append(bad, to)
			}
		}
		if len(bad) == 0 {
			continue
		}
		t.Errorf("%s 写向 %s（%s），但 FieldMap 里的 %s 不在该端点解码的请求结构中。"+
			"这个工具会为每一行记一条 400，而 400 读起来像源数据不合法。"+
			"接受这些字段的只有：%s。",
			et, meta.TargetRoute, tree.verbs[meta.TargetRoute],
			strings.Join(bad, ", "), strings.Join(sortedKeys(accepted), ", "))
	}
}

// tenant_id 是本客户端自己注入的，不属于源字段映射，所以豁免。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheIdempotencyReadRouteExists 是决策 292 补上的第三问。
//
// import 的幂等判断打的是 GET /api/v1/{entity}/by-source-id/{id}，而这条路由
// 在 manager 里不存在——于是对着真实 opskeeper 跑，EntityExists 永远返回
// false，重复导入会重复创建。集成测试抓不到，因为它的 mock 实现了这条路由。
//
// 这里只要求要么路由在、要么注册表写下了为什么，不替谁去实现那个读端点：
// 按来源 ID 查询要不要开放、开放给谁，是一次接口决定。
func TestTheIdempotencyReadRouteExists(t *testing.T) {
	tree := readTree(t)
	for _, et := range AllEntityTypes() {
		meta := GetEntityMeta(et)
		if !meta.IsImportable() {
			continue
		}
		readRoute := meta.TargetRoute + "/by-source-id/{id}"
		if _, registered := tree.routes[readRoute]; registered {
			continue
		}
		if strings.TrimSpace(meta.IdempotencyNote) == "" {
			t.Errorf("%s 的幂等查询端点 %s 在 manager 里没有注册，"+
				"所以重复导入会重复创建——而注册表没有写下这一点。"+
				"要么实现这个读端点，要么在 IdempotencyNote 里说清楚。", et, readRoute)
		}
	}
}

// TestAnEntityWithNoRouteSaysWhy 是闸门的另一半：把端点拿掉必须同时给出
// 一个理由，否则下一个读代码的人只会看到一张空白的表。
func TestAnEntityWithNoRouteSaysWhy(t *testing.T) {
	for _, et := range AllEntityTypes() {
		meta := GetEntityMeta(et)
		if meta.IsImportable() {
			continue
		}
		if strings.TrimSpace(meta.TargetMissing) == "" {
			t.Errorf("%s 没有 TargetRoute 也没有说明原因，读者只会以为这是一次遗漏。", et)
		}
	}
}

// TestImportRefusesBeforeWritingAnything 是这条性质的行为面：一次导入必须
// 在写第一行之前就说清楚，而不是把 404 摊成 N 条逐行失败。
func TestImportRefusesBeforeWritingAnything(t *testing.T) {
	snap := NewSnapshot("src", "", nil)
	snap.PutEntity(EntityPGConnections, []map[string]any{
		{"id": 1, "project_id": 42, "name": "prod-pg"},
	})
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := snap.WriteTo(path); err != nil {
		t.Fatal(err)
	}

	// Target 指向一个不存在的地址：真跑起来每行都会拿到连接错误或 404。
	_, err := Import(t.Context(), ImportOptions{
		Snapshot:      path,
		Target:        "http://127.0.0.1:1",
		TenantMapping: "42=1",
		RatePerSec:    1000,
	})
	if err == nil {
		t.Fatal("Import accepted an entity whose target endpoint does not exist")
	}
	if !strings.Contains(err.Error(), string(EntityPGConnections)) {
		t.Errorf("the refusal does not name the entity: %v", err)
	}
	if !strings.Contains(err.Error(), "middleware_resources") {
		t.Errorf("the refusal does not name the missing target: %v", err)
	}
}
