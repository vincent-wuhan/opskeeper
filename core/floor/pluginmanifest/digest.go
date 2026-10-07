package pluginmanifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// The content address a signature is computed over.
//
// A signature is only as good as the thing it signs, and the thing being
// signed here is a directory of files the agent will build and run. Three
// properties are therefore load-bearing, and each of them is a way a
// naive implementation gets attacked:
//
//  1. Unambiguous framing. Every field written into the hash is
//     length-prefixed. Without that, a file named "ab" with contents "c"
//     and a file named "a" with contents "bc" hash identically, and a
//     signature over one of them verifies over the other. Length prefixes
//     cost eight bytes a field and remove the entire class.
//
//  2. The file set, in a defined order. A hash over whatever the
//     filesystem happened to return is not a stable input; the digest has
//     to be the same on the machine that signs and the machine that
//     verifies, and filesystem walk order is not guaranteed to be.
//
//  3. Refusal to hash something that is not a regular file. A symlink in
//     a plugin tree is a pointer to somewhere else, and what it points at
//     can differ between the signing machine and the node. A package that
//     ships one has either made a mistake or made an argument, and neither
//     should be resolved by a hashing function.
//
// The one file not covered is the signature sidecar itself, which is what
// makes the signature detached; see SignatureFile.

// treeDigestPrefix domain-separates this hash from any other sha256 in the
// system, so a digest of a plugin tree can never be replayed as a digest
// of something else.
const treeDigestPrefix = "opskeeper.io/plugin-tree/v1\n"

// TreeDigest returns the hex-encoded sha256 of a plugin directory's
// contents.
//
// The digest covers every regular file under root, its path relative to
// root in slash form, whether it is executable, its size, and its bytes.
// The governance manifest is one of those files, which is what makes a
// signature over the tree a signature over the manifest as well: a
// manifest edited after signing does not verify, so `spec.tools` cannot be
// widened underneath a signature.
func TreeDigest(root string) (string, error) {
	entries, err := collectTree(root)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("%s: the package is empty, so there is nothing to sign", root)
	}

	h := sha256.New()
	_, _ = io.WriteString(h, treeDigestPrefix)

	// The count goes in first so that removing a file cannot produce the
	// same digest as renaming one: with only the per-file framing, dropping
	// the last entry is indistinguishable from a stream that ended there.
	writeField(h, fmt.Sprintf("%d", len(entries)))

	for _, e := range entries {
		writeField(h, e.relPath)
		writeField(h, e.mode)
		writeField(h, fmt.Sprintf("%d", e.size))
		h.Write(e.content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// treeEntry is one regular file, already read.
type treeEntry struct {
	relPath string
	// mode is "x" for an executable file and "-" for anything else. Only
	// the executable bit is recorded: it is the one mode change that alters
	// what a build does, and recording the umask-derived bits would make
	// the digest depend on the machine that extracted the package.
	mode    string
	size    int64
	content []byte
}

// collectTree walks root and returns its regular files, sorted by path.
func collectTree(root string) ([]treeEntry, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("read the package root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	var out []treeEntry
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		// The signature sidecar is excluded, and that is what makes a
		// detached signature detached. Including it would mean signing
		// produced a digest that the signature file itself invalidated,
		// so a signed package could never be verified and every re-sign
		// would be required. The exclusion is exact — by name, at the
		// package root — so a nested file of the same name is still
		// covered.
		if rel == SignatureFile {
			return nil
		}

		// A symlink is refused rather than followed. Following it would
		// make the digest a statement about a file that is not in the
		// package, and refusing it means the question of what it pointed
		// at never becomes the answer.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: the package contains a symlink; a package must ship only regular files", rel)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: the package contains a non-regular file (%s)", rel, d.Type())
		}

		fi, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		mode := "-"
		if fi.Mode().Perm()&0o111 != 0 {
			mode = "x"
		}
		out = append(out, treeEntry{
			relPath: rel,
			mode:    mode,
			size:    fi.Size(),
			content: content,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk the package: %w", walkErr)
	}

	// Sorted by the slash path, which is the same string that goes into
	// the hash. Sorting on filepath's native separator would order the
	// same tree differently on two systems that disagree about it.
	sort.Slice(out, func(i, j int) bool { return out[i].relPath < out[j].relPath })
	return out, nil
}

// writeField length-prefixes one field into the running hash.
//
// The 8-byte big-endian length comes first, so a reader — or an attacker
// trying to find a collision — cannot slide a field boundary.
func writeField(w io.Writer, s string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(s)))
	_, _ = w.Write(length[:])
	_, _ = io.WriteString(w, s)
}

// FileDigest returns the hex sha256 of one file, for callers that want to
// display what was signed.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
