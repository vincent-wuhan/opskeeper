// Package schema 加载并校验 Harness 黄金事故 case 文件。
//
// 路径 A 阶段 1 任务 1.8：单元测试基础设施。
//
// Loader 把 YAML case 文件解析为 Case struct，并用 case.schema.json
// 对应的内联 Go 校验规则做验证。所有 case 在编译期通过 go test 验证。
//
// 注：完整 YAML 解析需要 yaml.v3 依赖。当前实现把 YAML 文本当作结构化
// 字符串校验（验证必需字段存在、ID 格式、enum 范围），完整字段解析留给
// runner 阶段（届时引入 yaml.v3）。
package schema

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Case 是 case.yaml 解析后的结构化表示。
type Case struct {
	ID            string
	Description   string
	Severity      string
	Tags          []string
	Prerequisites []string
	Inject        []InjectStep
	Expect        Expect
	Rubric        Rubric
	Metadata      map[string]interface{}
	caseFilePath  string
	raw           string
}

// InjectStep 是 inject 数组中的一个故障注入步骤。
type InjectStep struct {
	Type     string
	Duration string
	Params   map[string]interface{}
}

// Expect 是预期 Agent 响应。
type Expect struct {
	TimeToDetect       int
	TimeToRemediate    int
	RootCauseLines     []string
	RemediationOptions []string
}

// Rubric 是评分阈值。
//
// zero-manual-ops-loop Day 7 task 7.3 扩展：增加 ApprovalRate /
// RecoveryPassRate 两个指标以对齐 loop-harness-rubric spec delta +
// harness-eval-platform spec delta。原 RCAAccuracy / TimeToRemediate /
// NoCollateralDamage 字段保持不变（向后兼容 Day 1-5 case.yaml）。
type Rubric struct {
	RCAAccuracy        float64 // [0,1] RootCauseJSON.root_cause_object 与 case.Expect.RootCauseLines 的 match 率
	TimeToRemediate    int     // 从 detected → recovered 的 wall-clock 时长（秒）
	NoCollateralDamage bool    // 修复不应有副作用
	// Day 7 task 7.3: 新增四指标字段。缺值不报错，标记 rubric_incomplete=true。
	ApprovalRate     *float64 `json:"approval_rate,omitempty"`      // [0,1] 自动通过审批的 proposal 数 / 总 proposal 数
	RecoveryPassRate *float64 `json:"recovery_pass_rate,omitempty"` // [0,1] verify_recovery 一次通过的比例
	RubricIncomplete bool     `json:"rubric_incomplete,omitempty"`  // true 当四指标未全部填充
}

// Allowed severities and prerequisites (mirrors case.schema.json enum).
var (
	allowedSeverities = map[string]bool{
		"P0": true, "P1": true, "P2": true, "P3": true,
	}
	allowedPrerequisites = map[string]bool{
		"pg.cluster reachable":          true,
		"pg.bench dataset loaded":       true,
		"pg.replica_configured":         true,
		"redis.cluster reachable":       true,
		"redis.bench dataset loaded":    true,
		"rabbitmq.cluster reachable":    true,
		"kafka.cluster reachable":       true,
		"k8s.cluster reachable":         true,
		"k8s.test_namespace exists":     true,
		"host.test_user_ssh_accessible": true,
		"opskeeper.adapter registered":  true,
	}
	idPattern         = regexp.MustCompile(`^[a-z][a-z0-9-]+/[a-z][a-z0-9-]+$`)
	toolMethodPattern = regexp.MustCompile(`^[a-z][a-z0-9_*-]+\.[A-Za-z][A-Za-z0-9_]+$`)
	durationPattern   = regexp.MustCompile(`^[0-9]+(s|m|h)$`)
)

// SetApprovalRate / SetRecoveryPassRate / SetRubricIncomplete 是
// Day 7 task 7.3 新增的 setter；JSON loader 用它们在 case.yaml 解析
// 之后注入四指标。
func (r *Rubric) SetApprovalRate(v float64)     { r.ApprovalRate = &v }
func (r *Rubric) SetRecoveryPassRate(v float64) { r.RecoveryPassRate = &v }
func (r *Rubric) MarkRubricIncomplete()         { r.RubricIncomplete = true }

