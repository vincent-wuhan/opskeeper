// Package chatdiagnose — inmemory_conversation_repo.go
//
// Day 6+: in-memory ConversationRepo fallback for dev/test
// environments without a live database. Production wire-up uses
// data/chatdiagnose/store.NewConversationRepoDB(*gorm.DB).
//
// Kept in the biz layer (not data/) so dev/test wiring doesn't need
// to construct a DB connection. Mirrors loop.inmemory_repos semantics
// (sync.Mutex protected; tests use freely under -race).
package chatdiagnose

import (
	"context"
	"errors"
	"sync"
	"time"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// InMemoryConversationRepo 是 chatdiagnose.ConversationRepo 的内存
// fallback 实现。
//
// 线程安全：sync.Mutex 保护所有 mutating 方法。
//
// 多租户：GetConversation 不强制 tenant 归属校验（dev/test 简化）；
// 跨租户防护由生产 SQL 实现（data/chatdiagnose/store）提供。
type InMemoryConversationRepo struct {
	mu            sync.Mutex
	conversations map[string]*chatdiagnosemodel.Conversation
	turns         map[string][]*chatdiagnosemodel.Turn
	nextTurnID    int64
}

// NewInMemoryConversationRepo 构造 InMemoryConversationRepo。
func NewInMemoryConversationRepo() *InMemoryConversationRepo {
	return &InMemoryConversationRepo{
		conversations: make(map[string]*chatdiagnosemodel.Conversation),
		turns:         make(map[string][]*chatdiagnosemodel.Turn),
	}
}

// CreateConversation 实现 ConversationRepo。
func (r *InMemoryConversationRepo) CreateConversation(_ context.Context, c *chatdiagnosemodel.Conversation) error {
	if c == nil {
		return errors.New("chatdiagnose in-memory repo: nil conversation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conversations[c.ID]; ok {
		return errors.New("chatdiagnose in-memory repo: conversation already exists")
	}
	cp := *c
	cp.CreatedAt = time.Now().UTC()
	cp.UpdatedAt = cp.CreatedAt
	r.conversations[c.ID] = &cp
	return nil
}

// GetConversation 实现 ConversationRepo。
func (r *InMemoryConversationRepo) GetConversation(_ context.Context, id string) (*chatdiagnosemodel.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conversations[id]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

// SaveTurn 实现 ConversationRepo。append-only：忽略 ID，重设 seq。
func (r *InMemoryConversationRepo) SaveTurn(_ context.Context, t *chatdiagnosemodel.Turn) error {
	if t == nil {
		return errors.New("chatdiagnose in-memory repo: nil turn")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextTurnID++
	cp := *t
	cp.ID = r.nextTurnID
	cp.CreatedAt = time.Now().UTC()
	r.turns[t.ConversationID] = append(r.turns[t.ConversationID], &cp)
	t.ID = cp.ID
	return nil
}

// GetTurns 实现 ConversationRepo。
func (r *InMemoryConversationRepo) GetTurns(_ context.Context, conversationID string) ([]*chatdiagnosemodel.Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	src := r.turns[conversationID]
	out := make([]*chatdiagnosemodel.Turn, 0, len(src))
	for _, t := range src {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

// UpdateConversationTitle 实现 ConversationRepo。
func (r *InMemoryConversationRepo) UpdateConversationTitle(_ context.Context, id, title string, updatedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conversations[id]
	if !ok {
		return errors.New("chatdiagnose in-memory repo: conversation not found")
	}
	c.Title = title
	c.UpdatedAt = updatedAt
	return nil
}

// SetTurnLinkedLoopEvent 实现 ConversationRepo。
func (r *InMemoryConversationRepo) SetTurnLinkedLoopEvent(_ context.Context, turnID, loopEventID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, turns := range r.turns {
		for _, t := range turns {
			if t.ID == turnID {
				t.LinkedLoopEventID = &loopEventID
				return nil
			}
		}
	}
	return errors.New("chatdiagnose in-memory repo: turn not found")
}
