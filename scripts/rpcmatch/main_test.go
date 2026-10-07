package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixture tree is written rather than committed because the check reads
// a repository layout, and a fixture that is itself a repository starts
// drifting from the thing it is a fixture for.
//
// Four cases, each of which has already been got wrong at least once while
// this check was being written:

// orphan            a manager registration with no sender anywhere
// sent              a manager registration with a real edge-side Call
// mentionedOnly     a method the edge only RegisterHandler's — the
//
//	manager→edge direction, which must NOT satisfy a
//	registration
//
// testOnlySender    a method sent only from a _test.go file
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func handlersFile(body string) string {
	return `package frontierbound

func install(c *Client) error {
` + body + `
	return nil
}
`
}

func TestOrphanIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile(
			"if err := c.Register(ctx, tunnel.MethodGhost, h); err != nil { return err }"),
	})
	receivers, err := receiversOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(receivers) != 1 || receivers[0] != "MethodGhost" {
		t.Fatalf("receivers = %v, want [MethodGhost]", receivers)
	}
	senders, err := sendersOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if senders["MethodGhost"] {
		t.Fatal("a method nobody sends must not appear in the sender set")
	}
}

func TestAnEdgeSideCallIsASender(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile(
			"if err := c.Register(ctx, tunnel.MethodLive, h); err != nil { return err }"),
		"core/edge/biz/agent.go": `package biz
func send(c *Client) error {
	_, err := c.Call(ctx, tunnel.MethodLive, body)
	return err
}`,
	})
	senders, err := sendersOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if !senders["MethodLive"] {
		t.Fatal("an edge-side Call must count as a sender")
	}
}

// The case that made the first version of this check a no-op for the
// direction it was written to catch.
func TestAnEdgeRegisterHandlerIsNotASender(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile(
			"if err := c.Register(ctx, tunnel.MethodManagerToEdge, h); err != nil { return err }"),
		"core/edge/service/handlers.go": `package service
func install(c *Client) error {
	return c.RegisterHandler(tunnel.MethodManagerToEdge, h)
}`,
	})
	senders, err := sendersOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if senders["MethodManagerToEdge"] {
		t.Fatal("the edge registering a handler is the edge being *called*; it is not a sender")
	}
}

func TestATestOnlyCallIsNotASender(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile(
			"if err := c.Register(ctx, tunnel.MethodTested, h); err != nil { return err }"),
		"core/edge/biz/agent_test.go": `package biz
func TestSend(t *testing.T) {
	_, err := c.Call(ctx, tunnel.MethodTested, body)
}`,
	})
	senders, err := sendersOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if senders["MethodTested"] {
		t.Fatal("a test proves the calling API exists, not that anything calls it in production")
	}
}

// A caller inside core/manager is the same process as the receiver.
func TestAManagerSideCallIsNotASender(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile(
			"if err := c.Register(ctx, tunnel.MethodSameProcess, h); err != nil { return err }"),
		"core/manager/biz/edge/service.go": `package edge
func call(ctx context.Context, c *frontierbound.Client) error {
	_, err := c.Call(ctx, edgeID, tunnel.MethodSameProcess, body)
	return err
}`,
	})
	senders, err := sendersOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if senders["MethodSameProcess"] {
		t.Fatal("core/manager is the receiver's own process; a call there is not a counterparty")
	}
}

// The vacuity guard: a tree with no registrations must not read as a pass.
func TestNoRegistrationsIsVacuous(t *testing.T) {
	root := writeTree(t, map[string]string{
		"core/manager/service/frontierbound/handlers.go": handlersFile("return nil"),
	})
	receivers, err := receiversOf(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(receivers) != 0 {
		t.Fatalf("receivers = %v, want none", receivers)
	}
}
