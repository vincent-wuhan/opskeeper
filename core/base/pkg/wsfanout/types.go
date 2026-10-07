package wsfanout

import (
	"encoding/json"
	"fmt"
	"time"
)

// Kind 区分 session 业务类型。注册表按 kind 过滤。
type Kind string

const (
	// KindAIOpsStream 是 AIOps chat 的 SSE 流。
	KindAIOpsStream Kind = "aiops_stream"
	// KindWebShell 是 WebShell 隧道会话。
	KindWebShell Kind = "webshell"
	// KindMarketplaceUpload 是 Marketplace 大文件上传（占位，本期不接）。
	KindMarketplaceUpload Kind = "marketplace_upload"
)

// ProtocolVersion 是 Message 协议版本。发送方写 1；接收方按 v 字段决定解码方式。
const ProtocolVersion = 1

// Action 是控制消息的语义类型。Handler 按 action 注册。
type Action string

const (
	// ActionStop 通知 owning pod 取消 AIOps SSE stream。
	ActionStop Action = "stop"
	// ActionKill 通知 owning pod 杀掉 WebShell 会话。
	ActionKill Action = "kill"
)

// Message 是跨副本控制消息的 payload。Version 固定 1；TS 由发送方填写。
type Message struct {
	Version   int       `json:"v"`
	Action    Action    `json:"action"`
	SessionID string    `json:"session_id"`
	Reason    string    `json:"reason,omitempty"`
	TS        time.Time `json:"ts"`
}

// SessionMeta 是注册表存储的元信息。Extra 透传业务自定义字段（如 user_id / device_id）。
type SessionMeta struct {
	PodID     string            `json:"pod_id"`
	Kind      Kind              `json:"kind"`
	StartedAt time.Time         `json:"started_at"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// SessionInfo 是 Scan/List 返回的活跃会话信息。
type SessionInfo struct {
	SessionID string    `json:"session_id"`
	PodID     string    `json:"pod_id"`
	Kind      Kind      `json:"kind"`
	StartedAt time.Time `json:"started_at"`
}

// ErrSessionOwned 表示 session 已被另一副本注册。
type ErrSessionOwned struct {
	Holder string
}

func (e *ErrSessionOwned) Error() string {
	return fmt.Sprintf("wsfanout: session already owned by pod=%s", e.Holder)
}

// IsErrSessionOwned 报告 err 是否为 ErrSessionOwned。
func IsErrSessionOwned(err error) bool {
	_, ok := err.(*ErrSessionOwned)
	return ok
}

// encodeMeta 把 SessionMeta 序列化成 map[string]string 写入 Redis Hash field。
// 设计：Hash 字段值统一字符串化，避免 value 是嵌套结构。
func encodeMeta(m SessionMeta) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeMeta 解析 EncodeMeta 写入的字符串。
func decodeMeta(s string) (SessionMeta, error) {
	var m SessionMeta
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return SessionMeta{}, err
	}
	return m, nil
}
