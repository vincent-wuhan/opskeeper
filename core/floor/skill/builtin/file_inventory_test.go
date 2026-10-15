package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree builds a directory layout so the walk can be tested against sizes that
// are known rather than against whatever this machine happens to hold.
func tree(t *testing.T, spec map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for rel, size := range spec {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

func inventory(t *testing.T, ctx context.Context, path string, p fileInventoryParams) fileInventoryResult {
	t.Helper()
	if p.Path == "" {
		p.Path = path
	}
	raw, err := walkInventory(ctx, p)
	if err != nil {
		t.Fatalf("walkInventory: %v", err)
	}
	var out fileInventoryResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func dirBytes(got fileInventoryResult, path string) (int64, bool) {
	for _, d := range got.Directories {
		if d.Path == path {
			return d.Bytes, true
		}
	}
	return 0, false
}

func TestTheDirectorySubtotalIsEverythingUnderItAndNotJustItsOwnFiles(t *testing.T) {
	root := tree(t, map[string]int{
		"a/one.log": 100,
		"a/two.log": 200,
		"b/big.bin": 1000,
		"top.txt":   7,
	})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 10, MaxDepth: 3})

	if want := int64(1307); got.TotalBytes != want {
		t.Errorf("total_bytes = %d, want %d", got.TotalBytes, want)
	}
	if want := int64(4); got.TotalFiles != want {
		t.Errorf("total_files = %d, want %d", got.TotalFiles, want)
	}
	if b, ok := dirBytes(got, filepath.Join(root, "a")); !ok || b != 300 {
		t.Errorf("a = %d (found=%v), want 300", b, ok)
	}
	if b, ok := dirBytes(got, filepath.Join(root, "b")); !ok || b != 1000 {
		t.Errorf("b = %d (found=%v), want 1000", b, ok)
	}
	// The root row has to equal the whole tree, or "which directory" cannot
	// be answered by comparing a child against its parent.
	if b, ok := dirBytes(got, root); !ok || b != 1307 {
		t.Errorf("root = %d (found=%v), want the whole tree", b, ok)
	}
}

func TestTheDirectoriesAreRankedBySize(t *testing.T) {
	root := tree(t, map[string]int{
		"small/x": 1,
		"big/y":   5000,
		"mid/z":   500,
	})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 10, MaxDepth: 3})

	if len(got.Directories) < 4 {
		t.Fatalf("directories = %#v, want the root and three children", got.Directories)
	}
	// The root is the largest by construction, and the rest follow.
	if got.Directories[0].Path != root {
		t.Errorf("first = %q, want the root", got.Directories[0].Path)
	}
	if got.Directories[1].Path != filepath.Join(root, "big") {
		t.Errorf("second = %q, want big", got.Directories[1].Path)
	}
}

func TestTheLargestFilesAreRankedAndClamped(t *testing.T) {
	spec := map[string]int{"f1": 10, "f2": 90, "f3": 50, "f4": 30, "f5": 70}
	root := tree(t, spec)
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 2, MaxDepth: 1})

	if len(got.LargestFiles) != 2 {
		t.Fatalf("largest_files = %d, want the requested 2", len(got.LargestFiles))
	}
	if got.LargestFiles[0].Bytes != 90 || got.LargestFiles[1].Bytes != 70 {
		t.Errorf("largest = %d, %d, want 90 then 70", got.LargestFiles[0].Bytes, got.LargestFiles[1].Bytes)
	}
	// The count is filtered, not the accounting: clamping the reply must not
	// change what the disk holds.
	if want := int64(250); got.TotalBytes != want {
		t.Errorf("total_bytes = %d, want %d: a clamped reply is still an honest total", got.TotalBytes, want)
	}
}