// HasAllFourMetrics reports whether the four-metric rubric is fully
// populated (used by leaderboard to mark NOT QUALIFIED).
func (r *Rubric) HasAllFourMetrics() bool {
	return r.ApprovalRate != nil && r.RecoveryPassRate != nil
}

// Loader 加载 case 文件并校验。
type Loader struct {
	casesDir string
}

// NewLoader 创建 Loader 实例。
func NewLoader(casesDir string) *Loader {
	return &Loader{casesDir: casesDir}
}

// LoadAll 加载目录下所有 case.yaml 并校验。
func (l *Loader) LoadAll() ([]*Case, error) {
	var cases []*Case
	err := filepath.Walk(l.casesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(path) != "case.yaml" {
			return nil
		}
		c, err := l.loadOne(path)
		if err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		cases = append(cases, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cases, nil
}

// LoadByID 加载指定 ID 的 case。
func (l *Loader) LoadByID(id string) (*Case, error) {
	all, err := l.LoadAll()
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, fmt.Errorf("case not found: %s", id)
}

// loadOne 解析单个 case.yaml 文件并校验。
func (l *Loader) loadOne(path string) (*Case, error) {
	yamlBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	raw := string(yamlBytes)
	c := &Case{caseFilePath: path, raw: raw}
	if err := c.parseAndValidate(); err != nil {
		return nil, err
	}
	return c, nil
}

// FilePath 返回 case.yaml 的源文件路径。
func (c *Case) FilePath() string {
	return c.caseFilePath
}

// parseAndValidate 从 raw 文本解析必需字段并校验。
//
// 完整 YAML 解析留给 runner 阶段（届时引入 yaml.v3）。当前实现聚焦在
// 编译期必做的 schema 校验（id 格式 / severity / 必需字段存在 / tool method 格式）。
func (c *Case) parseAndValidate() error {
	if id := extractTopLevelScalar(c.raw, "id"); id != "" {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("invalid id format: %q (expected: <type>/<symptom>)", id)
		}
		c.ID = id
	} else {
		return errors.New("missing required field: id")
	}

	if desc := extractTopLevelScalar(c.raw, "description"); desc != "" {
		if len(desc) < 10 {
			return fmt.Errorf("description too short (min 10 chars): %q", desc)
		}
		c.Description = desc
	} else {
		return errors.New("missing required field: description")
	}

	if sev := extractTopLevelScalar(c.raw, "severity"); sev != "" {
		if !allowedSeverities[sev] {
			return fmt.Errorf("invalid severity: %q (allowed: P0/P1/P2/P3)", sev)
		}
		c.Severity = sev
	} else {
		return errors.New("missing required field: severity")
	}

	// prerequisites
	c.Prerequisites = extractList(c.raw, "prerequisites")
	for _, p := range c.Prerequisites {
		if !allowedPrerequisites[p] {
			return fmt.Errorf("invalid prerequisite: %q", p)
		}
	}
	if len(c.Prerequisites) == 0 {
		return errors.New("prerequisites list is empty")
	}

	// inject 步骤数至少 1。type / duration / params 一起解析——这三个
	// 字段的去向是 injector.FromSchemaStep，而它消费全部三个。
	c.Inject = parseInjectSteps(c.raw)
	if len(c.Inject) == 0 {
		return errors.New("inject must have at least one step")
	}
	for i, step := range c.Inject {
		if step.Type == "" {
			return fmt.Errorf("inject step %d has no type", i)
		}
	}

	// expect.root_cause_lines / remediation_options 格式校验
	rootCause := extractList(c.raw, "root_cause_lines")
	for _, r := range rootCause {
		if !toolMethodPattern.MatchString(r) {
			return fmt.Errorf("invalid root_cause_lines entry: %q (expected <type>.<method>)", r)
		}
	}
	remediations := extractList(c.raw, "remediation_options")
	for _, r := range remediations {
		if !toolMethodPattern.MatchString(r) {
			return fmt.Errorf("invalid remediation_options entry: %q", r)
		}
	}
	c.Expect.RootCauseLines = rootCause
	c.Expect.RemediationOptions = remediations

	// expect.time_to_detect / time_to_remediate 是两个基线时长，缺了它们
	// time_efficiency 会对每个 case 都直接给满分，评测就再也测不出"发现
	// 太慢"这件事。所以缺失即报错，而不是当作 0 静默通过。
	for _, f := range []struct {
		key string
		dst *int
	}{
		{"time_to_detect", &c.Expect.TimeToDetect},
		{"time_to_remediate", &c.Expect.TimeToRemediate},
	} {
		v := extractScalar(c.raw, f.key)
		if v == "" {
			return fmt.Errorf("expect.%s is required", f.key)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid expect.%s: %q (expected a positive number of seconds)", f.key, v)
		}
		*f.dst = n
	}

	if len(c.Expect.RootCauseLines) == 0 {
		return errors.New("expect.root_cause_lines is required and must be non-empty")
	}
	if len(c.Expect.RemediationOptions) == 0 {
		return errors.New("expect.remediation_options is required and must be non-empty")
	}

	// rubric.rca_accuracy 必须在 0-1
	if acc := extractTopLevelScalar(c.raw, "rca_accuracy"); acc != "" {
		var f float64
		if _, err := fmt.Sscanf(acc, "%f", &f); err != nil {
			return fmt.Errorf("invalid rca_accuracy: %q", acc)
		}
		if f < 0 || f > 1 {
			return fmt.Errorf("rca_accuracy out of range [0,1]: %v", f)
		}
		c.Rubric.RCAAccuracy = f
	}

	// rubric.no_collateral_damage 直接决定 judge 的 collateral_safety 维度
	// 是否会因为 errors 非空而归零。此前它从未被解析，恒为 false，于是这个
	// 维度对所有 case 都稳定给满分——一次带 errors 的破坏性修复与一次完美
	// 的修复得分相同。
	if v := extractScalar(c.raw, "no_collateral_damage"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid rubric.no_collateral_damage: %q (expected true or false)", v)
		}
		c.Rubric.NoCollateralDamage = b
	}

	return nil
}

