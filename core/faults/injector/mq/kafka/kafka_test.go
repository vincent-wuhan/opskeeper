package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

func TestInjector_Type(t *testing.T) {
	i := New()
	if got := i.Type(); got != "kafka." {
		t.Errorf("Type() = %q, want %q", got, "kafka.")
	}
}

// 一个还没接线的注入器必须自报不可用。上一版这个测试断言 IsAvailable() == true，
// 断言的是一个"它能干活"的谎——而 Inject 那时只是往 map 里写了一行。
func TestInjector_CheckAvailableRefuses(t *testing.T) {
	// WithBrokers() 显式给空值，而不是靠"环境变量没设"：后者在一台配了
	// OPSKEEPER_HARNESS_KAFKA_BROKERS 的开发机上会变成一次真的注入。
	i := New(WithBrokers())
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
	i := New(WithBrokers())
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "kafka.kill_broker",
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
	i := New()
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type: "kafka.does_not_exist",
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
	i := New(WithBrokers())
	err := i.Cleanup(context.Background(), "never-injected")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound, got %v", err)
	}
}

// 每一个被列出来的类型，在"没有 broker"这个状态下都必须被同样地拒绝。
//
// 拒绝不挑类型。而且这条断言必须在"没有地址"这个状态下成立——
// 否则在一台配了 OPSKEEPER_HARNESS_KAFKA_BROKERS 的开发机上，
// 它会把这两种故障真的打进那套 Kafka（记录撤不回来）。
func TestInjector_AllSupportedTypesAreRefusedAlike(t *testing.T) {
	for _, typ := range SupportedTypes() {
		i := New(WithBrokers())
		_, err := i.Inject(context.Background(), injector.InjectSpec{Type: typ})
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want ErrUnavailable", typ, err)
		}
	}
	if len(SupportedTypes()) == 0 {
		t.Fatal("no supported types listed; the test above would pass on an empty list")
	}
}
