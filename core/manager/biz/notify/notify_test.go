package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	// notify is the shared base package: the channel registry this
	// package reads through the kernel. The package under test is the
	// manager-side glue; the two are different packages that happen to
	// share a name.
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
)

// The six built-in channel types, in the order the kernel registry lists
// them. Each case carries the signing scheme its channel uses, because the
// two signed channels stamp a timestamp into the request and a byte-for-byte
// comparison of two requests sent a microsecond apart is not a statement
// about the implementation — it is a statement about the clock.
const (
	signingNone     = ""
	signingFeishu   = "feishu"
	signingDingTalk = "dingtalk"
)

type channelCase struct {
	typ     string
	signing string
	spec    notify.ChannelSpec
}

// sixCases returns one real spec per built-in channel type, pointed at the
// given capture endpoint.
func sixCases(endpoint string) []channelCase {
	return []channelCase{
		{
			typ:     NameWebhook,
			signing: signingNone,
			spec:    notify.ChannelSpec{Type: NameWebhook, Name: "ops-webhook", URL: endpoint, Secret: "webhook-secret"},
		},
		{
			typ:     NameSlack,
			signing: signingNone,
			spec:    notify.ChannelSpec{Type: NameSlack, Name: "ops-slack", URL: endpoint},
		},
		{
			typ:     NameFeishu,
			signing: signingFeishu,
			spec:    notify.ChannelSpec{Type: NameFeishu, Name: "ops-feishu", URL: endpoint, Secret: "feishu-secret"},
		},
		{
			typ:     NameDingTalk,
			signing: signingDingTalk,
			spec:    notify.ChannelSpec{Type: NameDingTalk, Name: "ops-dingtalk", URL: endpoint, Secret: "dingtalk-secret"},
		},
		{
			typ:     NameWeCom,
			signing: signingNone,
			spec:    notify.ChannelSpec{Type: NameWeCom, Name: "ops-wecom", URL: endpoint},
		},
		{
			typ:     NameTelegram,
			signing: signingNone,
			spec:    notify.ChannelSpec{Type: NameTelegram, Name: "ops-telegram", URL: endpoint, ChatID: "chat-4242"},
		},
	}
}

// alertMessage is the message every channel is asked to deliver. Every field
// is fixed: a message carrying time.Now() would make the comparison a
// statement about the clock rather than about the two resolution paths.
func alertMessage() notify.Message {
	return notify.Message{
		Subject:    "node cpu high",
		Body:       "cpu at 92% for 5m",
		Severity:   notify.SeverityCritical,
		Source:     "prometheus",
		DedupeKey:  "rule:cpu_high|host:node-1",
		Labels:     map[string]string{"rule": "cpu_high", "incident_id": "42", "device_id": "7"},
		OccurredAt: time.Unix(1750000000, 0).UTC(),
	}
}

// TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel pins I2: the
// six built-in channels behave the same whether they are reached through the
// legacy channel registry or through the kernel.
//
// The two paths are compared at the wire, not at the factory: a sender that
// builds the same object but posts a different body is a different channel.
// Every channel is asked to deliver the same message to the same capture
// endpoint, and the two requests must agree on the sender name, the HTTP
// method, the path, the body bytes and the signing material.
//
// The two signed channels are compared with their clock-dependent fields
// normalized and each signature recomputed against its own timestamp — the
// scheme is the one the G4 e2e case already pins, reused rather than
// reinvented.
func TestTheSixBuiltinChannelsResolveIdenticallyThroughTheKernel(t *testing.T) {
	srv := newCaptureServer(t)
	cases := sixCases(srv.URL)

	// The legacy path: no view installed, so BuildChannel reads the
	// pre-migration registry map.
	legacy := make(map[string]notify.Sender, len(cases))
	for _, tc := range cases {
		sender, err := notify.BuildChannel(tc.spec, nil)
		if err != nil {
			t.Fatalf("legacy build of %s: %v", tc.typ, err)
		}
		legacy[tc.typ] = sender
	}

	// The kernel path: the six built-ins go through the same admission gate
	// every other plugin goes through, and the view they resolve into is
	// installed for the base package to read.
	host := kernel.NewHost(kernel.NewRegistry(), Policy(), slog.New(slog.DiscardHandler))
	set, err := Wire(host, Builtins(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() { notify.InstallChannelResolver(nil) })

	msg := alertMessage()
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			kernelSender, err := notify.BuildChannel(tc.spec, nil)
			if err != nil {
				t.Fatalf("kernel build of %s: %v", tc.typ, err)
			}
			if got, want := kernelSender.Name(), legacy[tc.typ].Name(); got != want {
				t.Errorf("sender name = %q, want %q", got, want)
			}

			before := srv.count()
			if err := legacy[tc.typ].Send(context.Background(), msg); err != nil {
				t.Fatalf("legacy send of %s: %v", tc.typ, err)
			}
			if err := kernelSender.Send(context.Background(), msg); err != nil {
				t.Fatalf("kernel send of %s: %v", tc.typ, err)
			}
			oldReq, newReq := srv.at(before), srv.at(before+1)

			if oldReq.Method != newReq.Method {
				t.Errorf("HTTP method = %q, want %q", newReq.Method, oldReq.Method)
			}
			if oldReq.Path != newReq.Path {
				t.Errorf("path = %q, want %q", newReq.Path, oldReq.Path)
			}

			switch tc.signing {
			case signingFeishu:
				assertFeishuRequestsMatch(t, tc, oldReq, newReq)
			case signingDingTalk:
				assertDingTalkRequestsMatch(t, tc, oldReq, newReq)
			default:
				if string(oldReq.Body) != string(newReq.Body) {
					t.Errorf("body bytes differ:\n legacy = %s\n kernel = %s", oldReq.Body, newReq.Body)
				}
			}

			assertSignatureHeadersMatch(t, tc, oldReq, newReq)
		})
	}

	// The default deployment holds all six types, each owned by its
	// first-party package.
	if got, want := len(set.Types()), len(cases); got != want {
		t.Fatalf("set holds %d types, want %d: %v", got, want, set.Types())
	}
	for _, tc := range cases {
		holder, ok := set.Holder(tc.typ)
		if !ok {
			t.Errorf("set does not hold %s", tc.typ)
			continue
		}
		if want := "opskeeper-notifier-" + tc.typ; holder != want {
			t.Errorf("holder of %s = %q, want %q", tc.typ, holder, want)
		}
	}
}