// extractTopLevelScalar 提取顶级 scalar 字段值（仅支持简单字符串/数字）。
func extractTopLevelScalar(raw, key string) string {
	// 匹配 key: value 直到行尾
	prefix := key + ":"
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimSpace(trimmed[len(prefix):])
		// 去掉引号
		rest = strings.Trim(rest, `"'`)
		// 去掉行内注释
		if idx := strings.Index(rest, "#"); idx >= 0 {
			rest = strings.TrimSpace(rest[:idx])
		}
		if rest == "" || rest == "|" || rest == ">" {
			continue
		}
		return rest
	}
	return ""
}

// indentOf 返回行首缩进的宽度（制表符按 1 计）。
func indentOf(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return i
		}
	}
	return len(line)
}

// extractScalar 提取任意缩进层级的标量键。case 文件里这些键只出现在
// expect: 之下，顶级键与它们不重名，所以按 key 前缀在全文查找是安全的。
func extractScalar(raw, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimSpace(trimmed[len(prefix):])
		if idx := strings.Index(rest, "#"); idx >= 0 {
			rest = strings.TrimSpace(rest[:idx])
		}
		return rest
	}
	return ""
}

// extractList 提取 list 字段值（- item 形式）。
//
// 列表在"缩进回落到 key 所在层级"时结束，而不是在遇到没有缩进的行时
// 结束。这两者的差别是实质的：case.yaml 里 root_cause_lines 与
// remediation_options 是同级的兄弟键，此前只在遇到顶级键时才断开，
// 于是 remediation_options 的条目被并进了 root_cause_lines。列表因此
// 多出期望里并不存在的项，matchRatio 的分母偏大，每个 run 的
// rca_accuracy 都被系统性压低——正确答案也会落在阈值之下。
func extractList(raw, key string) []string {
	prefix := key + ":"
	lines := strings.Split(raw, "\n")
	var result []string
	inList := false
	keyIndent := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inList {
			if strings.HasPrefix(trimmed, prefix) {
				inList = true
				keyIndent = indentOf(line)
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// 同级或更外层的键结束本列表
		if indentOf(line) <= keyIndent {
			break
		}
		// "- item" 格式
		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(trimmed[2:])
			item = strings.Trim(item, `"'`)
			if idx := strings.Index(item, "#"); idx >= 0 {
				item = strings.TrimSpace(item[:idx])
			}
			if item != "" {
				result = append(result, item)
			}
		}
	}
	return result
}

