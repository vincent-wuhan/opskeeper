package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

func TestInjector_Type(t *testing.T) {
	i := New()
	if got := i.Type(); got != "redis." {
		t.Errorf("Type() = %q, want %q", got, "redis.")
	}
}

// 没有连接的注入器必须自报不可用。
//
// 显式给一个空地址，而不是靠"环境里没有"：这台机器上很可能真的跑着
// 一个 Redis（compose 就起了一个），而断言的对象必须仍然是
// "没有连接的那个注入器"，而不是"这台机器恰好没配"。
func TestInjector_CheckAvailableRefuses(t *testing.T) {
	i := New(WithAddr(""))
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
	i := New(WithAddr(""))
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_big_key",
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

// 一个认不出的类型报"不可用"是错的——那是接线问题，与环境无关。
// 两种情况必须能被分开，所以这一条用一个**有地址**的注入器：
// 地址可用时，认不出的类型仍必须报 ErrUnsupportedType。
// 用 New() 而不是 New(WithAddr("")) 是刻意的：那会让"不可用"先命中，
// 而这条断言想问的是顺序。
func TestInjector_Inject_UnsupportedType(t *testing.T) {
	i := New(WithAddr("127.0.0.1:1"), WithPassword(""))
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type: "redis.does_not_exist",
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
	i := New()
	err := i.Cleanup(context.Background(), "never-injected")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound, got %v", err)
	}
}

// 每一个被列出来的类型都必须被同样地拒绝。
//
// 拒绝不挑类型。而且这条断言必须在"没有地址"这个状态下成立——
// 否则在一台配了 OPSKEEPER_HARNESS_REDIS_ADDR 的开发机上，
// 它会把这四种故障真的打进那台 Redis。
func TestInjector_AllSupportedTypesAreRefusedAlike(t *testing.T) {
	for _, typ := range SupportedTypes() {
		i := New(WithAddr(""))
		_, err := i.Inject(context.Background(), injector.InjectSpec{Type: typ})
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want ErrUnavailable", typ, err)
		}
	}
	if len(SupportedTypes()) == 0 {
		t.Fatal("no supported types listed; the test above would pass on an empty list")
	}
}