func TestTheFileRankIsClampedRatherThanTrusted(t *testing.T) {
	spec := map[string]int{}
	for i := 0; i < 40; i++ {
		spec[fmt.Sprintf("f%02d", i)] = i + 1
	}
	root := tree(t, spec)
	if got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 5000}); len(got.LargestFiles) != 40 {
		t.Errorf("an oversized top_n returned %d rows; there are only 40 files", len(got.LargestFiles))
	}
	if got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 5}); len(got.LargestFiles) != 5 {
		t.Errorf("top_n 5 returned %d rows, want 5", len(got.LargestFiles))
	}
}

func TestAMinimumSizeKeepsTheNoiseOutWithoutChangingTheTotal(t *testing.T) {
	root := tree(t, map[string]int{"tiny": 1, "big": 4096})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 10, MaxDepth: 2, MinSizeBytes: 1000})

	if len(got.LargestFiles) != 1 {
		t.Fatalf("largest_files = %#v, want only the file above the floor", got.LargestFiles)
	}
	if !strings.HasSuffix(got.LargestFiles[0].Path, "big") {
		t.Errorf("kept %q, want the large one", got.LargestFiles[0].Path)
	}
	if want := int64(4097); got.TotalBytes != want {
		t.Errorf("total_bytes = %d, want %d: the filter shapes the list, not the account", got.TotalBytes, want)
	}
}

func TestMaxDepthDecidesWhichDirectoriesAreReported(t *testing.T) {
	root := tree(t, map[string]int{"a/b/c/deep.log": 10})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 20, MaxDepth: 1})

	if _, ok := dirBytes(got, filepath.Join(root, "a")); !ok {
		t.Error("depth 1 should still report a")
	}
	if _, ok := dirBytes(got, filepath.Join(root, "a", "b")); ok {
		t.Error("depth 1 reported a/b, which is below the depth asked for")
	}
	// The bytes are still counted. Depth decides what is SHOWN, not what was
	// read — a tool that stopped reading at depth 1 would report a disk as
	// empty for having a deep tree.
	if want := int64(10); got.TotalBytes != want {
		t.Errorf("total_bytes = %d, want %d: depth shapes the list, not the account", got.TotalBytes, want)
	}
}

func TestMaxDepthIsClampedToTheCeiling(t *testing.T) {
	root := tree(t, map[string]int{"a/b/c/d.log": 1})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 50, MaxDepth: 9999})
	if len(got.Directories) > maxInventoryDepth+1 {
		t.Errorf("reported %d directory levels for a ceiling of %d", len(got.Directories), maxInventoryDepth)
	}
}

func TestASymlinkIsNamedButNotFollowed(t *testing.T) {
	// A symlink to the tree's own parent is the shape that turns a walker
	// into an infinite loop, and following it would also count the same bytes
	// twice. Both are wrong for a number someone acts on.
	root := tree(t, map[string]int{"real/data.log": 100})
	loop := filepath.Join(root, "real", "back")
	if err := os.Symlink(root, loop); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	done := make(chan fileInventoryResult, 1)
	go func() {
		done <- inventory(t, context.Background(), root, fileInventoryParams{TopN: 20, MaxDepth: 3})
	}()
	select {
	case got := <-done:
		if want := int64(100); got.TotalBytes != want {
			t.Errorf("total_bytes = %d, want %d: the link was followed and the bytes counted twice", got.TotalBytes, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk followed a symlink back to its own root and never returned")
	}
}

func TestASymlinkToElsewhereIsCountedOnceAndByNameOnly(t *testing.T) {
	outside := tree(t, map[string]int{"huge.bin": 1 << 20})
	root := tree(t, map[string]int{"a.log": 5})
	if err := os.Symlink(outside, filepath.Join(root, "elsewhere")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 20, MaxDepth: 3})
	if want := int64(5); got.TotalBytes != want {
		t.Errorf("total_bytes = %d, want %d: the link's target was walked and added in", got.TotalBytes, want)
	}
}

func TestAnUnreadableDirectoryDoesNotAbandonTheOtherHalf(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything, so this cannot be arranged here")
	}
	root := tree(t, map[string]int{"open/a.log": 100, "closed/secret.log": 5000})
	if err := os.Chmod(filepath.Join(root, "closed"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "closed"), 0o750) })

	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 20, MaxDepth: 3})
	if got.TotalBytes != 100 {
		t.Errorf("total_bytes = %d, want the readable half (100)", got.TotalBytes)
	}
	if got.Reason == "" || !strings.Contains(got.Reason, "closed") {
		t.Errorf("reason = %q, want it to name the directory nobody could account for", got.Reason)
	}
}