// assertFeishuRequestsMatch compares two Feishu requests with the two
// clock-dependent body fields normalized, and checks that the signature each
// request carries is the one its own timestamp implies.
func assertFeishuRequestsMatch(t *testing.T, tc channelCase, oldReq, newReq capturedRequest) {
	t.Helper()

	oldBody, oldTS, oldSign := feishuBody(t, oldReq.Body)
	newBody, newTS, newSign := feishuBody(t, newReq.Body)
	if oldTS == "" || newTS == "" {
		t.Fatal("feishu body carries no top-level timestamp")
	}
	if string(oldBody) != string(newBody) {
		t.Errorf("feishu body differs outside timestamp/sign:\n legacy = %s\n kernel = %s", oldBody, newBody)
	}
	if want := feishuSign(newTS, tc.spec.Secret); newSign != want {
		t.Errorf("feishu sign = %q, want %q (timestamp=%q)", newSign, want, newTS)
	}
	if oldSign == "" {
		t.Error("legacy feishu request carries no signature")
	}
}

// assertDingTalkRequestsMatch compares two DingTalk requests. The signature
// rides the URL, so the query is compared with the two signing parameters
// removed, and the signature is recomputed against its own timestamp.
func assertDingTalkRequestsMatch(t *testing.T, tc channelCase, oldReq, newReq capturedRequest) {
	t.Helper()

	if string(oldReq.Body) != string(newReq.Body) {
		t.Errorf("dingtalk body bytes differ:\n legacy = %s\n kernel = %s", oldReq.Body, newReq.Body)
	}

	oldQuery, oldTS, oldSign := dingTalkQuery(t, oldReq.RawQuery)
	newQuery, newTS, newSign := dingTalkQuery(t, newReq.RawQuery)
	if oldTS == "" || newTS == "" {
		t.Fatal("dingtalk query carries no timestamp")
	}
	if oldQuery != newQuery {
		t.Errorf("dingtalk query differs outside timestamp/sign:\n legacy = %q\n kernel = %q", oldQuery, newQuery)
	}
	if want := dingTalkSign(newTS, tc.spec.Secret); newSign != want {
		t.Errorf("dingtalk sign = %q, want %q (timestamp=%q)", newSign, want, newTS)
	}
	if oldSign == "" {
		t.Error("legacy dingtalk request carries no signature")
	}
}

// assertSignatureHeadersMatch compares the signature headers of two
// requests. The generic webhook signs the body with a header; the two chat
// channels sign in the body or the query and carry no signing header.
func assertSignatureHeadersMatch(t *testing.T, tc channelCase, oldReq, newReq capturedRequest) {
	t.Helper()

	const header = "X-OpsKeeper-Signature"
	oldSig := oldReq.Header.Get(header)
	newSig := newReq.Header.Get(header)
	if oldSig != newSig {
		t.Errorf("%s = %q, want %q", header, newSig, oldSig)
	}
	if oldSig == "" {
		return
	}
	want := "sha256=" + hex.EncodeToString(hmacSHA256(tc.spec.Secret, newReq.Body))
	if newSig != want {
		t.Errorf("%s = %q, want %q", header, newSig, want)
	}
}

