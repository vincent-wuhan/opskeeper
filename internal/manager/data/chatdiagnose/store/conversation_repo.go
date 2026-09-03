// Package store — conversation_repo.go
//
// *sql.DB-backed (GORM) 实现 chatdiagnose.ConversationRepo narrow
// interface，供 cmd/opskeeper/main.go 注入。
//
// 关键不变量：
//   - 多租户隔离：GetConversation 必须按 tenant 维度拒绝跨租户
//     访问；调用方传入不属于自己的 id 时返回 nil + nil（与
//     spec "跨租户 conversation_id 访问被拒" 对齐 —— 由 service
//     层在 chatdiagnose 包装 ConversationTenantMismatch）。
//   - turn append-only：SaveTurn 始终 INSERT；UpdateConversationTitle
//     只允许 title + updated_at 字段；SetTurnLinkedLoopEvent 只允许
//     linked_loop_event_id 字段（spec §"Append-only 契约例外"）。
//   - llm_context_snapshot：仅摘要 + sha 索引，不存完整 tool result
//     文本（spec §"LLM context snapshot 设计"）。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	chatdiagnose "github.com/vincent-wuhan/opskeeper/internal/manager/biz/chatdiagnose"
	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// ConversationRepoDB 是 chatdiagnose.ConversationRepo 的 GORM 实现。
type ConversationRepoDB struct {
	db *gorm.DB
}

// NewConversationRepoDB 构造。db 不得为 nil。
func NewConversationRepoDB(db *gorm.DB) *ConversationRepoDB {
	return &ConversationRepoDB{db: db}
}

// Compile-time interface satisfaction check。
var _ chatdiagnose.ConversationRepo = (*ConversationRepoDB)(nil)

// CreateConversation 写入新会话行。
func (r *ConversationRepoDB) CreateConversation(ctx context.Context, c *chatdiagnosemodel.Conversation) error {
	if c == nil {
		return errors.New("chatdiagnose repo: nil conversation")
	}
	if c.ID == "" {
		return errors.New("chatdiagnose repo: conversation ID required")
	}
	if c.TenantID == "" {
		return errors.New("chatdiagnose repo: tenant_id required")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.UpdatedAt = time.Now().UTC()
	if err := r.db.WithContext(ctx).Create(c).Error; err != nil {
		if isDuplicateKey(err) {
			// 重复创建当作冲突错误返回，让上层处理。
			return fmt.Errorf("chatdiagnose repo: conversation already exists: %w", err)
		}
		return fmt.Errorf("chatdiagnose repo: create: %w", err)
	}
	return nil
}

// GetConversation 按 id 查询。多租户隔离通过单独方法（GetConversationForTenant）
// 提供；这里只做按主键读，service 层负责租户归属校验（chatdiagnose
// spec §"跨租户 conversation_id 访问被拒"）。
func (r *ConversationRepoDB) GetConversation(ctx context.Context, id string) (*chatdiagnosemodel.Conversation, error) {
	if id == "" {
		return nil, errors.New("chatdiagnose repo: id required")
	}
	var row chatdiagnosemodel.Conversation
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("chatdiagnose repo: get: %w", err)
	}
	return &row, nil
}

// GetConversationForTenant 是 chatdiagnose spec §"跨租户 conversation_id
// 访问被拒" 配套入口：service 层在续接 conversation 时调用，
// 跨租户返回 ConversationTenantMismatch 错误让 HTTP 层映射 403。
func (r *ConversationRepoDB) GetConversationForTenant(ctx context.Context, id, tenantID string) (*chatdiagnosemodel.Conversation, error) {
	c, err := r.GetConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if c.TenantID != tenantID {
		return nil, chatdiagnose.ErrConversationTenantMismatch
	}
	return c, nil
}