// parseInjectSteps 解析 inject 列表的每个 step：type、duration、params。
//
// 为什么不是只提 type（它此前做的就是这件事）：schema.InjectStep 是
// injector.FromSchemaStep 的输入，而后者消费三个字段。只填 type 的结果是
// Params 一直为 nil、Duration 一直为空串——corpus 里写下的 cores: 4 /
// table: orders / message_count: 100000 在加载时全部被丢掉，注入器拿到的是
// 一个空 map，而 FromSchemaStep 还会因为 time.ParseDuration("") 直接报错。
// 一条从来跑不通的装配路径与一条被静默削弱的注入路径，都不是"暂时够用"。
//
// 支持的语法范围与这个文件其余部分一致：一层键值对，值是标量、带引号的
// 字符串或行内列表（tables: [orders]）。更深的结构（params 之下再嵌 map）
// 不支持，也不猜——那一层被忽略，而不是被拼进上一层，因为把
// `limit_memory_mb` 和它所属的容器名混成同级，会造出一个从来不存在过的参数。
func parseInjectSteps(raw string) []InjectStep {
	block, ok := blockUnderKey(raw, "inject")
	if !ok {
		return nil
	}
	var steps [][]string
	for _, line := range block {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") {
			steps = append(steps, []string{line})
			continue
		}
		if len(steps) == 0 {
			continue
		}
		steps[len(steps)-1] = append(steps[len(steps)-1], line)
	}
	out := make([]InjectStep, 0, len(steps))
	for _, lines := range steps {
		out = append(out, parseInjectStep(lines))
	}
	return out
}

// blockUnderKey returns the lines nested under a top-level key, stopping when
// the indentation falls back to the key's own level.
//
// The stopping rule is the one extractList already learned the hard way: a
// sibling key at the same level ends the block, and a rule that only stopped
// on unindented lines would swallow the next key's contents into this one.
func blockUnderKey(raw, key string) ([]string, bool) {
	lines := strings.Split(raw, "\n")
	keyIndent := 0
	start := -1
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, key+":") {
			keyIndent = indentOf(line)
			start = index + 1
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	var block []string
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			block = append(block, line)
			continue
		}
		if indentOf(line) <= keyIndent {
			break
		}
		block = append(block, line)
	}
	return block, true
}

// parseInjectStep reads one "- type: ..." step and its continuation lines.
func parseInjectStep(lines []string) InjectStep {
	step := InjectStep{}
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		content := trimmed
		if strings.HasPrefix(content, "- ") {
			content = strings.TrimSpace(content[2:])
		}
		key, value, ok := splitYAMLPair(content)
		if !ok {
			continue
		}
		switch key {
		case "type":
			if step.Type == "" {
				step.Type = value
			}
		case "duration":
			if step.Duration == "" {
				step.Duration = value
			}
		case "params":
			step.Params = parseParamBlock(lines[index+1:], indentOf(line))
		}
	}
	return step
}

