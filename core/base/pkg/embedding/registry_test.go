package embedding

import (
	"context"
	"errors"
	"testing"
)

type stubEmbedder struct{ dim int }

func (s stubEmbedder) Dim() int { return s.dim }
func (s stubEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, nil
}

func TestRegisteredProvidersCoversBuiltins(t *testing.T) {
	got := RegisteredProviders()
	want := map[string]bool{"openai": true, "local": true, "fastembed": true, "onnx": true}
	for _, name := range got {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("these built-in providers have no factory: %v", want)
	}
}

func TestRegisterProviderRefusesDuplicate(t *testing.T) {
	err := RegisterProvider("openai", func(Config) (Embedder, error) { return stubEmbedder{}, nil })
	if err == nil {
		t.Fatal("registering over a built-in provider must be refused")
	}
}

func TestRegisterProviderRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterProvider("", func(Config) (Embedder, error) { return stubEmbedder{}, nil }); err == nil {
		t.Fatal("an empty provider name must be refused")
	}
	if err := RegisterProvider("brand-new", nil); err == nil {
		t.Fatal("a nil factory must be refused")
	}
}

func TestNewUnknownProviderErrors(t *testing.T) {
	_, err := New(Config{Provider: "does-not-exist"})
	if err == nil {
		t.Fatal("an unknown provider must be refused")
	}
}

func TestNewDispatchesThroughTheRegistry(t *testing.T) {
	// A provider registered by a package builds through the same path a
	// built-in does, so New is a lookup rather than a switch.
	name := "third-party-embedding-test"
	if _, err := New(Config{Provider: name}); err == nil {
		t.Skip("provider already registered by another test")
	}
	if err := RegisterProvider(name, func(cfg Config) (Embedder, error) {
		return stubEmbedder{dim: 42}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	e, err := New(Config{Provider: name})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if e.Dim() != 42 {
		t.Fatalf("Dim = %d; want 42 (the registered factory must be the one that built it)", e.Dim())
	}
}

func TestFactoryErrorPropagates(t *testing.T) {
	name := "failing-embedding-test"
	if _, err := New(Config{Provider: name}); err == nil {
		t.Skip("provider already registered by another test")
	}
	want := errors.New("no such model")
	if err := RegisterProvider(name, func(Config) (Embedder, error) { return nil, want }); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := New(Config{Provider: name})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v; want %v", err, want)
	}
}
