package leaderboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 下面三样（Regression / Severity / 阈值）原本住在 leaderboard.go，
// 和一个纯内存的 Leaderboard 类型放在一起。那个类型 269 行、零生产调用方，
// 只被它自己的测试调用——一份被自己的测试证明正确的、没有任何人用的实现。
// 它在这里被删掉了，词汇留下：基线检测只有这一处定义。

// Regression 是回归检测结果。
type Regression struct {
	CaseID        string
	BaselineScore float64
	CurrentScore  float64
	DropPercent   float64  // 下降百分比（0-100）
	Severity      Severity // warn / block
	Message       string
	DetectedAt    time.Time
}

// Severity 是回归严重等级。
type Severity string

const (
	SeverityNone  Severity = "none"  // 无回归
	SeverityWarn  Severity = "warn"  // 5% ≤ drop < 15%：告警
	SeverityBlock Severity = "block" // drop ≥ 15%：阻断
)

// 阈值常量（与 build plan 验收对齐）
const (
	WarnThreshold  = 0.05 // 5% 下降告警
	BlockThreshold = 0.15 // 15% 下降阻断
)

// 这个文件是 2026-07 那次实现留下的洞的补法。
//
// Leaderboard 类型（SetBaseline / Baseline / CheckRegression / Blockers /
// Warns）写完就没有第二个调用方：它的全部状态在三个 map 里，进程一退就没了。
// 于是 `cmd/opskeeper-eval leaderboard` 只读 harness/result/loop 渲染一张看板，
// 基线与回归检测这两个能力在命令行上不存在——而 printUsage 里那一行写的是
// "leaderboard  显示排行榜 + 回归基线"。
//
// 补法不是给内存 map 加一个 Save，而是把基线变成一份**可以被 CI 提交进仓库
// 的文件**：harness 的其他产物（LoopResult JSON、Markdown 看板）本来就在磁盘上
// 当唯一事实源，基线混进内存是这条链上唯一一处例外。

// Baseline 是一次锁定时刻的全部分数。
//
// Metrics 记下这一次锁定用的是哪几个指标，因为"分数下降 5%"这句话只有在
// 知道分母是哪些指标时才有意义；下一次聚合口径改了却看不出，历史基线就成了
// 一个数字而不是一次记录。
type Baseline struct {
	LockedAt time.Time          `json:"locked_at"`
	LockedBy string             `json:"locked_by,omitempty"`
	Metrics  []string           `json:"metrics"`
	Scores   map[string]float64 `json:"scores"`
}

// boardMetric 是参与聚合的一个指标，以及它在一个 entry 上的读法。
type boardMetric struct {
	name  string
	value func(*LoopBoardEntry) *float64
}

// BoardMetrics 是看板聚合口径，顺序即报告里的展示顺序。
//
// 四个指标与 design doc §2.2 的 4-metric rubric 一致。time_to_remediate 不在
// 里：它越短越好，和其余四个"越大越好"的指标平均到一起没有意义。
var BoardMetrics = []boardMetric{
	{"rca_accuracy", func(e *LoopBoardEntry) *float64 { return e.RCAAccuracy }},
	{"approval_rate", func(e *LoopBoardEntry) *float64 { return e.ApprovalRate }},
	{"recovery_pass_rate", func(e *LoopBoardEntry) *float64 { return e.RecoveryPassRate }},
	{"kb_hit_rate", func(e *LoopBoardEntry) *float64 { return e.KBHitRate }},
}

// AggregateScore 把一个 entry 的已测指标取平均，得到用于比较基线的单一分数。
//
// 未测量的指标被跳过而不是当 0：一个只有 rca_accuracy 的 case 不该因为另外
// 三个没跑而看起来掉了一半。全部未测量时返回 ok=false，调用方必须把它记进
// Unmeasured 而不是记成 0 分回归。
func AggregateScore(e *LoopBoardEntry) (float64, bool) {
	sum := 0.0
	n := 0
	for _, m := range BoardMetrics {
		if v := m.value(e); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// LockBaseline 用当前看板的分数组出一份基线。
//
// unmeasured 里的 case 不会被写进基线：把一个没有分数的 case 锁成 0，下一次
// 任何非负分数都会被判成 100% 回归。
func LockBaseline(b *LoopBoard, by string) (*Baseline, []string) {
	out := &Baseline{
		LockedAt: time.Now().UTC(),
		LockedBy: by,
		Metrics:  metricNames(),
		Scores:   make(map[string]float64, len(b.Entries)),
	}
	var unmeasured []string
	for _, e := range b.Entries {
		score, ok := AggregateScore(e)
		if !ok {
			unmeasured = append(unmeasured, e.CaseID)
			continue
		}
		out.Scores[e.CaseID] = score
	}
	sort.Strings(unmeasured)
	return out, unmeasured
}

func metricNames() []string {
	out := make([]string, 0, len(BoardMetrics))
	for _, m := range BoardMetrics {
		out = append(out, m.name)
	}
	return out
}

// SaveBaseline 落盘。写临时文件再改名，rename 在同一目录内是原子的，
// 于是并发跑 CI 不会读到半个 JSON。
func SaveBaseline(path string, b *Baseline) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("baseline: marshal: %w", err)
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("baseline: mkdir %s: %w", dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("baseline: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("baseline: rename %s: %w", tmp, err)
	}
	return nil
}

// LoadBaseline 读基线。
//
// 文件不存在时返回的 error 说的是"基线尚未建立"而不是别的：调用方最可能的
// 误用是拿 --check-regression 去跑一个还没锁过基线的分支，此时正确的反应是
// 非零退出并告诉人去锁，而不是把它当成"零回归"。
func LoadBaseline(path string) (*Baseline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("baseline %s: not established yet — run `opskeeper-eval leaderboard --lock-baseline` first", path)
		}
		return nil, fmt.Errorf("baseline: read %s: %w", path, err)
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("baseline: parse %s: %w", path, err)
	}
	if len(b.Scores) == 0 {
		return nil, fmt.Errorf("baseline %s: holds no scores", path)
	}
	return &b, nil
}

