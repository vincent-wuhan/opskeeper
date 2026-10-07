// Package-level spill: what happens to a tool reply that is larger than the
// host is willing to hand back.
//
// # Why this exists, and why it used to do nothing
//
// This started life as `builtin.MaxInlineBytes` with a helper that no builtin
// called: the constant was written for exactly this purpose, the helper was
// written and tested, and no high-cardinality tool ever reached it. The
// comment on the old file said it applied to "all builtin skills"; `grep` said
// otherwise, and the tools it named — host_dmesg, host_grep_file,
// host_sosreport — were the ones that could put a gigabyte into a model's
// context.
//
// So it lives here now, in the registry package rather than in the catalogue
// of tools, and the tool broker calls it for every reply. The ceiling is an
// argument rather than a constant, because the ceiling belongs to the tool's
// declared limits; this package provides the mechanism and the default, and
// the manifest provides the number.
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultMaxOutputBytes is the ceiling a tool gets when it declares none.
//
// One mebibyte, because that is the number this repository wrote down when it
// wrote the helper that was never called. It is high enough that no
// legitimate read is cut — a dmesg tail, a grep hit list, a probe result — and
// low enough that a runaway cannot fill a context window. The point is not
// the exact value; the point is that there is one.
const DefaultMaxOutputBytes int64 = 1 << 20

// DefaultSpillDir is where an oversized reply is written.
//
// /var/tmp rather than a spool directory under the edge's own state: an
// operator debugging a truncated tool wants to find the file with the same
// `ls` they would have used anyway.
const DefaultSpillDir = "/var/tmp"

// DefaultPreviewBytes is how much of an oversized reply is echoed inline
// when the caller does not say.
//
// Small on purpose. The inline part is what enters the model's context, and
// the model that asked for a 40 MiB grep is the reason it is being cut; it
// gets the path, the size, and a taste, and it can ask for a narrower slice.
const DefaultPreviewBytes = 1024

// spillRetention is how long a spilled reply is kept.
//
// Bounded because this is a diagnostics buffer on a host that may never be
// rebooted: a fleet of nodes writing an unbounded pile of tool output into
// /var/tmp is a disk incident with no operator in the loop. An operator who
// wants a file kept copies it somewhere real.
const spillRetention = 24 * time.Hour

// tempDir is the fallback location, as a variable so the double-failure
// branch — the only branch where the caller gets no file at all — can be
// reached by a test. Everything else here is deliberately not injectable:
// a spill path that a test could redirect is a spill path a package could
// redirect.
var tempDir = os.TempDir

// SpillResult is what Spill returns.
type SpillResult struct {
	// Inline is the text to hand back: the whole reply when it fits, and a
	// notice naming the size, the limit and the path when it does not.
	Inline string
	// Spilled reports whether the full reply reached disk.
	Spilled bool
	// SpillPath is where it went, when it did.
	SpillPath string
	// TotalBytes is the size of the original reply, which is the fact an
	// operator needs and the model cannot infer.
	TotalBytes int
	// Limit is the ceiling that was applied.
	Limit int64
}

// Spill bounds one tool reply.
//
// previewBytes caps the inline part, and it exists as a parameter rather than
// as a constant because the *caller's* limit governs it: a caller whose
// ceiling is 512 bytes cannot inline a kilobyte of preview and still honour
// its own ceiling. A bound whose replacement is larger than the thing it
// replaced is not a bound, and that is the bug this parameter was added to
// fix — see toolbroker.boundOutput.
//
// A limit of zero or less means DefaultMaxOutputBytes; a dir of "" means
// DefaultSpillDir; a preview below zero means no preview. All three defaults
// exist so that a caller with nothing to say still gets a bounded answer — the failure this whole mechanism exists to
// prevent is a reply that was never bounded at all.
//
// Spilling is best-effort and never fails the call. A host whose /var/tmp is
// read-only still returns a usable notice with a preview; what it loses is
// the full text, which is a degradation, not an error. The alternative —
// failing the tool because the disk is full — would turn a truncatable
// answer into no answer, and the model cannot tell those apart from "the tool
// is broken".
func Spill(toolName, dir string, limit int64, previewBytes int, output []byte) SpillResult {
	if limit <= 0 {
		limit = DefaultMaxOutputBytes
	}
	if dir == "" {
		dir = DefaultSpillDir
	}
	if previewBytes < 0 {
		previewBytes = 0
	}
	res := SpillResult{TotalBytes: len(output), Limit: limit}
	if int64(len(output)) <= limit {
		res.Inline = string(output)
		return res
	}

	path, err := writeSpill(toolName, dir, output)
	if err != nil {
		preview := previewOf(output, previewBytes)
		res.Inline = fmt.Sprintf("[%d bytes exceed the %d byte limit for %s; full output could not be saved: %v]\n%s",
			len(output), limit, toolName, err, preview)
		return res
	}
	res.Spilled = true
	res.SpillPath = path
	res.Inline = fmt.Sprintf("[%d bytes exceed the %d byte limit for %s; full output saved to %s — read it with a file tool or narrow the query]\n%s",
		len(output), limit, toolName, path, previewOf(output, previewBytes))
	return res
}

// writeSpill puts the reply on disk, degrading through two failures.
//
// The degradation is tested rather than assumed: a directory that exists and
// is read-only makes MkdirAll succeed and WriteFile fail, which is exactly
// what a root-owned /var/tmp looks like in a container, and a helper that
// only degrades on mkdir reports success having written nothing.
func writeSpill(toolName, dir string, output []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		dir = tempDir()
	}
	name := fmt.Sprintf("opskeeper-%s-%d.log", sanitizeToolName(toolName), time.Now().UnixNano())
	path := filepath.Join(dir, name)

	// 0600, not 0644. A spilled reply is node evidence — kernel ring
	// buffers, command lines, log lines — and on a host with more than one
	// local account, world-readable is a way for another account to read
	// what the agent was allowed to read.
	if err := os.WriteFile(path, output, 0o600); err != nil {
		alt := filepath.Join(tempDir(), name)
		if altErr := os.WriteFile(alt, output, 0o600); altErr != nil {
			return "", err
		}
		path = alt
	}
	pruneSpills(dir)
	return path, nil
}

// pruneSpills deletes this tool's own spills older than the retention.
//
// Only this tool's own files, matched on the prefix writeSpill chose: a prune
// that swept the directory by age would delete whatever else lives in
// /var/tmp, which on a real host is not exclusively ours.
func pruneSpills(dir string) {
	cutoff := time.Now().Add(-spillRetention)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "opskeeper-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// sanitizeToolName keeps a tool name from becoming a path.
//
// The name comes from a manifest, so it is not trusted to be a bare
// identifier: a name carrying a slash would otherwise write the spill
// wherever the package chose.
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

func previewOf(output []byte, previewBytes int) string {
	if len(output) <= previewBytes {
		return string(output)
	}
	return string(output[:previewBytes])
}
