package store

import (
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
)

// Migrate registers the aiops chat tables (sessions, messages, tool calls,
// mutating proposals, query templates) with gorm (sessions, messages, tool calls,
// mutating proposals) with gorm's AutoMigrate. CHECK constraints on Role /
// Status / Decision are carried in the model tags and reproduced on both
// MySQL and SQLite.
//
// chat_mutating_proposals (added PR-7 of / reviewer
// reality-check) is the audit source of truth for
// Migrate registers the aiops chat tables (sessions, messages, tool calls,
// mutating proposals, query templates) with gorm's AutoMigrate. CHECK
// constraints on Role / Status / Decision are carried in the model tags
// and reproduced on both MySQL and SQLite.
//
// chat_mutating_proposals (added PR-7 of / reviewer
// reality-check) is the audit source of truth for
// — every mutating tool_call leaves one row regardless of approve /
// reject. See model.MutatingProposal for the schema rationale.
//
// aiops_query_templates (c1-nl-query) caches successful NL→query
// translations so repeated questions skip the LLM round-trip. See
// model.QueryTemplate for the LRU thresholds and tenant-scoping.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&model.Session{},
		&model.Message{},
		&model.ToolCall{},
		&model.MutatingProposal{},
		&model.UserAgent{},
		&model.QueryTemplate{},
	)
}