func TestATruncatedWalkSaysSoAndStillAnswers(t *testing.T) {
	// The whole contract of the ceiling is this: a partial answer that says
	// it is partial beats a complete-looking one that is not.
	spec := map[string]int{}
	for i := 0; i < 30; i++ {
		spec[fmt.Sprintf("d%d/f.log", i)] = 10
	}
	root := tree(t, spec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	raw, err := walkInventory(ctx, fileInventoryParams{Path: root, TopN: 5, MaxDepth: 3})
	if err != nil {
		t.Fatalf("a cancelled walk returned an error instead of a partial answer: %v", err)
	}
	var got fileInventoryResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Error("truncated = false after the context ended")
	}
	if got.Reason == "" {
		t.Error("a truncated walk gave no reason")
	}
}

func TestAFullWalkSaysItIsNotTruncated(t *testing.T) {
	root := tree(t, map[string]int{"a.log": 10})
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 5, MaxDepth: 3})
	if got.Truncated {
		t.Errorf("truncated = true, reason = %q, on a walk that finished", got.Reason)
	}
}

func TestAnEmptyDirectoryIsAnAnswerRatherThanAFailure(t *testing.T) {
	root := t.TempDir()
	got := inventory(t, context.Background(), root, fileInventoryParams{TopN: 5, MaxDepth: 3})
	if got.TotalBytes != 0 || got.TotalFiles != 0 {
		t.Errorf("got %#v, want an empty but successful account", got)
	}
}

func TestTheWalkRefusesAPathThatIsNotADirectory(t *testing.T) {
	// "A single file has no tree" is the honest answer. Walking a file's
	// parent and reporting the whole filesystem would answer a question
	// nobody asked.
	root := tree(t, map[string]int{"one.log": 1})
	if _, err := walkInventory(context.Background(), fileInventoryParams{Path: filepath.Join(root, "one.log")}); err == nil {
		t.Fatal("a file was accepted as a tree to account for")
	}
}

func TestAPathThatDoesNotExistIsAFailureRatherThanAnEmptyAnswer(t *testing.T) {
	_, err := walkInventory(context.Background(), fileInventoryParams{Path: filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("a missing path reported an empty tree instead of an error")
	}
}

// --- the declared interface ------------------------------------------------

func TestTheDeclaredParamsAreCheckedBeforeAnythingIsRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"no path", `{}`, "path is required"},
		{"relative", `{"path":"tmp/x"}`, "must be absolute"},
		{"traversal", `{"path":"/var/../../etc"}`, "must not contain .."},
		{"negative floor", `{"path":"/tmp","min_size_bytes":-1}`, "must not be negative"},
		{"wrong type", `{"path":42}`, "decode params"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (FileInventory{}).Execute(context.Background(), json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("%s was accepted", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestTheWalkBudgetIsShorterThanTheDeclaredTimeout(t *testing.T) {
	// The tool returns an answer that says it stopped; the broker is the
	// backstop for when it cannot. Two ceilings at the same number would mean
	// the backstop is what fires, and the caller gets nothing.
	limits := (FileInventory{}).Metadata().Limits
	if int(inventoryWalkBudget.Seconds()) >= limits.TimeoutSeconds {
		t.Errorf("the walk may take %s but the gate allows %ds: the broker would kill it first",
			inventoryWalkBudget, limits.TimeoutSeconds)
	}
}
