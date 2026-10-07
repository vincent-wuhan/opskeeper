package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

func TestInjector_Type(t *testing.T) {
	i := New(WithKubeconfig(""))
	if got := i.Type(); got != "k8s." {
		t.Errorf("Type() = %q, want %q", got, "k8s.")
	}
}

// 一个还没接线的注入器必须自报不可用。上一版这个测试断言 IsAvailable() == true，
// 断言的是一个"它能干活"的谎——而 Inject 那时只是往 map 里写了一行。
func TestInjector_CheckAvailableRefuses(t *testing.T) {
	i := New(WithKubeconfig(""))
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
	i := New(WithKubeconfig(""))
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "k8s.set_bad_image",
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
	i := New(WithKubeconfig(""))
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type: "k8s.does_not_exist",
	})
	if err == nil || !errors.Is(err, injector.ErrUnsupportedType) {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestInjector_Cleanup_EmptyIDFails(t *testing.T) {
	i := New(WithKubeconfig(""))
	err := i.Cleanup(context.Background(), "")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound on empty ID, got %v", err)
	}
}

func TestInjector_Cleanup_UnknownIDFails(t *testing.T) {
	i := New(WithKubeconfig(""))
	err := i.Cleanup(context.Background(), "never-injected")
	if !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Errorf("expected ErrInjectionNotFound, got %v", err)
	}
}

// 每一个被列出来的类型，在"没有 kubeconfig"这个状态下都必须被同样地拒绝。
//
// 拒绝不挑类型。而且这条断言必须在"没有 kubeconfig"这个状态下成立——
// New() 会读 KUBECONFIG，不钉住的话，开发者本机配了集群之后
// 它会去真的 cordon 他的节点。
func TestInjector_AllSupportedTypesAreRefusedAlike(t *testing.T) {
	for _, typ := range SupportedTypes() {
		i := New(WithKubeconfig(""))
		_, err := i.Inject(context.Background(), injector.InjectSpec{Type: typ})
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want ErrUnavailable", typ, err)
		}
	}
	if len(SupportedTypes()) == 0 {
		t.Fatal("no supported types listed; the test above would pass on an empty list")
	}
}
