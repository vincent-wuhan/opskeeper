// Package label — dataguard Phase 1 业务层。
//
// 设计要点：
//   - LabelManager 统一封装创建 / 查询 / override
//   - ApplyHeuristic 把启发式结果落到 DB（含置信度阈值：≥0.85 自打、0.70-0.85
//     自打 + 通知 admin、<0.70 不入库）
//   - ResolveEffective 处理「子资源显式打标 vs 父资源继承」的优先级
//   - audit 由 Phase 3 接入 audit 表前先打日志
package label

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/heuristic"
	"github.com/vincent-wuhan/opskeeper/internal/dataguard/store"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// Repo 是 biz 视角的持久化接口（store 层实现）。
type Repo interface {
	Create(ctx context.Context, l *store.DataSensitivityLabel) error
	Get(ctx context.Context, resourceType, resourceID string) (*store.DataSensitivityLabel, error)
	List(ctx context.Context, sensitivity, source string, limit, offset int) ([]*store.DataSensitivityLabel, int64, error)
	ListByResourceType(ctx context.Context, resourceType, sensitivity string, limit, offset int) ([]*store.DataSensitivityLabel, error)
	Delete(ctx context.Context, resourceType, resourceID string) error
}

// ParentResolver 把子资源 ID 解析为父资源 ID 列表（用于继承）。
type ParentResolver interface {
	Parents(ctx context.Context, resourceType, resourceID string) ([]ParentRef, error)
}

// ParentRef 父资源引用。
type ParentRef struct {
	Type string
	ID   string
}

// LabelManager 是 dataguard 业务层对外服务。
type LabelManager struct {
	repo     Repo
	resolver ParentResolver
	engine   heuristic.Engine
	log      *slog.Logger
	now      func() time.Time
}