// SaveTurn 持久化新 turn。append-only：忽略任何 ID 字段（autoIncrement
// 自填），即便调用方预填了 ID 也覆盖。
func (r *ConversationRepoDB) SaveTurn(ctx context.Context, t *chatdiagnosemodel.Turn) error {
	if t == nil {
		return errors.New("chatdiagnose repo: nil turn")
	}
	if t.ConversationID == "" {
		return errors.New("chatdiagnose repo: turn.ConversationID required")
	}
	if t.Role == "" {
		return errors.New("chatdiagnose repo: turn.Role required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	cp := *t
	cp.ID = 0 // append-only: never honor caller-supplied id
	if err := r.db.WithContext(ctx).Create(&cp).Error; err != nil {
		return fmt.Errorf("chatdiagnose repo: save turn: %w", err)
	}
	t.ID = cp.ID
	return nil
}

// GetTurns 返回某会话的所有 turn，按 seq ASC。
//
// 注意：当前 Turn schema 没有 seq 列（消息顺序由 id autoIncrement 隐式
// 给出）；按 id ASC 排序即可满足"按写入顺序"的语义。当未来 schema
// 升级加入显式 seq 列时，本方法加 .Order("seq ASC, id ASC") 即可。
func (r *ConversationRepoDB) GetTurns(ctx context.Context, conversationID string) ([]*chatdiagnosemodel.Turn, error) {
	if conversationID == "" {
		return nil, errors.New("chatdiagnose repo: conversationID required")
	}
	var rows []chatdiagnosemodel.Turn
	err := r.db.WithContext(ctx).
		Where("conversation_id = ?", conversationID).
		Order("id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose repo: get turns: %w", err)
	}
	out := make([]*chatdiagnosemodel.Turn, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return out, nil
}

// UpdateConversationTitle 只允许 patch title + updated_at 两个字段。
func (r *ConversationRepoDB) UpdateConversationTitle(ctx context.Context, id, title string, updatedAt time.Time) error {
	if id == "" {
		return errors.New("chatdiagnose repo: id required")
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	res := r.db.WithContext(ctx).Model(&chatdiagnosemodel.Conversation{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"title":      title,
			"updated_at": updatedAt,
		})
	if res.Error != nil {
		return fmt.Errorf("chatdiagnose repo: update title: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("chatdiagnose repo: conversation not found: %s", id)
	}
	return nil
}

// SetTurnLinkedLoopEvent 只 patch linked_loop_event_id 字段。
//
// spec §"Append-only 契约例外"：linked_loop_event_id 是唯一允许
// 在 INSERT 之后更新的字段（双向引用闭环）。
func (r *ConversationRepoDB) SetTurnLinkedLoopEvent(ctx context.Context, turnID, loopEventID int64) error {
	if turnID <= 0 {
		return errors.New("chatdiagnose repo: turnID required")
	}
	res := r.db.WithContext(ctx).Model(&chatdiagnosemodel.Turn{}).
		Where("id = ?", turnID).
		Update("linked_loop_event_id", loopEventID)
	if res.Error != nil {
		return fmt.Errorf("chatdiagnose repo: set linked loop event: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("chatdiagnose repo: turn not found: %d", turnID)
	}
	return nil
}

// SetTurnLinkedRootCause 配套 patch linked_root_cause_id 字段。
func (r *ConversationRepoDB) SetTurnLinkedRootCause(ctx context.Context, turnID, rootCauseID int64) error {
	if turnID <= 0 {
		return errors.New("chatdiagnose repo: turnID required")
	}
	res := r.db.WithContext(ctx).Model(&chatdiagnosemodel.Turn{}).
		Where("id = ?", turnID).
		Update("linked_root_cause_id", rootCauseID)
	if res.Error != nil {
		return fmt.Errorf("chatdiagnose repo: set linked root cause: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("chatdiagnose repo: turn not found: %d", turnID)
	}
	return nil
}

// CreateIncidentPattern 写入一条 KB pattern 行（KB-first 启用后由
// postmortem 完成回调触发）。
func (r *ConversationRepoDB) CreateIncidentPattern(ctx context.Context, p *chatdiagnosemodel.IncidentPattern) error {
	if p == nil {
		return errors.New("chatdiagnose repo: nil pattern")
	}
	if p.TenantID == "" {
		return errors.New("chatdiagnose repo: pattern tenant required")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = time.Now().UTC()
	if err := r.db.WithContext(ctx).Create(p).Error; err != nil {
		return fmt.Errorf("chatdiagnose repo: create pattern: %w", err)
	}
	return nil
}

// isDuplicateKey 与 loop/store/event_repo.go 同名函数行为一致
// （GORM ErrDuplicatedKey + 字符串 fallback）。本包独立维护以
// 避免 data 层跨域 import。
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return contains(msg, "Error 1062") || contains(msg, "UNIQUE constraint") || contains(msg, "duplicate key")
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
