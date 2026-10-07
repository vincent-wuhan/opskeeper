package skill

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// spillInto builds an oversized reply without allocating a megabyte per case.
func oversized(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'x'
	}
	return out
}

func TestSpillPassesAReplythatFitsThrough(t *testing.T) {
	res := Spill("host_dmesg", t.TempDir(), 1024, DefaultPreviewBytes, []byte("short output"))

	if res.Spilled {
		t.Error("a reply inside the limit was spilled")
	}
	if res.Inline != "short output" {
		t.Errorf("Inline = %q, want the whole reply", res.Inline)
	}
	if res.TotalBytes != len("short output") || res.Limit != 1024 {
		t.Errorf("TotalBytes=%d Limit=%d; the caller is told what it was bounded against", res.TotalBytes, res.Limit)
	}
}

// A zero limit means the default, not "no limit". The failure this whole
// mechanism exists to prevent is a reply that was never bounded, and a
// caller that passes zero must not be the way to get one.
func TestSpillTreatsNoLimitAsTheDefaultRatherThanAsNone(t *testing.T) {
	dir := t.TempDir()
	res := Spill("host_grep_file", dir, 0, DefaultPreviewBytes, oversized(int(DefaultMaxOutputBytes)+1))

	if res.Limit != DefaultMaxOutputBytes {
		t.Errorf("Limit = %d, want the %d byte default", res.Limit, DefaultMaxOutputBytes)
	}
	if !res.Spilled {
		t.Error("a reply over the default ceiling was not spilled")
	}
	_ = os.Remove(res.SpillPath)
}

func TestSpillWritesTheWholeReplyWhereTheModelCanBeToldToLook(t *testing.T) {
	out := oversized(int(DefaultMaxOutputBytes) + 512*1024)
	res := Spill("host_grep_file", t.TempDir(), DefaultMaxOutputBytes, DefaultPreviewBytes, out)

	if !res.Spilled || res.SpillPath == "" {
		t.Fatalf("an oversized reply was not spilled: %+v", res)
	}
	info, err := os.Stat(res.SpillPath)
	if err != nil {
		t.Fatalf("spill file missing: %v", err)
	}
	if info.Size() != int64(len(out)) {
		t.Errorf("spill holds %d bytes, want the full %d", info.Size(), len(out))
	}
	if !strings.Contains(res.Inline, res.SpillPath) {
		t.Errorf("the inline notice does not name where the output went: %q", res.Inline)
	}
	// The whole point of spilling: the model is told how to get the rest,
	// rather than being handed a wall of text it cannot use.
	if !strings.Contains(res.Inline, "file tool") {
		t.Errorf("the notice does not say how to read the spilled output: %q", res.Inline)
	}
	if len(res.Inline) > 2*DefaultPreviewBytes {
		t.Errorf("the inline notice is %d bytes; the thing being bounded is the inline part", len(res.Inline))
	}
	_ = os.Remove(res.SpillPath)
}

// A spilled reply is node evidence. World-readable would hand it to every
// local account on a host that has more than one, which is a different
// capability from the one the agent was granted.
func TestSpillIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}
	res := Spill("host_sosreport", t.TempDir(), 64, DefaultPreviewBytes, oversized(1024))
	if !res.Spilled {
		t.Fatal("nothing was spilled")
	}
	defer func() { _ = os.Remove(res.SpillPath) }()

	info, err := os.Stat(res.SpillPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("spill file mode is %04o; it is readable by group or other", mode)
	}
}

// The name comes from a manifest, so it is not trusted to be a bare
// identifier. A name carrying a path separator would otherwise write the
// spill wherever the package chose.
func TestASpillNameCannotBecomeAPath(t *testing.T) {
	dir := t.TempDir()
	res := Spill("../../etc/evil", dir, 64, DefaultPreviewBytes, oversized(1024))

	if !res.Spilled {
		t.Fatal("nothing was spilled")
	}
	defer func() { _ = os.Remove(res.SpillPath) }()

	if filepath.Dir(res.SpillPath) != dir {
		t.Errorf("spill landed in %s, outside %s", filepath.Dir(res.SpillPath), dir)
	}
	if strings.Contains(filepath.Base(res.SpillPath), "/") {
		t.Errorf("spill name %q carries a separator", res.SpillPath)
	}
}

// A node that never reboots and an investigation that never ends would
// otherwise fill /var/tmp. The prune is bounded to this mechanism's own
// files, because /var/tmp on a real host is not exclusively ours.
func TestOldSpillsArePrunedAndForeignFilesAreNot(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "opskeeper-host_dmesg-1.log")
	foreign := filepath.Join(dir, "somebody-elses-file.log")
	for _, path := range []string{old, foreign} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		stale := time.Now().Add(-2 * spillRetention)
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	Spill("host_dmesg", dir, 64, DefaultPreviewBytes, oversized(1024))

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a spill older than the retention is still on disk")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("the prune removed a file this mechanism did not write")
	}
}

// An unwritable spill directory degrades to the temp directory and still
// spills. The test that used to sit here asserted the opposite and was wrong:
// the fallback exists precisely so a read-only /var/tmp is not the end of the
// answer.
func TestAnUnwritableSpillDirectoryDegradesToTheTempDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("an unwritable directory is only meaningful as a non-root user")
	}
	dir := filepath.Join(t.TempDir(), "read-only")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := Spill("host_dmesg", dir, 64, DefaultPreviewBytes, oversized(4096))

	if !res.Spilled {
		t.Fatal("a read-only spill directory ended the answer instead of degrading it")
	}
	defer func() { _ = os.Remove(res.SpillPath) }()
	// Cleaned because os.TempDir() carries a trailing separator on some
	// platforms and a string comparison would then report a difference
	// that is not one.
	if got, want := filepath.Clean(filepath.Dir(res.SpillPath)), filepath.Clean(tempDir()); got != want {
		t.Errorf("spill landed in %s, want the temp directory %s", got, want)
	}
}

// Both locations unwritable is the one case where the model gets a preview
// and no file. It has to still get a usable notice: failing the tool would
// turn a truncatable reply into no reply, and the model cannot tell those
// apart from a broken tool.
func TestWhenNothingCanBeWrittenTheReplyIsStillBoundedAndExplained(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("an unwritable directory is only meaningful as a non-root user")
	}
	blocked := t.TempDir()
	seed := func(name string) string {
		path := filepath.Join(blocked, name)
		if err := os.MkdirAll(path, 0o500); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		return path
	}
	original := tempDir
	tempDir = func() string { return seed("temp") }
	defer func() { tempDir = original }()

	res := Spill("host_dmesg", seed("spill"), 64, DefaultPreviewBytes, oversized(4096))

	if res.Spilled {
		t.Error("a spill was reported although nothing could be written")
	}
	if res.SpillPath != "" {
		t.Errorf("a path was reported although nothing was written: %q", res.SpillPath)
	}
	if !strings.Contains(res.Inline, "could not be saved") {
		t.Errorf("the notice does not say what happened: %q", res.Inline)
	}
	if res.TotalBytes != 4096 || res.Limit != 64 {
		t.Errorf("TotalBytes=%d Limit=%d; the size facts survive the failure", res.TotalBytes, res.Limit)
	}
}