// feishuBody returns the body with its two signing fields removed, plus the
// timestamp and the signature it carried.
func feishuBody(t *testing.T, body []byte) ([]byte, string, string) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("feishu body is not a JSON object: %v", err)
	}
	ts := stringField(t, payload, "timestamp")
	sign := stringField(t, payload, "sign")
	delete(payload, "timestamp")
	delete(payload, "sign")
	canonical, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("re-marshal feishu body: %v", err)
	}
	return canonical, ts, sign
}

// dingTalkQuery returns the query without its two signing parameters, plus
// the timestamp and the signature it carried.
func dingTalkQuery(t *testing.T, raw string) (string, string, string) {
	t.Helper()
	vals, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("dingtalk query %q: %v", raw, err)
	}
	ts := vals.Get("timestamp")
	sign := vals.Get("sign")
	vals.Del("timestamp")
	vals.Del("sign")
	return vals.Encode(), ts, sign
}

func stringField(t *testing.T, payload map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := payload[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q is not a string: %v", key, err)
	}
	return s
}

// feishuSign mirrors core/base/pkg/notify/webhook.go signFeishu: HMAC-SHA256
// with the key "<timestamp>\n<secret>" over an empty message, base64-std.
// This is the scheme the G4 e2e case already pins.
func feishuSign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// dingTalkSign mirrors core/base/pkg/notify/webhook.go signDingTalkURL:
// HMAC-SHA256 keyed by the secret over "<timestamp>\n<secret>", base64-std.
func dingTalkSign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func hmacSHA256(key string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return mac.Sum(nil)
}

// TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds
// pins the semantics this slice exists for: a deployment that ships its own
// slack channel gets its own slack channel.
//
// The third-party package holds the type first, so the first-party package
// stands aside — an Info, not a failure — and the other five built-ins
// activate normally. What the deployment ends up with is the variant.
func TestAThirdPartyVariantOwningSlackWinsOverTheBuiltinAndBootStillSucceeds(t *testing.T) {
	const variantPlugin = "opskeeper-notifier-slack-blocks"

	handler := &captureHandler{}
	log := slog.New(handler)

	host := kernel.NewHost(kernel.NewRegistry(), Policy(), log)
	if err := host.Activate(context.Background(), manifestFor(variantPlugin, NameSlack), variantContribution{plugin: variantPlugin}); err != nil {
		t.Fatalf("activate the third-party variant: %v", err)
	}

	// The built-in implementation, built before the view is installed, so
	// the test can tell the two apart afterwards.
	legacySlack, err := notify.BuildChannel(notify.ChannelSpec{Type: NameSlack, Name: "ops-slack", URL: "http://127.0.0.1:1/hook"}, nil)
	if err != nil {
		t.Fatalf("legacy build of slack: %v", err)
	}

	set, err := Wire(host, Builtins(), log)
	if err != nil {
		t.Fatalf("wire must not fail when a third-party variant holds a channel type: %v", err)
	}
	t.Cleanup(func() { notify.InstallChannelResolver(nil) })

	// The variant holds slack; the five other built-ins are ours.
	holder, ok := set.Holder(NameSlack)
	if !ok {
		t.Fatal("the set does not hold slack at all")
	}
	if holder != variantPlugin {
		t.Errorf("holder of slack = %q, want %q", holder, variantPlugin)
	}
	for _, typ := range []string{NameWebhook, NameFeishu, NameDingTalk, NameWeCom, NameTelegram} {
		holder, ok := set.Holder(typ)
		if !ok {
			t.Errorf("the set does not hold %s", typ)
			continue
		}
		if want := "opskeeper-notifier-" + typ; holder != want {
			t.Errorf("holder of %s = %q, want %q", typ, holder, want)
		}
	}

	// What answers a slack spec is the variant, not the built-in.
	resolved, err := notify.BuildChannel(notify.ChannelSpec{Type: NameSlack, Name: "ops-slack", URL: "http://127.0.0.1:1/hook"}, nil)
	if err != nil {
		t.Fatalf("build slack through the view: %v", err)
	}
	if got, want := resolved.Name(), variantSenderName; got != want {
		t.Errorf("sender name = %q, want %q (the third-party variant)", got, want)
	}
	if legacySlack.Name() == resolved.Name() {
		t.Error("the legacy registry and the kernel view answer slack identically; the test cannot tell the variant apart")
	}

	// The standing aside is an Info naming both sides, and the only Warn
	// is the kernel's own rollback of the duplicate registration.
	stood := handler.recordsWithMessage("notification channel plugin stood aside")
	if len(stood) != 1 {
		t.Fatalf("got %d standing-aside records, want 1: %+v", len(stood), handler.records)
	}
	if got, want := stood[0].Attrs["plugin"], PluginSlack; got != want {
		t.Errorf("standing-aside plugin = %v, want %v", got, want)
	}
	if got, want := stood[0].Attrs["holder"], variantPlugin; got != want {
		t.Errorf("standing-aside holder = %v, want %v", got, want)
	}
	if stood[0].Level < slog.LevelInfo {
		t.Errorf("standing-aside level = %v, want at least Info", stood[0].Level)
	}
	for _, rec := range handler.records {
		if rec.Level < slog.LevelWarn {
			continue
		}
		if rec.Message != "plugin activation failed; rolled back" {
			t.Errorf("unexpected record at %v: %s", rec.Level, rec.Message)
		}
	}
}