// NewLabelManager 构造 LabelManager。
func NewLabelManager(repo Repo, resolver ParentResolver, engine heuristic.Engine, log *slog.Logger) *LabelManager {
	if log == nil {
		log = slog.Default()
	}
	if engine == nil {
		engine = heuristic.NewCompositeEngine()
	}
	return &LabelManager{repo: repo, resolver: resolver, engine: engine, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入当前时间（测试）。
func (m *LabelManager) WithClock(now func() time.Time) *LabelManager {
	m.now = now
	return m
}

// ApplyHeuristic 触发自动打标。
//
// 置信度阈值（design §2.3）：
//   - ≥ 0.85：自打 + 写 audit
//   - 0.70-0.85：自打 + 通知 admin（Notes 含 "needs_admin_review"）
//   - < 0.70：返回 nil（不入库）
//
// 不覆盖人工打标（SourceManual / SourceOverride）。
func (m *LabelManager) ApplyHeuristic(ctx context.Context, res heuristic.Resource) (*store.DataSensitivityLabel, error) {
	if m.engine == nil {
		return nil, nil
	}
	match, ok := m.engine.Match(ctx, res)
	if !ok {
		return nil, nil
	}
	if match.Confidence < 0.70 {
		m.log.InfoContext(ctx, "dataguard: heuristic below threshold (pending)",
			"resource", string(res.Type)+":"+res.ID, "confidence", match.Confidence)
		return nil, nil
	}
	notes := match.Reason
	if match.Confidence < 0.85 {
		notes = match.Reason + " | needs_admin_review"
	}
	now := m.now()
	l := &store.DataSensitivityLabel{
		ResourceType: string(res.Type),
		ResourceID:   res.ID,
		Sensitivity:  string(match.Sensitivity),
		LabelSource:  string(store.SourceHeuristic),
		Confidence:   match.Confidence,
		LabeledBy:    "heuristic",
		Notes:        notes,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := m.repo.Create(ctx, l); err != nil {
		return nil, fmt.Errorf("dataguard: apply heuristic: %w", err)
	}
	m.audit(ctx, "heuristic_labeled", l)
	return l, nil
}

// CreateManual 人工打标入口（admin only — HTTP 层 enforce）。
func (m *LabelManager) CreateManual(ctx context.Context, l *store.DataSensitivityLabel, caller string) error {
	if l == nil {
		return errors.New("dataguard: nil label")
	}
	if _, err := dataguard.Parse(l.Sensitivity); err != nil {
		return fmt.Errorf("dataguard: invalid sensitivity: %w", err)
	}
	l.LabelSource = string(store.SourceManual)
	l.LabeledBy = caller
	if l.Confidence == 0 {
		l.Confidence = 1.0
	}
	now := m.now()
	l.CreatedAt = now
	l.UpdatedAt = now
	if err := m.repo.Create(ctx, l); err != nil {
		return fmt.Errorf("dataguard: create manual: %w", err)
	}
	m.audit(ctx, "manual_labeled", l)
	return nil
}

// UpdateOverride 显式 override（仅 admin；HTTP 层校验）。
func (m *LabelManager) UpdateOverride(ctx context.Context, resourceType, resourceID string, newSensitivity dataguard.Sensitivity, caller, reason string) (*store.DataSensitivityLabel, error) {
	if _, err := dataguard.Parse(string(newSensitivity)); err != nil {
		return nil, fmt.Errorf("dataguard: invalid sensitivity: %w", err)
	}
	old, _ := m.repo.Get(ctx, resourceType, resourceID)
	prev := ""
	if old != nil {
		prev = old.Sensitivity
	}
	now := m.now()
	l := &store.DataSensitivityLabel{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Sensitivity:  string(newSensitivity),
		LabelSource:  string(store.SourceOverride),
		Confidence:   1.0,
		LabeledBy:    caller,
		Notes:        fmt.Sprintf("override_of=%s | %s", prev, reason),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := m.repo.Create(ctx, l); err != nil {
		return nil, fmt.Errorf("dataguard: override: %w", err)
	}
	m.audit(ctx, "label_override", l, "prev_sensitivity="+prev)
	return l, nil
}

// Get 简单 wrapper。
func (m *LabelManager) Get(ctx context.Context, resourceType, resourceID string) (*store.DataSensitivityLabel, error) {
	return m.repo.Get(ctx, resourceType, resourceID)
}

// List 简单 wrapper。
func (m *LabelManager) List(ctx context.Context, sensitivity, source string, limit, offset int) ([]*store.DataSensitivityLabel, int64, error) {
	return m.repo.List(ctx, sensitivity, source, limit, offset)
}

// Delete 强制清理（admin only）。
func (m *LabelManager) Delete(ctx context.Context, resourceType, resourceID string) error {
	if err := m.repo.Delete(ctx, resourceType, resourceID); err != nil {
		return err
	}
	m.audit(ctx, "label_deleted", &store.DataSensitivityLabel{
		ResourceType: resourceType, ResourceID: resourceID,
	})
	return nil
}

// ResolveEffective 返回资源生效的 sensitivity（含继承）。
//
// 优先级：
//  1. 本资源 Source ∈ {manual, override} 直接用
//  2. 本资源 Source=heuristic 且 Confidence ≥ 0.85
//  3. 沿 ParentResolver 找祖先
//  4. 默认 Internal (0.50)
func (m *LabelManager) ResolveEffective(ctx context.Context, resourceType, resourceID string) (dataguard.Sensitivity, float64, bool, error) {
	l, err := m.repo.Get(ctx, resourceType, resourceID)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return "", 0, false, err
	}

	if l != nil {
		if l.LabelSource == string(store.SourceManual) || l.LabelSource == string(store.SourceOverride) {
			s, _ := dataguard.Parse(l.Sensitivity)
			return s, l.Confidence, false, nil
		}
		if l.LabelSource == string(store.SourceHeuristic) && l.Confidence >= 0.85 {
			s, _ := dataguard.Parse(l.Sensitivity)
			return s, l.Confidence, false, nil
		}
	}

	if m.resolver != nil {
		parents, err := m.resolver.Parents(ctx, resourceType, resourceID)
		if err == nil {
			for _, p := range parents {
				pl, err := m.repo.Get(ctx, p.Type, p.ID)
				if err != nil || pl == nil {
					continue
				}
				if pl.LabelSource == string(store.SourceManual) ||
					pl.LabelSource == string(store.SourceOverride) ||
					pl.LabelSource == string(store.SourceInherited) {
					s, _ := dataguard.Parse(pl.Sensitivity)
					return s, pl.Confidence, true, nil
				}
				if pl.LabelSource == string(store.SourceHeuristic) && pl.Confidence >= 0.85 {
					s, _ := dataguard.Parse(pl.Sensitivity)
					return s, pl.Confidence, true, nil
				}
			}
		}
	}

	return dataguard.Internal, 0.50, false, nil
}

// InheritFromParent 显式触发「子资源继承父资源」打标。
//
// 不覆盖人工打标（manual / override）。
func (m *LabelManager) InheritFromParent(ctx context.Context, parentType, parentID string, childType string, childIDs []string) (int, error) {
	parent, err := m.repo.Get(ctx, parentType, parentID)
	if err != nil {
		return 0, fmt.Errorf("dataguard: get parent for inherit: %w", err)
	}
	now := m.now()
	count := 0
	for _, cid := range childIDs {
		existing, _ := m.repo.Get(ctx, childType, cid)
		if existing != nil && (existing.LabelSource == string(store.SourceManual) || existing.LabelSource == string(store.SourceOverride)) {
			continue
		}
		l := &store.DataSensitivityLabel{
			ResourceType: childType,
			ResourceID:   cid,
			Sensitivity:  parent.Sensitivity,
			LabelSource:  string(store.SourceInherited),
			Confidence:   parent.Confidence,
			LabeledBy:    "inherit:" + parentType + ":" + parentID,
			Notes:        fmt.Sprintf("inherited_from=%s:%s", parentType, parentID),
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := m.repo.Create(ctx, l); err != nil {
			return count, err
		}
		count++
	}
	m.audit(ctx, "inherit_cascade", nil, fmt.Sprintf("parent=%s:%s children=%d", parentType, parentID, count))
	return count, nil
}

// audit 写一条结构化日志。
func (m *LabelManager) audit(ctx context.Context, action string, l *store.DataSensitivityLabel, extra ...string) {
	fields := []any{"action", action}
	if l != nil {
		fields = append(fields,
			"resource_type", l.ResourceType,
			"resource_id", l.ResourceID,
			"sensitivity", l.Sensitivity,
			"label_source", l.LabelSource,
			"confidence", l.Confidence,
			"labeled_by", l.LabeledBy,
		)
	}
	for _, e := range extra {
		fields = append(fields, "extra", e)
	}
	m.log.InfoContext(ctx, "dataguard audit", fields...)
}

// ParseJSONTags 解析 ComplianceTags JSON 字符串为列表。
func ParseJSONTags(raw string) ([]string, error) {
	var tags []string
	if raw == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// EncodeJSONTags 把 []string 序列化为 JSON 字符串。
func EncodeJSONTags(tags []string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ListByResourceType 列出指定资源类型的所有 label。
func (m *LabelManager) ListByResourceType(ctx context.Context, resourceType, sensitivity string, limit, offset int) ([]*store.DataSensitivityLabel, error) {
	return m.repo.ListByResourceType(ctx, resourceType, sensitivity, limit, offset)
}

// Repo 返回内部 repo（HTTP handler 用）。
func (m *LabelManager) Repo() Repo { return m.repo }
