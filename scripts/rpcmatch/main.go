// Command rpcmatch answers a question no dead-code report in this tree
// asks: **a method the manager registers, does production ever send it?**
//
// Why this exists
// ---------------
// Decision 346 deleted the whole webssh tunnel surface — six methods and
// sixteen types — and the reason it took that long is worth stating,
// because it is a shape the existing gates do not see.
//
// Thirteen of the sixteen types were already reported dead. The other
// three were not, and could not be: the manager *registers* handlers for
// `shell_output` and `shell_exit`, so a reachability walk finds the types
// and stops. The walk is right and the answer is still wrong. Those two
// handlers had no sender at all — nothing on the edge had ever sent
// either message, because the thing that would produce them (an SSH
// client on the node) had been moved to the manager and no longer
// existed there.
//
// So the missing question is not "can I walk to it" but **"who on the
// other end speaks it"**. A receiver with no counterparty is code that
// compiles, passes every gate, and does nothing — and in this case it was
// not doing nothing: `DispatchOutput` wrote bytes into a live operator's
// terminal keyed by a SessionID that arrives on the wire.
//
// What this checks
// ----------------
//
//   - The receiver set: every `tunnel.MethodX` passed to `c.Register` in
//     core/manager/service/frontierbound (production files only).
//   - The sender set: every `tunnel.MethodX` in a call position outside
//     core/manager, **excluding _test.go** — an edge registering a handler
//     for a method the manager calls is not a sender of anything the
//     manager registered.
//
// A method in the receiver set with no production sender is an orphan and
// fails the check. Tests are excluded on purpose: a test proves the
// calling API exists, not that anything calls it in production, and the
// one time this rule was broken by accident it produced a green gate over
// four methods that no node had ever sent.
//
// The reverse direction is deliberately not checked. "Sent but never
// registered" would also be a defect, but the same constant names serve
// both directions on this wire (MethodHeartbeat is edge→manager while
// MethodGetProcessList is manager→edge), so the text of a call site does
// not say which way it points. Getting that wrong would mean inventing a
// direction this tool cannot see.
//
// Usage:
//
//	go run ./scripts/rpcmatch [repo-root]
//
// Exit status is 1 if any registered method has no production sender.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const receiverPkg = "core/manager/service/frontierbound"

var (
	// c.Register(ctx, tunnel.MethodX, ...) — the receiver side.
	registerRe = regexp.MustCompile(`\.Register\(\s*\w+\s*,\s*tunnel\.(Method\w+)`)
	// A sender is a method name in a **call** position, not merely a
	// mention. This distinction is the whole check: the edge registers a
	// handler for the manager→edge methods (MethodGetProcessList), so a
	// "referenced anywhere outside core/manager" rule would let a manager
	// registration of a manager→edge method pass on the strength of the
	// edge's own handler for it. `(?s)` because the call and the argument
	// are routinely split across lines.
	senderRe = regexp.MustCompile(`(?s)\b(?:Call|CallAsync|Notify|Push|Send)\w*\(.{0,200}?tunnel\.(Method\w+)`)
)

// skipDirs are not source we want to read.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "testdata": true, "vendor": true}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	receivers, err := receiversOf(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rpcmatch:", err)
		os.Exit(2)
	}
	if len(receivers) == 0 {
		fmt.Fprintln(os.Stderr, "rpcmatch: no registered methods found — the check would pass vacuously")
		os.Exit(2)
	}
	senders, err := sendersOf(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rpcmatch:", err)
		os.Exit(2)
	}

	var orphans []string
	for _, m := range receivers {
		if !senders[m] {
			orphans = append(orphans, m)
		}
	}
	sort.Strings(orphans)
	for _, m := range orphans {
		fmt.Printf("rpcmatch: %s is registered by %s and never sent by production code — "+
			"a receiver with no counterparty, or wire nobody speaks\n", m, receiverPkg)
	}
	fmt.Printf("rpcmatch: %d methods registered by %s, %d of them sent by production code, %d orphaned\n",
		len(receivers), receiverPkg, len(receivers)-len(orphans), len(orphans))

	if len(orphans) > 0 {
		os.Exit(1)
	}
	fmt.Println("rpcmatch: every method the manager registers on the tunnel has a sender in production code")
}

func receiversOf(root string) ([]string, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || !strings.HasSuffix(filepath.ToSlash(path), receiverPkg) && d.Name() != filepath.Base(receiverPkg) {
				return nil
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.ToSlash(filepath.Dir(rel)) != receiverPkg {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range registerRe.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, err
}

func sendersOf(root string) (map[string]bool, error) {
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		slash := filepath.ToSlash(rel)
		// Everything inside core/manager is excluded, not just the receiver
		// package. The receiver and any reference in the same process are
		// not two ends of a conversation — and the wire carries both
		// directions under one namespace, so MethodGetProcessList is called
		// *by* the manager while MethodPushHostMetrics is called *to* it.
		// Counting a manager-side call as a sender would let any
		// manager→edge method pass this check by being merely mentioned.
		if strings.HasPrefix(slash, "core/manager/") || strings.HasPrefix(slash, "core/floor/tunnel/") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range senderRe.FindAllStringSubmatch(string(src), -1) {
			out[strings.TrimPrefix(m[1], "tunnel.")] = true
		}
		return nil
	})
	return out, err
}
