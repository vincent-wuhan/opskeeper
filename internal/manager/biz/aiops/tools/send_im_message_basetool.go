package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
)

// ToolNameSendIMMessage is the wire name.
const ToolNameSendIMMessage = "send_im_message"

// IMChannel is one configured outbound channel (设置→渠道), narrowed to what
// the tool needs to resolve + report it.
type IMChannel struct {
	ID   uint64
	Name string
	Kind string
}

// IMSender is the seam to the channel store + notify router. Implemented in
// cmd/main.go over the alert channel repo + notify.Router (same
// BuildSenderFromChannel path the alert notifier / flow notify node use), so
// this package stays decoupled from the data layer.
type IMSender interface {
	ListIMChannels(ctx context.Context) ([]IMChannel, error)
	SendIM(ctx context.Context, channelID uint64, title, text string) error
}

// SendIMMessageTool lets the assistant proactively push a message to a
// configured IM channel (Feishu / DingTalk / Slack / Telegram / WeCom).
type SendIMMessageTool struct {
	sender IMSender
	log    *slog.Logger
}

// NewSendIMMessageTool builds the tool.
func NewSendIMMessageTool(s IMSender, log *slog.Logger) *SendIMMessageTool {
	if log == nil {
		log = slog.Default()
	}
	return &SendIMMessageTool{sender: s, log: log}
}

var sendIMMessageSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "channel": { "type": "string", "description": "目标渠道名——设置→渠道里配好的飞书 / 钉钉 / Slack / Telegram 等渠道的名字。" },
    "text": { "type": "string", "description": "要发送的正文（纯文本，可带换行）。" },
    "title": { "type": "string", "description": "可选标题 / 主题。" }
  },
  "required": ["channel", "text"]
}`)

const sendIMMessageWhenToUse = "用户要把某个结论 / 通知主动发到飞书、钉钉等群里时用（比如\"把这段诊断发到运维群\"）。" +
	"channel 传渠道名（设置→渠道里配的）。不确定有哪些渠道时，先随便填一个调一次——报错里会列出所有可用渠道名，再据此重发。"

// Info — Class=write: it sends a real message (side-effecting, viewers can't
// use it) but it is not destructive.
func (t *SendIMMessageTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameSendIMMessage,
		Description: "Send a message to a configured IM channel (Feishu / DingTalk / Slack / Telegram / WeCom). Pass the channel NAME from 设置→渠道.",
		WhenToUse:   sendIMMessageWhenToUse,
		Parameters:  sendIMMessageSchema,
		Class:       "write",
	}, nil
}

type sendIMMessageArgs struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Title   string `json:"title"`
}

// InvokableRun resolves the channel by name (case-insensitive) and sends.
// A miss returns the available channel names so the LLM can self-correct.
func (t *SendIMMessageTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.sender == nil {
		return "", fmt.Errorf("send_im_message: channels not wired")
	}
	var in sendIMMessageArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("send_im_message: bad args: %w", err)
	}
	in.Channel = strings.TrimSpace(in.Channel)
	if in.Channel == "" || strings.TrimSpace(in.Text) == "" {
		return "", fmt.Errorf("send_im_message: channel and text are required")
	}
	chans, err := t.sender.ListIMChannels(ctx)
	if err != nil {
		return "", fmt.Errorf("send_im_message: list channels: %w", err)
	}
	var target *IMChannel
	for i := range chans {
		if strings.EqualFold(chans[i].Name, in.Channel) {
			target = &chans[i]
			break
		}
	}
	if target == nil {
		names := make([]string, 0, len(chans))
		for _, c := range chans {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			return "", fmt.Errorf("send_im_message: no channels configured. Add one under 设置→渠道 first")
		}
		return "", fmt.Errorf("send_im_message: channel %q not found. Available channels: %s", in.Channel, strings.Join(names, ", "))
	}
	if err := t.sender.SendIM(ctx, target.ID, in.Title, in.Text); err != nil {
		return "", fmt.Errorf("send_im_message: send to %q: %w", target.Name, err)
	}
	t.log.Info("send_im_message: sent", slog.String("channel", target.Name), slog.String("kind", target.Kind))
	out, _ := json.Marshal(map[string]any{"sent": true, "channel": target.Name, "kind": target.Kind})
	return string(out), nil
}
