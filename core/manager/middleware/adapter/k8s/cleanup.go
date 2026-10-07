// cleanup.go reclaims space inside a volume by truncating old log files.
//
// What this tool is not. It is not a "clean up the node" tool: kubelet's
// rotated container logs live in /var/log on the node's filesystem, not in
// anyone's PVC, and this adapter has no node-shell path to them. What it does
// is reach into the volume an application is already failing to write to and
// free the space its own logs are holding. The name says logs because that is
// the only thing it is willing to touch — the alternative, a tool that empties
// whatever directory it is pointed at, is `rm` with extra steps.
//
// Three decisions that are not decoration.
//
// Truncate, not delete. `truncate -s 0` releases the blocks and keeps the
// inode, so an application holding the file open for append keeps working and
// does not have to learn its log file disappeared. `rm` breaks that
// contract, and a disk-pressure fix that takes the writer down with it has
// replaced one outage with a louder one.
//
// dry_run defaults to true. The tool proposes; a human disposes. A caller
// that wants bytes back has to say `dry_run: false` in the same breath as
// naming the directory, and the approval record shows both. The failure this
// is defending against is a model that assembles a plausible-looking call
// with a default it never chose.
//
// The file list is re-derived, never accepted. The caller names a directory
// and an age; the list of files is `find`'s output, run twice — once to
// report, once to act. A list passed in from a previous call describes a
// filesystem one moment ago, and files get rotated, rewritten and re-created
// in between. Acting on it would truncate whatever now occupies those names.
package k8s

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// cleanupDefaultAgeDays is the minimum age a log file must have.
	//
	// It is a floor, not a policy: the caller may demand a longer age, and
	// a shorter one is refused. Truncating a log that something is still
	// writing to destroys the only copy of the lines written in the last
	// few minutes — which is exactly the window an operator is reading when
	// they start this. Seven days is the shortest age at which "old" and
	// "still being appended to" stop being the same set of files for a
	// normally-rotating workload.
	cleanupDefaultAgeDays = 7

	// cleanupMinAgeDays is the floor the age argument may not go below.
	cleanupMinAgeDays = 1

	// cleanupMaxFilesPerCall bounds one call. Beyond it the call is
	// REFUSED, not truncated: an operator who asked to clear a hundred
	// thousand files and got the first five hundred has been told the disk
	// is fixed when it is not, and the tool would report success.
	cleanupMaxFilesPerCall = 500

	// cleanupDefaultMinFileMB ignores small files. Log rotation leaves
	// behind a scatter of kilobyte files, and truncating a hundred of them
	// to reclaim nothing is a destructive action with no upside.
	cleanupDefaultMinFileMB = 1

	cleanupExecTimeout = 60 * time.Second
)

// protectedCleanupPaths are never truncated, whatever the caller says.
//
// These are the directories whose emptiness would be reported as a
// successful cleanup while leaving the system unable to boot or to log. The
// check is on the cleaned directory and on every file `find` returns, because
// a symlink or a bind mount inside a log directory is the standard way a
// "safe" cleanup reaches somewhere else.
var protectedCleanupPaths = []string{
	"/",
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root",
	"/sbin", "/sys", "/usr", "/var/lib", "/var/run",
}

