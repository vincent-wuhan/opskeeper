package rabbitmq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

func TestInjector_Type(t *testing.T) {
	i := New(WithURL(""))
	if got := i.Type(); got != "rabbitmq." {
		t.Errorf("Type() = %q, want %q", got, "rabbitmq.")
	}
}

// 一个还没接线的注入器必须自报不可用。上一版这个测试断言 IsAvailable() == true，
// 断言的是一个"它能干活"的谎——而 Inject 那时只是往 map 里写了一行。
func TestInjector_CheckAvailableRefuses(t *testing.T) {
	// WithURL("") 显式给空值，而不是靠"环境变量没设"：后者在一台配了
	// OPSKEEPER_HARNESS_RABBITMQ_URL 的开发机上会变成一次真的注入，
	// 而消息删不掉。
	i := New(WithURL(""))
	err := i.CheckAvailable(context.Background())
	if err == nil {
		t.Fatal("CheckAvailable returned nil for an injector that touches no real system")
	}
	if !errors.Is(err, injector.ErrUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrUnavailable", err)
	}
	if len(err.Error()) < 40 {
		t.Fatalf("error = %q, want it to say what is missing", err)
	}
}

// 不可用就必须一步都不走。不在这里拦住，一次"注入成功"会一路走到 judge 那里，
// 变成一个假的回归结论。
func TestInjector_InjectRefusesWhenUnavailable(t *testing.T) {
	i := New(WithURL(""))
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "rabbitmq.inject_message_burst",
		Duration: 30 * time.Second,
		Params:   map[string]interface{}{"sessions": 5},
	})
	if !errors.Is(err, injector.ErrUnavailable) {
		t.Fatalf("Inject error = %v, want ErrUnavailable", err)
	}
	if res != nil {
		t.Fatalf("Inject returned a result %+v alongside an error; a refusal must produce nothing", res)
	}
}

func TestInjector_Inject_UnsupportedType(t *testing.T) {
	i := New(WithURL(""))
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type: "rabbitmq.does_not_exist",
	})
	if err == nil || !errors.Is(err, injector.ErrUnsupportedType) {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestInjector_Cleanup_EmptyIDFails(t *testing.T) {
	i := New()
	err := i.Cleanup(context.Background(), "")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound on empty ID, got %v", err)
	}
}

func TestInjector_Cleanup_UnknownIDFails(t *testing.T) {
	i := New(WithURL(""))
	err := i.Cleanup(context.Background(), "never-injected")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound, got %v", err)
	}
}

// 每一个被列出来的类型，在"没有连接"这个状态下都必须被同样地拒绝。
//
// 拒绝不挑类型。而且这条断言必须在"没有 URL"这个状态下成立——
// 否则在一台配了 OPSKEEPER_HARNESS_RABBITMQ_URL 的开发机上，
// 它会往那套 broker 里灌进十万条撤不回来的消息。
func TestInjector_AllSupportedTypesAreRefusedAlike(t *testing.T) {
	for _, typ := range SupportedTypes() {
		i := New(WithURL(""))
		_, err := i.Inject(context.Background(), injector.InjectSpec{Type: typ})
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want ErrUnavailable", typ, err)
		}
	}
	if len(SupportedTypes()) == 0 {
		t.Fatal("no supported types listed; the test above would pass on an empty list")
	}
}

// 口令不许出现在任何一条会被打印出来的错误里。
func TestRedactHidesThePassword(t *testing.T) {
	got := redact("amqp://opskeeper:hunter2@rabbit.internal:5672/prod")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("redact(%q) = %q, want the password gone", "amqp://opskeeper:hunter2@...", got)
	}
	if !strings.Contains(got, "rabbit.internal:5672") {
		t.Errorf("redact(%q) = %q, want the host kept so the error is still actionable", "…", got)
	}
	// 没有凭据的 URL 必须原样返回，而不是被改成一个更难读的形状。
	plain := "amqp://rabbit.internal:5672"
	if redact(plain) != plain {
		t.Errorf("redact(%q) = %q, want it unchanged", plain, redact(plain))
	}
}