// parseParamBlock reads the flat key/value lines one level under "params:".
//
// Only the level directly under params is read. A deeper line is a structure
// this parser does not model, and skipping it is deliberate: attributing it to
// the level above would invent a parameter the case never declared.
func parseParamBlock(lines []string, paramsIndent int) map[string]interface{} {
	childIndent := -1
	out := make(map[string]interface{})
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := indentOf(line)
		if indent <= paramsIndent {
			break
		}
		if childIndent < 0 {
			childIndent = indent
		}
		if indent != childIndent {
			continue
		}
		key, value, ok := splitYAMLPair(trimmed)
		if !ok {
			continue
		}
		if strings.TrimSpace(value) == "" {
			// A key with no value on the line is the head of a nested
			// structure this parser does not model. Recording it as an empty
			// string would invent a parameter, and inventing one is worse
			// than not reporting it: a consumer reads the map as the case's
			// declared parameters.
			continue
		}
		out[key] = parseParamValue(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitYAMLPair splits "key: value" on the first colon that is not inside a
// quoted run, so `key: "session:active:user_42"` keeps its value intact.
func splitYAMLPair(line string) (string, string, bool) {
	var quote byte
	for index := 0; index < len(line); index++ {
		char := line[index]
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			quote = char
		case ':':
			key := strings.TrimSpace(line[:index])
			if key == "" {
				return "", "", false
			}
			return key, strings.TrimSpace(line[index+1:]), true
		}
	}
	return "", "", false
}

// parseParamValue reads a scalar, a quoted string, or an inline list.
//
// Numbers come back as int rather than float when they are whole, because a
// parameter like `sessions: 5` read back as "5" is a different string from the
// one the case file wrote for anything that compares it.
func parseParamValue(value string) interface{} {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		inner := strings.TrimSpace(value[1 : len(value)-1])
		if inner == "" {
			return []interface{}{}
		}
		items := strings.Split(inner, ",")
		out := make([]interface{}, 0, len(items))
		for _, item := range items {
			item = strings.Trim(strings.TrimSpace(item), `"'`)
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' || first == '\'') && last == first {
			return value[1 : len(value)-1]
		}
	}
	if index := strings.Index(value, " #"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	if number, err := strconv.Atoi(value); err == nil {
		return number
	}
	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return number
	}
	return value
}

// Suite 是一组 case 的集合。
type Suite struct {
	Name  string
	Cases []*Case
}

// LoadSuite 按 prefix 过滤加载 suite。
func (l *Loader) LoadSuite(prefix string) (*Suite, error) {
	all, err := l.LoadAll()
	if err != nil {
		return nil, err
	}
	suite := &Suite{Name: prefix}
	for _, c := range all {
		if strings.HasPrefix(c.ID, prefix) {
			suite.Cases = append(suite.Cases, c)
		}
	}
	if len(suite.Cases) == 0 {
		return nil, fmt.Errorf("no cases match prefix: %s", prefix)
	}
	return suite, nil
}

// Summary 输出 loader 概览（用于 opskeeper-eval list-cases）。
type Summary struct {
	TotalCases int
	BySeverity map[string]int
	ByResource map[string]int
}

// Summarize 汇总所有 case 的统计。
func (l *Loader) Summarize() (*Summary, error) {
	all, err := l.LoadAll()
	if err != nil {
		return nil, err
	}
	s := &Summary{
		TotalCases: len(all),
		BySeverity: make(map[string]int),
		ByResource: make(map[string]int),
	}
	for _, c := range all {
		s.BySeverity[c.Severity]++
		if idx := strings.Index(c.ID, "/"); idx >= 0 {
			s.ByResource[c.ID[:idx]]++
		}
	}
	return s, nil
}

// ErrNoCases 是 LoadSuite 无匹配时的标准错误。
var ErrNoCases = errors.New("no cases match")

// Score 是 judge 评分结果。
type Score struct {
	Overall          float64                `json:"overall"`
	RCAAccuracy      float64                `json:"rca_accuracy"`
	TimeToDetectMs   int64                  `json:"time_to_detect_ms"`
	TimeToRemediate  int64                  `json:"time_to_remediate_ms"`
	CollateralDamage int                    `json:"collateral_damage"`
	RubricCompliance map[string]float64     `json:"rubric_compliance"`
	Flagged          bool                   `json:"flagged"`
	Reason           string                 `json:"reason,omitempty"`
	Judges           map[string]JudgeResult `json:"judges,omitempty"`
}

// JudgeResult 是单个 judge 模型的评分结果。
type JudgeResult struct {
	Model     string  `json:"model"`
	Score     float64 `json:"score"`
	Reasoning string  `json:"reasoning,omitempty"`
	LatencyMs int64   `json:"latency_ms"`
}

// ToolCall 是 Agent 调用工具方法的记录。
type ToolCall struct {
	Tool       string                 `json:"tool"`
	Args       map[string]interface{} `json:"args,omitempty"`
	Result     map[string]interface{} `json:"result,omitempty"`
	DurationMs int64                  `json:"duration_ms"`
	ApprovedBy string                 `json:"approved_by,omitempty"` // 写操作时填
	Error      string                 `json:"error,omitempty"`
}
