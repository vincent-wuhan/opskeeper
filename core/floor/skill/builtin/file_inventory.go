package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

func init() { skill.Register(&FileInventory{}) }

// Bounds for a walk. They are constants rather than parameters because the
// failure they prevent — a walk that never returns and a reply that fills a
// context window — is not something a caller should be able to opt into by
// forgetting an argument.
const (
	// maxWalkEntries is the number of filesystem entries a single call may
	// visit. A node's filesystem has far more of them than any question is
	// worth answering, and this is the number that turns "walks forever"
	// into "answers and says it stopped".
	maxWalkEntries = 200_000
	// maxInventoryDepth stops the directory subtotals from being a full
	// tree. Deeper than this, the size has been attributed to a parent
	// already and the extra levels say less each time.
	maxInventoryDepth = 8
	// walkPollInterval is how often the walk checks the deadline and the
	// context. One entry is fast; a million of them are not, and a walk
	// that cannot be cancelled is a tool that holds a gate slot.
	walkPollInterval = 2048
)

// FileInventory accounts for the space under one path.
//
// The edge has had find_large_files / du_summary / stat_file for a while,
// reached over the frontier tunnel from the control plane's BaseTools. That
// is not the same capability as a node agent being able to ask: it answers
// about the path the control plane was pointed at, and a node debugging its
// own full disk has no route to it. So the golden case host/disk-full stayed
// a GAP with the reason "nothing anywhere reads a file inventory for a node"
// — which was true, and is the sentence this file exists to make false.
type FileInventory struct{}

func (FileInventory) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_file_inventory",
		Name:        "文件空间盘点",
		Description: "盘点一个目录树占用了多少空间：按大小排的目录小计 + 最大的文件. 诊断磁盘满 / 空间暴涨时定位到具体路径. NOT for: 按内容搜索 (host_grep_file).",
		Class:       skill.ClassSafe,
		Category:    "filesystem",
		Params: skill.ParamSchema{
			{Name: "path", Param: skill.Param{Type: "string", Required: true, Desc: "目录绝对路径"}},
			{Name: "top_n", Param: skill.Param{Type: "int", Default: 20, Desc: "每张榜返回条数, 默认 20, 上限 100"}},
			{Name: "max_depth", Param: skill.Param{Type: "int", Default: 3, Desc: "目录小计的最大深度, 默认 3, 上限 8"}},
			{Name: "min_size_bytes", Param: skill.Param{Type: "int", Default: 0, Desc: "文件榜的最小字节数, 默认 0"}},
		},
		ResultPreview: "{path, total_bytes, total_files, files_walked, truncated, reason?, directories: [{path, bytes}], largest_files: [{path, bytes}]}",
		// Bounded because the reply is a model context and the walk is the
		// one unbounded thing this package does.
		Limits: domain.ToolLimits{OutputBytes: 262144, TimeoutSeconds: 120},
	}
}

type fileInventoryParams struct {
	Path         string `json:"path"`
	TopN         int    `json:"top_n"`
	MaxDepth     int    `json:"max_depth"`
	MinSizeBytes int64  `json:"min_size_bytes"`
}

type dirTotal struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type fileSize struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type fileInventoryResult struct {
	Path         string     `json:"path"`
	TotalBytes   int64      `json:"total_bytes"`
	TotalFiles   int64      `json:"total_files"`
	FilesWalked  int        `json:"files_walked"`
	Truncated    bool       `json:"truncated"`
	Reason       string     `json:"reason,omitempty"`
	Directories  []dirTotal `json:"directories"`
	LargestFiles []fileSize `json:"largest_files"`
}

func (f FileInventory) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p fileInventoryParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("host_file_inventory: decode params: %w", err)
		}
	}
	if p.Path == "" {
		return nil, fmt.Errorf("host_file_inventory: path is required")
	}
	if !filepath.IsAbs(p.Path) {
		return nil, fmt.Errorf("host_file_inventory: path must be absolute")
	}
	if strings.Contains(p.Path, "..") {
		return nil, fmt.Errorf("host_file_inventory: path must not contain ..")
	}
	if p.TopN <= 0 {
		p.TopN = 20
	}
	if p.TopN > 100 {
		p.TopN = 100
	}
	if p.MaxDepth <= 0 {
		p.MaxDepth = 3
	}
	if p.MaxDepth > maxInventoryDepth {
		p.MaxDepth = maxInventoryDepth
	}
	if p.MinSizeBytes < 0 {
		return nil, fmt.Errorf("host_file_inventory: min_size_bytes must not be negative")
	}

	// The deadline comes from the caller's context, and the walk polls it
	// rather than running it to completion: a tool the operator cancelled has
	// to actually stop, and a walk of a busy network mount is exactly where
	// "eventually" is not good enough.
	ctx, cancel := context.WithTimeout(ctx, inventoryWalkBudget)
	defer cancel()
	return walkInventory(ctx, p)
}

// inventoryWalkBudget is the walk's own ceiling, shorter than the gate's
// timeout_seconds so that the tool returns an answer that says it stopped
// rather than being killed by the broker with nothing to show.
const inventoryWalkBudget = 90 * time.Second

