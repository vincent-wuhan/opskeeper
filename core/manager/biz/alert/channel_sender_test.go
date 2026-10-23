package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

func chWithConfig(name, typ string, cfg map[string]string) *model.Channel {
	b, _ := json.Marshal(cfg)
	return &model.Channel{Name: name, ChannelType: typ, ConfigJSON: string(b)}
}

func TestBuildSenderFromChannel(t *testing.T) {
	types := []string{
		model.ChannelTypeSlack, model.ChannelTypeFeishu, model.ChannelTypeDingTalk,
		model.ChannelTypeWeCom, model.ChannelTypeTelegram, model.ChannelTypeWebhook,
	}
	for _, typ := range types {
		ch := chWithConfig(typ+"-1", typ, map[string]string{"endpoint": "https://example.com/x", "secret": "s"})
		s, err := BuildSenderFromChannel(ch)
		if err != nil {
			t.Fatalf("%s: unexpected err %v", typ, err)
		}
		if s == nil || s.Name() != typ+"-1" {
			t.Errorf("%s: bad sender %v", typ, s)
		}
	}

	// Empty type falls back to the generic webhook sender.
	if _, err := BuildSenderFromChannel(chWithConfig("x", "", map[string]string{"endpoint": "https://x"})); err != nil {
		t.Errorf("empty type: %v", err)
	}
	// Legacy "url" key still resolves an endpoint.
	if _, err := BuildSenderFromChannel(chWithConfig("l", "webhook", map[string]string{"url": "https://x"})); err != nil {
		t.Errorf("legacy url key: %v", err)
	}
	// No endpoint → error (this is what channelHasDestination also guards).
	if _, err := BuildSenderFromChannel(chWithConfig("none", "slack", map[string]string{})); err == nil {
		t.Error("expected error for missing endpoint")
	}
	// Unknown type → error.
	if _, err := BuildSenderFromChannel(chWithConfig("m", "mystery", map[string]string{"endpoint": "https://x"})); err == nil {
		t.Error("expected error for unknown channel type")
	}
}

// channelHasDestination must read the "endpoint" key the encoder actually
// writes (the prior bug checked "url" only, so every persisted channel was
// skipped).
func TestChannelHasDestination_EndpointKey(t *testing.T) {
	if !channelHasDestination(chWithConfig("a", "slack", map[string]string{"endpoint": "https://x"})) {
		t.Error("endpoint key should count as a destination")
	}
	if channelHasDestination(chWithConfig("b", "slack", map[string]string{})) {
		t.Error("empty config should not be a destination")
	}
}

// ── S-4 集成：钉钉非零 errcode 在存量路径落 failed 并被 RetryWorker 重试 ──
//
// 这条测试钉住 issue 的验收句：「钉钉 errcode != 0 在存量路径落
// notification_deliveries.failed 并被 RetryWorker 重试」。
//
// 它走的是生产路径本身——持久化的渠道行 → BuildSenderFromChannel 造出的
// 渠道 sender → 渠道自己的 HTTP 接口 → MaybeNotify 落账 → RetryWorker
// 重试。alert 包自带的 fakeNotifier 停在 Notifier 接缝上，会把 S-4 要修的
// 那一段（渠道在 HTTP 200 里回一个非零错误码）整个挡在测试之外，所以这里
// 用一个把投递接到真实 sender 上的替身。

// channelNotifier 把投递接到渠道自己的 sender 上：Send 按名字取回持久化
// 渠道并用它的 ChannelType + ConfigJSON 造 sender，SendVia 直接用调用方
// 已经造好的 sender。两条路都真正发一次 HTTP。
type channelNotifier struct {
	repo Repo
}

