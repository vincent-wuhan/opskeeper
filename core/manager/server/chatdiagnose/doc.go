// Package chatdiagnose exposes manager HTTP handlers for the
// conversational-diagnosis entry point (D13-D16 of
// zero-manual-ops-loop).
//
// Endpoints (spec §conversational-diagnosis §HTTP API):
//
//	POST /api/v1/chat/diagnose
//	POST /api/v1/chat/conversations/{id}/promote
//	POST /api/v1/chat/conversations/{id}/reports
//
// All three are admin-or-tenant-scoped; auth/middleware is wired at
// mount time in cmd/opskeeper/main.go.
package chatdiagnose