// variantSenderName is the name the fake third-party slack implementation
// gives its senders, so a test can tell it from the built-in one.
const variantSenderName = "slack-variant"

// variantContribution is a stand-in for a third-party package that
// implements the slack channel type itself.
type variantContribution struct {
	plugin string
}

func (c variantContribution) PluginName() string { return c.plugin }

func (c variantContribution) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindNotifier, NameSlack, func(spec notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
		return notify.NewGenericWebhookSender(variantSenderName, spec.URL, spec.Secret, nil), nil
	})
}

// TestWireLogsExactlyTheWhitelistedRecords pins the startup log contract:
// the default deployment adds exactly seven records to the boot log — the
// kernel's six "plugin activated" and one summary line — and no warning or
// anything above it.
//
// A default deployment that logged a warning would look like a deployment
// with a problem, and an operator would go looking for one.
func TestWireLogsExactlyTheWhitelistedRecords(t *testing.T) {
	handler := &captureHandler{}
	log := slog.New(handler)

	host := kernel.NewHost(kernel.NewRegistry(), Policy(), log)
	set, err := Wire(host, Builtins(), log)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() { notify.InstallChannelResolver(nil) })

	activated := handler.recordsWithMessage("plugin activated")
	if len(activated) != 6 {
		t.Errorf("got %d kernel activation records, want 6: %+v", len(activated), handler.records)
	}
	summary := handler.recordsWithMessage("notification channels activated")
	if len(summary) != 1 {
		t.Fatalf("got %d summary records, want 1: %+v", len(summary), handler.records)
	}
	if len(handler.records) != 7 {
		t.Errorf("the default deployment logged %d records, want exactly 7: %+v", len(handler.records), handler.records)
	}
	for _, rec := range handler.records {
		if rec.Level < slog.LevelWarn {
			continue
		}
		t.Errorf("unexpected record at %v or above: %s %v", rec.Level, rec.Message, rec.Attrs)
	}

	// The summary names every type and the package that holds it, so an
	// operator can read the notifier ownership off one line.
	attrs := summary[0].Attrs
	types, ok := attrs["types"].([]string)
	if !ok {
		t.Fatalf("summary carries no types attribute: %v", attrs)
	}
	if len(types) != 6 {
		t.Errorf("summary names %d types, want 6: %v", len(types), types)
	}
	holders, ok := attrs["holders"].(map[string]string)
	if !ok {
		t.Fatalf("summary carries no holders attribute: %v", attrs)
	}
	for _, typ := range types {
		if want := "opskeeper-notifier-" + typ; holders[typ] != want {
			t.Errorf("summary holder of %s = %q, want %q", typ, holders[typ], want)
		}
	}

	// The set the view answers with is the same six types.
	if len(set.Types()) != 6 {
		t.Errorf("the set holds %d types, want 6: %v", len(set.Types()), set.Types())
	}
}

// capturedRequest is one request the capture endpoint received.
type capturedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// captureServer is an httptest server that records what it received. It is
// the endpoint both resolution paths post to, so the comparison is of the
// bytes on the wire rather than of two in-memory objects.
type captureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedRequest
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		cs.mu.Lock()
		cs.requests = append(cs.requests, capturedRequest{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     body,
		})
		cs.mu.Unlock()
		// The chat channels judge the reply body, and a body saying
		// "errcode 0" is what a real bot endpoint answers.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"code":0,"msg":"ok","errmsg":"ok"}`))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.requests)
}

func (cs *captureServer) at(i int) capturedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.requests[i]
}

// logRecord is one captured log record.
type logRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// captureHandler records every log record it is handed.
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, logRecord{Level: r.Level, Message: r.Message, Attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// recordsWithMessage returns the captured records carrying the given
// message, in the order they were logged.
func (h *captureHandler) recordsWithMessage(msg string) []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logRecord
	for _, rec := range h.records {
		if rec.Message == msg {
			out = append(out, rec)
		}
	}
	return out
}