func (n *channelNotifier) Send(ctx context.Context, msg notify.Message, channels ...string) error {
	for _, name := range channels {
		ch, err := n.repo.GetChannelByName(ctx, name)
		if err != nil {
			return err
		}
		sender, err := BuildSenderFromChannel(ch)
		if err != nil {
			return err
		}
		if err := sender.Send(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

func (n *channelNotifier) SendVia(ctx context.Context, msg notify.Message, sender notify.Sender) error {
	return sender.Send(ctx, msg)
}

// replyBox 是一个可改写的渠道回包，用来在同一条测试里先让渠道拒绝、
// 再让它接受——重试能否恢复，取决于第二次的渠道回包。
type replyBox struct {
	mu   sync.Mutex
	body string
}

func (b *replyBox) set(body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.body = body
}

func (b *replyBox) get() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.body
}

func TestDingTalkNonZeroErrcodeFailsTheDeliveryAndIsRetried(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	box := &replyBox{body: `{"errcode":310000,"errmsg":"sign not match"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(box.get()))
	}))
	t.Cleanup(srv.Close)

	repo := newFakeRepo()
	incident := &model.Incident{
		Title:        "edge 7 disk_full",
		ScopeType:    model.RuleScopeHost,
		Rule:         "disk_full",
		Severity:     "critical",
		Status:       model.IncidentStatusOpen,
		DedupeKey:    "host:7:disk_full",
		Summary:      "disk full",
		LastFiredAt:  now.Add(-10 * time.Minute),
		FirstFiredAt: now.Add(-10 * time.Minute),
	}
	if err := repo.CreateIncident(ctx, incident); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	repo.channels["dingtalk"] = &model.Channel{
		ID:          7,
		Name:        "dingtalk",
		ChannelType: model.ChannelTypeDingTalk,
		Enabled:     true,
		ConfigJSON:  `{"endpoint":"` + srv.URL + `","secret":"s"}`,
	}

	notifier := &channelNotifier{repo: repo}
	uc := NewUsecase(repo, nil)
	msg := notify.Message{
		Subject:    "disk full",
		Severity:   notify.SeverityCritical,
		Source:     model.RuleScopeHost,
		DedupeKey:  incident.DedupeKey,
		OccurredAt: now.Add(-5 * time.Minute),
	}
	uc.MaybeNotify(ctx, &FiringResult{Incident: incident}, msg, NotifyOpts{
		Notifier:        notifier,
		DefaultChannels: []string{"dingtalk"},
	})

	// 渠道回的是 HTTP 200 + 非零 errcode：投递必须记成 failed。
	if len(repo.deliveries) != 1 {
		t.Fatalf("delivery rows = %d; want 1", len(repo.deliveries))
	}
	d := repo.deliveries[0]
	if d.Status != model.DeliveryStatusFailed {
		t.Fatalf("delivery status = %q; want %q", d.Status, model.DeliveryStatusFailed)
	}
	if d.ErrorMessage == nil || !strings.Contains(*d.ErrorMessage, "310000") {
		t.Fatalf("delivery error = %v; want the channel's error code", d.ErrorMessage)
	}
	if !hasEventType(repo.events, model.EventTypeNotificationFailed) {
		t.Fatal("a notification_failed event must be written for a rejected delivery")
	}
	if hasEventType(repo.events, model.EventTypeNotificationSent) {
		t.Fatal("a rejected delivery must not write a notification_sent event")
	}

	// 渠道恢复正常后，RetryWorker 重试同一条投递并把它变成成功。
	box.set(`{"errcode":0,"errmsg":"ok"}`)
	worker := NewRetryWorker(RetryWorkerOpts{
		Repo:              repo,
		Notifier:          notifier,
		Usecase:           uc,
		MaxAttempts:       5,
		BackoffPerAttempt: time.Minute,
		Now:               func() time.Time { return now },
	})
	worker.RunOnce(ctx)

	d = repo.deliveries[0]
	if d.Status != model.DeliveryStatusSuccess {
		t.Fatalf("delivery status after retry = %q; want %q", d.Status, model.DeliveryStatusSuccess)
	}
	if d.AttemptCount != 2 {
		t.Fatalf("attempt_count after retry = %d; want 2", d.AttemptCount)
	}
	if !hasEventType(repo.events, model.EventTypeNotificationSent) {
		t.Fatal("a successful retry must write a notification_sent event")
	}
	if _, ok := repo.notifiedCalls[incident.ID]; !ok {
		t.Fatal("a successful retry must mark the incident notified")
	}
}