// RegressionReport 是一次对照的完整结果。
//
// 四个分类是分开的，因为它们是四种不同的情况，而把它们合成一句"无回归"是
// 这条命令最容易犯的错：New 是一个从没锁过的新 case，Missing 是这次没跑到、
// 上次锁过的 case，Unmeasured 是跑了但一个指标都没测出来。三者都不是"没退步"。
type RegressionReport struct {
	Regressions []*Regression
	New         []string
	Missing     []string
	Unmeasured  []string
	Improved    []string
}

// CheckBoard 对照基线。
func CheckBoard(b *LoopBoard, base *Baseline) *RegressionReport {
	rep := &RegressionReport{}
	seen := make(map[string]bool, len(b.Entries))
	for _, e := range b.Entries {
		seen[e.CaseID] = true
		score, ok := AggregateScore(e)
		if !ok {
			rep.Unmeasured = append(rep.Unmeasured, e.CaseID)
			continue
		}
		baseScore, has := base.Scores[e.CaseID]
		if !has {
			rep.New = append(rep.New, e.CaseID)
			continue
		}
		r := CheckRegressionFor(base, e.CaseID, baseScore, score)
		switch {
		case r.Severity == SeverityNone && score > baseScore:
			rep.Improved = append(rep.Improved, e.CaseID)
		case r.Severity != SeverityNone:
			rep.Regressions = append(rep.Regressions, r)
		}
	}
	for caseID := range base.Scores {
		if !seen[caseID] {
			rep.Missing = append(rep.Missing, caseID)
		}
	}
	sort.Strings(rep.New)
	sort.Strings(rep.Missing)
	sort.Strings(rep.Unmeasured)
	sort.Strings(rep.Improved)
	sort.Slice(rep.Regressions, func(i, j int) bool {
		return rep.Regressions[i].DropPercent > rep.Regressions[j].DropPercent
	})
	return rep
}

// CheckRegressionFor 是不依赖实例的对照，CheckRegression（map 版）与
// CheckBoard 都走它。阈值与严重等级只有这一处定义。
func CheckRegressionFor(base *Baseline, caseID string, baselineScore, currentScore float64) *Regression {
	r := &Regression{
		CaseID:        caseID,
		BaselineScore: baselineScore,
		CurrentScore:  currentScore,
		DetectedAt:    time.Now().UTC(),
	}
	// 顺序是有意的：基线为 0 的判断必须在 "current >= baseline" 之前。
	// 一个 0.000 的基线让 drop% 无定义，而 current 0.5 >= baseline 0.0 看起来
	// 像一次提升——把无定义读成提升，是这条命令能给出的最贵的一个假阳性：
	// 它发生在有人把某个 case 的基线锁成 0 之后。
	if baselineScore <= 0 {
		r.Severity = SeverityBlock
		r.Message = fmt.Sprintf("baseline is %.3f, current %.3f: the drop is undefined, not absent", baselineScore, currentScore)
		return r
	}
	if currentScore >= baselineScore {
		r.Severity = SeverityNone
		r.Message = fmt.Sprintf("no regression (current %.3f >= baseline %.3f)", currentScore, baselineScore)
		return r
	}
	drop := (baselineScore - currentScore) / baselineScore
	r.DropPercent = drop * 100
	switch {
	case drop >= BlockThreshold:
		r.Severity = SeverityBlock
	case drop >= WarnThreshold:
		r.Severity = SeverityWarn
	default:
		r.Severity = SeverityNone
	}
	r.Message = fmt.Sprintf("%.1f%% drop (baseline %.3f → current %.3f)", drop*100, baselineScore, currentScore)
	return r
}

// Worst 是全部回归里最严重的一个等级，没有回归时是 SeverityNone。
func (rep *RegressionReport) Worst() Severity {
	worst := SeverityNone
	for _, r := range rep.Regressions {
		if r.Severity == SeverityBlock {
			return SeverityBlock
		}
		worst = SeverityWarn
	}
	return worst
}

// Unaccounted 是这次没被判定过的 case 数——新 case、消失的 case、没测出分数的
// case。它们都不构成回归，但"一个都没判定"必须能被一句话说出来。
func (rep *RegressionReport) Unaccounted() int {
	return len(rep.New) + len(rep.Missing) + len(rep.Unmeasured)
}