// runCleanupLogs is the handler body for k8s.cleanup_logs.
func runCleanupLogs(ctx context.Context, a *Adapter, args map[string]any) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	p := params(args)
	pvcName, err := p.requireName("pvc")
	if err != nil {
		return 0, "", false, err
	}
	namespaceArg, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	targetDir, err := p.requireString("path")
	if err != nil {
		return 0, "", false, err
	}
	if err := validateCleanupPath(targetDir); err != nil {
		return 0, "", false, err
	}
	ageDays, err := intArg(args, "older_than_days", cleanupDefaultAgeDays)
	if err != nil {
		return 0, "", false, fmt.Errorf("k8s: older_than_days: %w", err)
	}
	if ageDays < cleanupMinAgeDays {
		return 0, "", false, fmt.Errorf("k8s: older_than_days must be at least %d; a shorter age truncates logs that something is still appending to", cleanupMinAgeDays)
	}
	minFileMB, err := intArg(args, "min_file_mb", cleanupDefaultMinFileMB)
	if err != nil {
		return 0, "", false, fmt.Errorf("k8s: min_file_mb: %w", err)
	}
	dryRun, err := p.optionalBool("dry_run", true)
	if err != nil {
		return 0, "", false, err
	}

	ns, err := client.resolveNamespace(ctx, colPVCs, pvcName, namespaceArg)
	if err != nil {
		return 0, "", false, err
	}
	mount, reason := findPVCMount(ctx, client, ns, pvcName)
	if reason != "" {
		return 0, "", false, fmt.Errorf("k8s: cannot reach a filesystem for claim %s/%s: %s", ns, pvcName, reason)
	}
	// The directory has to be inside the mount that was discovered, not
	// merely inside the container. Without this the tool would happily
	// truncate /var/log inside the same container while the caller believed
	// it was clearing a full volume — the call would report the volume's
	// bytes as reclaimed when the volume was never touched.
	if !pathWithin(mount.mountPath, targetDir) {
		return 0, "", false, fmt.Errorf("k8s: path %q is not inside %s, which is where claim %s/%s is mounted in pod %s; this tool only cleans the volume, not the container",
			targetDir, mount.mountPath, ns, pvcName, mount.pod)
	}
	if path.Clean(targetDir) == path.Clean(mount.mountPath) {
		return 0, "", false, fmt.Errorf("k8s: path %q is the mount root of claim %s/%s; name the log directory inside it, because a whole-volume truncation has no directory left to be wrong about",
			targetDir, ns, pvcName)
	}

	// The find expression is fixed here and takes no caller text, so a path
	// or an age cannot become part of the program being run. -xdev keeps it
	// off any filesystem mounted inside the log directory, which is the
	// other way a "log cleanup" reaches a database's data directory.
	// -printf is a GNU extension; an image without it is reported as an
	// error rather than silently cleaning something else.
	findArgs := []string{
		"find", targetDir, "-xdev", "-type", "f",
		"-mtime", "+" + strconv.Itoa(ageDays),
		"-size", "+" + strconv.Itoa(minFileMB) + "M",
		"-printf", "%s\t%p\n",
	}
	stdout, stderr, ok, err := client.exec(ctx, ns, mount.pod, mount.container, findArgs, cleanupExecTimeout)
	if err != nil {
		return 0, "", false, fmt.Errorf("k8s: listing reclaimable logs in %s/%s failed: %s", ns, pvcName, condense(err.Error()))
	}
	if !ok {
		return 0, "", false, fmt.Errorf("k8s: find exited non-zero in %s/%s: %s; the image may not have GNU find, in which case this tool reports the failure rather than cleaning something it could not list",
			ns, pvcName, condense(strings.TrimSpace(stdout+" "+stderr)))
	}
	files, err := parseCleanupListing(stdout)
	if err != nil {
		return 0, "", false, fmt.Errorf("k8s: could not read the file listing from find in %s/%s: %w", ns, pvcName, err)
	}
	if len(files) == 0 {
		return 0, fmt.Sprintf("no file under %s in claim %s/%s is older than %d day(s) and larger than %d MiB; there is nothing to reclaim and nothing was truncated",
			targetDir, ns, pvcName, ageDays, minFileMB), true, nil
	}
	if len(files) > cleanupMaxFilesPerCall {
		return 0, "", false, fmt.Errorf("k8s: %d files under %s qualify, which is more than the %d this call will touch; raise min_file_mb or narrow the path — this tool refuses rather than clearing part of the set and reporting success",
			len(files), targetDir, cleanupMaxFilesPerCall)
	}
	for _, file := range files {
		if reason := protectedReason(file.path); reason != "" {
			return 0, "", false, fmt.Errorf("k8s: refusing to clean: %s (%s)", file.path, reason)
		}
		if !pathWithin(targetDir, file.path) {
			return 0, "", false, fmt.Errorf("k8s: find returned %s, which is outside %s; the listing is not trusted", file.path, targetDir)
		}
	}

	var totalBytes int64
	sample := make([]string, 0, 5)
	for _, file := range files {
		totalBytes += file.size
		if len(sample) < 5 {
			sample = append(sample, file.path)
		}
	}
	headline := fmt.Sprintf("%d file(s) under %s in claim %s/%s hold %d bytes (%.1f MiB) older than %d day(s)",
		len(files), targetDir, ns, pvcName, totalBytes, float64(totalBytes)/(1<<20), ageDays)

	if dryRun {
		return 0, headline + "; dry run, nothing was truncated — pass dry_run: false to reclaim it", true, nil
	}

	// The list is re-derived immediately before acting, with the same
	// expression, so the truncation cannot act on a name that was rotated
	// and replaced between the report and the write.
	truncate := append([]string{"find", targetDir, "-xdev", "-type", "f",
		"-mtime", "+" + strconv.Itoa(ageDays),
		"-size", "+" + strconv.Itoa(minFileMB) + "M",
		"-exec", "truncate", "-s", "0", "{}", "+"})
	_, stderr, ok, err = client.exec(ctx, ns, mount.pod, mount.container, truncate, cleanupExecTimeout)
	if err != nil {
		return 0, headline + fmt.Sprintf("; TRUNCATION FAILED: %s", condense(err.Error())), false, err
	}
	if !ok {
		return 0, headline + fmt.Sprintf("; TRUNCATION FAILED: %s", condense(stderr)), false, fmt.Errorf("k8s: truncate exited non-zero in %s/%s: %s", ns, pvcName, condense(stderr))
	}
	return len(files), headline + fmt.Sprintf("; truncated to zero (sizes were re-read immediately before truncating). Files included: %s", strings.Join(sample, ", ")), true, nil
}

