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

	// inject 步骤数至少 1
	injectTypes := extractListItems(c.raw, "inject", "type")
	if len(injectTypes) == 0 {
		return errors.New("inject must have at least one step")
	}
	c.Inject = make([]InjectStep, len(injectTypes))
	for i, t := range injectTypes {
		c.Inject[i] = InjectStep{Type: t}
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

// extractList 提取顶级 list 字段值（- item 形式）。
func extractList(raw, key string) []string {
	prefix := key + ":"
	lines := strings.Split(raw, "\n")
	var result []string
	inList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inList {
			if strings.HasPrefix(trimmed, prefix) {
				inList = true
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// 顶级新字段结束
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.Contains(line, ":") {
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

// extractListItems 在 inject 列表下提取每个 step 的 type 字段。
//
// 输入 YAML 格式：
//
//	inject:
//	  - type: foo
//	    duration: 300s
//	  - type: bar
//
// 输出：["foo", "bar"]
func extractListItems(raw, listKey, itemKey string) []string {
	var result []string
	prefix := listKey + ":"
	lines := strings.Split(raw, "\n")
	inList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inList {
			if strings.HasPrefix(trimmed, prefix) {
				inList = true
			}
			continue
		}
		// 列表结束：遇到顶级（无缩进）的新字段
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && !strings.HasPrefix(trimmed, "- ") && strings.Contains(line, ":") {
			break
		}
		// "- type: foo" 形式
		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(trimmed[2:])
			// item 可能是 "type: foo" 或 "duration: 300s" 或 "params:" 等
			if strings.HasPrefix(item, itemKey+":") {
				v := strings.TrimSpace(item[len(itemKey)+1:])
				v = strings.Trim(v, `"'`)
				if v != "" {
					result = append(result, v)
				}
			}
		}
	}
	return result
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
