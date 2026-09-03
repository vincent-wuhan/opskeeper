package imbridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/agent"
)

// Sender is the platform-agnostic outbound surface. The provider
// implementation (Feishu / DingTalk) wraps its own client to satisfy
// it. The bridge layer never imports a specific provider package;
// HTTP handlers construct the right Sender (with the app's
// credentials) and pass it into HandleInbound.
type Sender interface {
	// SendText creates a new text message in the target chat and
	// returns the platform's message id. The bridge stores the id
	// so progressive edits can update the same message.
	SendText(ctx context.Context, receiveID, receiveIDType, text string) (messageID string, err error)
	// EditText replaces the body of a previously-sent text message.
	EditText(ctx context.Context, messageID, text string) error
}

// streamEditor turns the SSE-shape agent.Event stream into throttled
// EditText calls on the platform. It assumes the assistant text grows
// monotonically — we always send the full accumulated buffer, never
// deltas, because Feishu / DingTalk both replace rather than append on
// edit.
type streamEditor struct {
	ctx           context.Context
	sender        Sender
	chatID        string
	receiveIDType string
	messageID     string // initially the placeholder id; "" if placeholder send failed
	locale        string // app.DefaultLocale, drives the OnFatal apology language
	log           *slog.Logger

	mu     sync.Mutex
	buf    string
	lastAt time.Time
	// throttle: every 800ms or 200 char delta
}

const (
	editIntervalMs   = 800
	editCharsTrigger = 200
)

func newStreamEditor(ctx context.Context, sender Sender, chatID, receiveIDType, placeholderMessageID, locale string, log *slog.Logger) *streamEditor {
	return &streamEditor{
		ctx:           ctx,
		sender:        sender,
		chatID:        chatID,
		receiveIDType: receiveIDType,
		messageID:     placeholderMessageID,
		locale:        locale,
		log:           log,
	}
}

// OnEvent is wired as the emit callback for agent.RunStreamWithOpts.
// We only care about EventAssistant (text chunks) and EventDone
// (terminal — force-flush). Tool calls / task notifications are
// suppressed in IM for now; they'd be too noisy as inline chat
// messages.
func (e *streamEditor) OnEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventAssistant:
		// Pull the text content out of the event. agent.Event for
		// assistant carries the persisted assistant turn — we
		// concatenate its Content field. If the runtime later
		// exposes a per-chunk delta we should switch to that to
		// avoid quadratic edit traffic.
		text := assistantText(ev)
		if text == "" {
			return
		}
		e.mu.Lock()
		e.buf = text
		shouldFlush := e.shouldFlushLocked()
		e.mu.Unlock()
		if shouldFlush {
			e.flush()
		}
	case agent.EventDone:
		e.flush() // terminal: ensure last chunk lands
	default:
		// EventToolStart / EventToolEnd / EventTaskNotification —
		// surfaced in the web UI's per-turn timeline; in IM they'd
		// fragment the message and confuse users. Drop for now.
	}
}

// OnFatal is called by the bridge when the agent run aborted — we
// replace the placeholder with the error so the IM user gets a clear
// signal instead of a stale "思考中…".
func (e *streamEditor) OnFatal(err error) error {
	prefix := "⚠ 助手执行失败："
	if strings.ToLower(strings.TrimSpace(e.locale)) == "en" {
		prefix = "⚠ Assistant failed: "
	}
	e.mu.Lock()
	e.buf = prefix + err.Error()
	e.mu.Unlock()
	return e.flush()
}

// Flush is the bridge's final post-run nudge — ensures the last buf
// has been written even if the agent terminated without emitting Done.
func (e *streamEditor) Flush() error { return e.flush() }

func (e *streamEditor) shouldFlushLocked() bool {
	if time.Since(e.lastAt) >= editIntervalMs*time.Millisecond {
		return true
	}
	return false
}

func (e *streamEditor) flush() error {
	e.mu.Lock()
	buf := e.buf
	mid := e.messageID
	e.lastAt = time.Now()
	e.mu.Unlock()
	if buf == "" {
		return nil
	}
	if mid == "" {
		// Placeholder failed earlier — do a one-shot send now.
		newID, err := e.sender.SendText(e.ctx, e.chatID, e.receiveIDType, buf)
		if err != nil {
			e.log.Warn("imbridge: fallback send failed", slog.Any("err", err))
			return err
		}
		e.mu.Lock()
		e.messageID = newID
		e.mu.Unlock()
		return nil
	}
	if err := e.sender.EditText(e.ctx, mid, buf); err != nil {
		e.log.Warn("imbridge: edit failed", slog.String("message_id", mid), slog.Any("err", err))
		return err
	}
	return nil
}

// assistantText pulls the persisted assistant text out of an
// EventAssistant payload. The agent runtime stores the full
// accumulated assistant turn (not per-token deltas) on the event so we
// can just take it.
func assistantText(ev agent.Event) string {
	// agent.Event.Assistant is set on assistant events — see
	// internal/manager/biz/aiops/agent/agent.go. We avoid a direct
	// type assertion on the field shape to stay forward-compatible:
	// if the runtime adds richer payloads (citations, attachments)
	// they live on the same Event struct.
	if ev.Assistant == nil {
		return ""
	}
	return ev.Assistant.Content
}