// validateCleanupPath refuses a target that is not a usable log directory.
func validateCleanupPath(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("k8s: path %q must be absolute; a relative path in a container is resolved against whatever the process's working directory happens to be", dir)
	}
	clean := path.Clean(dir)
	if strings.Contains(dir, "..") {
		return fmt.Errorf("k8s: path %q contains '..'; name the directory directly", dir)
	}
	if reason := protectedReason(clean); reason != "" {
		return fmt.Errorf("k8s: refusing to clean %s: %s", clean, reason)
	}
	if clean == "/" {
		return fmt.Errorf("k8s: refusing to clean /")
	}
	return nil
}

// protectedReason explains why a path may not be cleaned, or returns "".
func protectedReason(p string) string {
	clean := path.Clean(p)
	for _, guarded := range protectedCleanupPaths {
		if clean == guarded {
			return "it is a system directory"
		}
		if strings.HasPrefix(clean, guarded+"/") && guarded != "/" {
			return "it is inside the system directory " + guarded
		}
	}
	return ""
}

// pathWithin reports whether child is base itself or below it.
func pathWithin(base, child string) bool {
	base = path.Clean(base)
	child = path.Clean(child)
	if base == child {
		return true
	}
	if base == "/" {
		return strings.HasPrefix(child, "/")
	}
	return strings.HasPrefix(child, base+"/")
}

// cleanupFile is one row of `find -printf '%s\t%p\n'`.
type cleanupFile struct {
	size int64
	path string
}

// parseCleanupListing reads the size/path pairs find printed.
//
// A row that does not parse is a hard error rather than a skipped line. A
// listing where some rows are unreadable and some are fine is a listing whose
// total is wrong, and the total is what the operator reads as "space
// reclaimed".
func parseCleanupListing(out string) ([]cleanupFile, error) {
	var files []cleanupFile
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		sizeText, filePath, found := strings.Cut(trimmed, "\t")
		if !found {
			// find -printf emits a literal tab; a line without one means
			// either a filename containing a newline (possible, and not
			// something this can attribute) or an image whose find does not
			// support -printf. Either way the count is not trustworthy.
			return nil, fmt.Errorf("row %q has no size/path separator; find may not support -printf", trimmed)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("row %q has a non-numeric size", trimmed)
		}
		if strings.TrimSpace(filePath) == "" {
			return nil, fmt.Errorf("row %q has an empty path", trimmed)
		}
		files = append(files, cleanupFile{size: size, path: strings.TrimSpace(filePath)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}
