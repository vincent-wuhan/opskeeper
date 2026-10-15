// Command registryindex turns this repository's shipped packages into the
// index document a registry serves.
//
// Why the repository publishes one. Until now the phrase "the remote
// registry" named something this tree could read but never produce, so the
// only honest test of the reader was one written by hand — which is the same
// defect decision 462 spent a knife on, one level up: a path that exists but
// that nothing on the tree exercises. If the repository can emit the
// document, then the reader can be tested against a real one, and the
// question "does the catalogue agree with the manifests" has an answer that
// is recomputed rather than remembered.
//
// What it deliberately does not do:
//
//   - It does not upload anything. This writes a file. Where that file is
//     served from is an operator's decision, and a build step that reaches
//     out to a registry is a build step whose failure says nothing about
//     this tree.
//
//   - It does not invent a URL. A row with no --base-url gets an empty URL,
//     and ParseIndex accepts that (the field is optional) while install
//     refuses it. A registry that published a URL it cannot serve would be
//     worse than one that publishes none.
//
//   - It does not read the version from the directory name or from a
//     sidecar. The manifest is the single source, and the index row's
//     version is copied out of it — so a row that disagrees with its
//     manifest cannot be produced, only hand-written.
//
// Usage:
//
//	go run ./scripts/registryindex [-registry NAME] [-base-url URL] [-out FILE] [packages-root]
//
// With no -out the document goes to stdout, which is what a CI job that
// pipes it into an object store wants.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// manifestFile is the per-package governance manifest. It is the same file
// the node reads, which is the entire point: an index built from a
// re-serialised copy would be an index of something nobody installs.
const manifestFile = "pig-ops.yaml"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "registryindex:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		registry = flag.String("registry", "opskeeper-official", "the registry name this document declares")
		baseURL  = flag.String("base-url", "", "prefix prepended to each package path to form its URL")
		out      = flag.String("out", "", "write here instead of stdout")
	)
	flag.Parse()

	root := "plugins/pig-ops"
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	idx, err := build(*registry, *baseURL, root)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the index: %w", err)
	}
	data = append(data, '\n')

	if *out == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", *out, err)
	}
	return nil
}

// build reads every package under root and projects it onto the index.
//
// Sorting by name is not cosmetic: the document is a published artefact and
// two runs over an unchanged tree must produce identical bytes, or every
// registry consumer sees a change that did not happen.
func build(registry, baseURL, root string) (pluginmanifest.Index, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return pluginmanifest.Index{}, fmt.Errorf("reading %s: %w", root, err)
	}

	idx := pluginmanifest.Index{
		APIVersion: pluginmanifest.IndexAPIVersion,
		Kind:       pluginmanifest.IndexKind,
		Registry:   registry,
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	for _, name := range dirs {
		pkgDir := filepath.Join(root, name)
		manifestPath := filepath.Join(pkgDir, manifestFile)

		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return pluginmanifest.Index{}, fmt.Errorf("%s: %w", name, err)
		}
		// Decoded here for the same reason ParseIndex decodes: the producer
		// refuses to emit a row for a manifest this build would refuse to
		// install. A registry that publishes a broken package has moved the
		// failure to the node instead of removing it.
		m, err := decodeStrict(raw)
		if err != nil {
			return pluginmanifest.Index{}, fmt.Errorf("%s: %w", name, err)
		}

		// The directory name and the manifest name are two claims about the
		// same package. Disagreement is refused rather than resolved: one of
		// the two is a typo, and picking a winner would publish whichever
		// one this script happened to read first.
		if m.Metadata.Name != name {
			return pluginmanifest.Index{}, fmt.Errorf("%s: the directory is named %q but the manifest declares %q",
				name, name, m.Metadata.Name)
		}

		digest, err := pluginmanifest.TreeDigest(pkgDir)
		if err != nil {
			return pluginmanifest.Index{}, fmt.Errorf("%s: %w", name, err)
		}

		item := pluginmanifest.IndexItem{
			Name:         m.Metadata.Name,
			Version:      m.Metadata.Version,
			SHA256:       digest,
			ManifestYAML: string(raw),
		}
		if baseURL != "" {
			item.URL = joinURL(baseURL, name)
		}
		idx.Items = append(idx.Items, item)
	}

	if len(idx.Items) == 0 {
		return pluginmanifest.Index{}, fmt.Errorf("%s: no packages found, so the index would claim an "+
			"empty catalogue is a fact about the registry", root)
	}
	return idx, nil
}