func walkInventory(ctx context.Context, p fileInventoryParams) (json.RawMessage, error) {
	root, err := os.Lstat(p.Path)
	if err != nil {
		return nil, fmt.Errorf("host_file_inventory: %w", err)
	}
	if !root.IsDir() {
		return nil, fmt.Errorf("host_file_inventory: %s is not a directory; "+
			"this tool accounts for a tree, and a single file has no tree", p.Path)
	}
	// The root's device is the boundary. A bind mount or an automount under
	// the path would otherwise pull another filesystem's whole size into this
	// answer, which is both slow and a lie: the bytes are not on the disk
	// being asked about.
	rootDev := deviceOf(root)

	res := fileInventoryResult{
		Path:         p.Path,
		Directories:  []dirTotal{},
		LargestFiles: []fileSize{},
	}
	// direct holds the bytes sitting in a directory itself; children holds
	// the shape of the tree. The two are separate because the walk visits a
	// directory BEFORE its children, so a parent's subtotal cannot be
	// accumulated as the walk goes — it has to be rolled up afterwards,
	// deepest level first.
	type frame struct {
		path   string
		depth  int
		parent string
	}
	stack := []frame{{path: p.Path, depth: 0}}
	direct := map[string]int64{}
	children := map[string][]string{}
	var visited []string

	for len(stack) > 0 {
		if res.FilesWalked%walkPollInterval == 0 {
			if err := ctx.Err(); err != nil {
				res.Truncated = true
				res.Reason = "the walk ran out of time; the totals below are what it saw"
				return finishInventory(res, visited, direct, children, p)
			}
		}
		if res.FilesWalked >= maxWalkEntries {
			res.Truncated = true
			res.Reason = fmt.Sprintf("the walk stopped at %d entries; the totals below are what it saw",
				maxWalkEntries)
			return finishInventory(res, visited, direct, children, p)
		}

		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		// Recorded before the read, so a directory nobody could open is still
		// in the tree with a subtotal of zero and a reason attached — which
		// is a different statement from a directory that was not there.
		visited = append(visited, cur.path)

		entries, err := os.ReadDir(cur.path)
		if err != nil {
			// One unreadable directory is not a failed account. The nodes
			// that cannot be read are the ones nobody can account for, and
			// saying so beats abandoning the other half.
			res.Reason = joinReason(res.Reason, fmt.Sprintf("%s: %v", cur.path, err))
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		var here int64
		for _, e := range entries {
			res.FilesWalked++
			full := filepath.Join(cur.path, e.Name())
			// Lstat, not Stat: a symlink is a NAME for somewhere else, and
			// following it is how a walk ends up in a cycle or counts one
			// filesystem twice.
			fi, err := os.Lstat(full)
			if err != nil {
				res.Reason = joinReason(res.Reason, fmt.Sprintf("%s: %v", full, err))
				continue
			}
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				// Counted by name, not followed, and not counted again
				// where it points.
			case fi.IsDir():
				if deviceOf(fi) != rootDev {
					res.Reason = joinReason(res.Reason,
						fmt.Sprintf("%s: another filesystem, not this disk's", full))
					continue
				}
				stack = append(stack, frame{path: full, depth: cur.depth + 1, parent: cur.path})
				children[cur.path] = append(children[cur.path], full)
			default:
				here += fi.Size()
				res.TotalFiles++
				if fi.Size() >= p.MinSizeBytes {
					res.LargestFiles = append(res.LargestFiles, fileSize{Path: full, Bytes: fi.Size()})
				}
			}
		}
		direct[cur.path] += here
		res.TotalBytes += here
	}
	return finishInventory(res, visited, direct, children, p)
}

// finishInventory rolls each directory's own bytes up through its subtree.
//
// The rollup runs deepest level first, because that is the only order in
// which a parent can be computed: a child's total has to exist before the
// parent adds it, and a walk that visits parents first cannot know it yet.
// Getting this wrong is not visible as a crash — it shows up as a root
// directory that holds 7 bytes while its children hold thirteen thousand,
// which is a number that reads as an answer and is not one.
func finishInventory(res fileInventoryResult, visited []string, direct map[string]int64, children map[string][]string, p fileInventoryParams) (json.RawMessage, error) {
	totals := make(map[string]int64, len(visited))
	depths := make(map[string]int, len(visited))
	for _, path := range visited {
		totals[path] = direct[path]
		depths[path] = depthOf(path, p.Path)
	}
	// Deepest level first. That order is the whole reason this is a separate
	// pass: a child's total must exist before its parent adds it, and the
	// walk visits parents first.
	ordered := append([]string(nil), visited...)
	sort.Slice(ordered, func(i, j int) bool { return depths[ordered[i]] > depths[ordered[j]] })
	for _, path := range ordered {
		for _, child := range children[path] {
			totals[path] += totals[child]
		}
	}
	rolled := make([]dirTotal, 0, len(visited))
	for _, path := range visited {
		if depths[path] <= p.MaxDepth {
			rolled = append(rolled, dirTotal{Path: path, Bytes: totals[path]})
		}
	}
	sort.Slice(rolled, func(i, j int) bool {
		if rolled[i].Bytes != rolled[j].Bytes {
			return rolled[i].Bytes > rolled[j].Bytes
		}
		return rolled[i].Path < rolled[j].Path
	})
	res.Directories = trimDirs(rolled, p.TopN)

	sort.Slice(res.LargestFiles, func(i, j int) bool {
		if res.LargestFiles[i].Bytes != res.LargestFiles[j].Bytes {
			return res.LargestFiles[i].Bytes > res.LargestFiles[j].Bytes
		}
		return res.LargestFiles[i].Path < res.LargestFiles[j].Path
	})
	if len(res.LargestFiles) > p.TopN {
		res.LargestFiles = res.LargestFiles[:p.TopN]
	}
	return json.Marshal(res)
}

func trimDirs(in []dirTotal, topN int) []dirTotal {
	if len(in) > topN {
		return in[:topN]
	}
	return in
}

func depthOf(path, root string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

func deviceOf(fi os.FileInfo) uint64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Without a device the walk cannot enforce the boundary, so it
		// reports everything and says nothing was skipped — which is the
		// honest failure for a platform that does not answer.
		return 0
	}
	return uint64(st.Dev)
}

func joinReason(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
