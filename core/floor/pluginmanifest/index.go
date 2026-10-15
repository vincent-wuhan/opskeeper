package pluginmanifest

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The remote index: a registry's own list of what it offers.
//
// Why this exists. The catalog answers "what can this tenant install", and
// until now it could only answer it from directories on this machine. A
// package that exists in a registry and has not been installed here was
// therefore invisible, which is the same shape of wrong answer the
// single-root index had (decision 459) one level further out: the route
// claimed a question it was answering a narrower one.
//
// Why the manifest travels as bytes. An index entry carries the exact
// contents of the package's pig-ops.yaml, not a re-serialised object. Two
// reasons, and the second is the one that matters:
//
//   - there is then no second parser. The index is decoded with the same
//     known-fields strictness a node applies to a manifest it is handed, so
//     a key this repository does not know is an error here exactly as it
//     would be there;
//   - the index cannot disagree with the manifest the node will eventually
//     read, because it is those bytes rather than a projection of them.
//
// Why the listing repeats name and version. The listing is what an operator
// reads; the manifest is what the node enforces. An index whose listing said
// one thing and whose manifest said another would be a catalogue that
// misinforms the only reader who cannot check. ParseIndex refuses the
// disagreement rather than picking a winner, because there is no
// defensible one: one of the two is wrong and the index does not know which.

// IndexAPIVersion and IndexKind identify the document.
//
// They are separate constants from the manifest's own, and deliberately so:
// a manifest is a package's declaration and an index is a registry's
// catalogue, and a reader that accepted either shape under the other's
// constants would be accepting a document whose kind nobody checked.
const (
	IndexAPIVersion = "opskeeper.io/v1"
	IndexKind       = "PluginIndex"
)

// Index is one registry's list of offerings.
type Index struct {
	// APIVersion and Kind are checked on read.
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Registry names the source these offerings come from. It is a label
	// an operator reads, and Install checks it against the allowlist, so a
	// registry cannot offer itself under somebody else's name — but note
	// that the label is checked at install, not here: an index is a public
	// document and refusing to *read* one because its label is unusual
	// would make an unknown-but-harmless registry invisible rather than
	// merely un-installable.
	Registry string `json:"registry"`
	// Items are the offerings, in whatever order the registry chose. Nothing
	// downstream depends on the order, and Entries sorts anyway.
	Items []IndexItem `json:"items"`
}

// IndexItem is one row: a listing and the manifest behind it.
type IndexItem struct {
	// Name and Version must equal what Manifest declares. See the note above.
	Name    string `json:"name"`
	Version string `json:"version"`
	// URL is where the package itself can be fetched. It is not fetched by
	// this package: resolving and checking it is the install path's job, and
	// a reader that fetched here would turn a listing request into a
	// download.
	URL string `json:"url,omitempty"`
	// SHA256 is the package's tree digest, as produced by TreeDigest on the
	// directory that holds it. Empty is allowed and means "the registry did
	// not say", which the install path treats as "no digest to check
	// against" rather than as "digest verified" — the two are very
	// different things to be able to say afterwards, and only one of them
	// is true.
	SHA256 string `json:"sha256,omitempty"`
	// ManifestYAML is the package's pig-ops.yaml, byte for byte.
	ManifestYAML string `json:"manifest_yaml"`
}

// ParseIndex decodes and validates a registry's index.
//
// Every item's manifest goes through the same Decode a node applies, so an
// index cannot offer a package the host would refuse on install. A single
// bad item fails the whole document: an index that silently dropped the rows
// it could not read would be a catalogue that quietly omits packages, and an
// operator cannot act on an omission they were never shown.
func ParseIndex(data []byte) (Index, error) {
	var idx Index
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&idx); err != nil {
		return Index{}, fmt.Errorf("plugin index is not valid: %w", err)
	}
	if idx.APIVersion != IndexAPIVersion {
		return Index{}, fmt.Errorf("plugin index: apiVersion = %q, want %q", idx.APIVersion, IndexAPIVersion)
	}
	if idx.Kind != IndexKind {
		return Index{}, fmt.Errorf("plugin index: kind = %q, want %q", idx.Kind, IndexKind)
	}
	for i, item := range idx.Items {
		m, err := sdk.Decode([]byte(item.ManifestYAML))
		if err != nil {
			return Index{}, fmt.Errorf("plugin index: item %d (%s): the manifest is not admissible: %w",
				i, item.Name, err)
		}
		if m.Metadata.Name != item.Name {
			return Index{}, fmt.Errorf("plugin index: item %d lists %q but its manifest declares %q; "+
				"a listing that disagrees with the manifest misinforms the reader who cannot check it",
				i, item.Name, m.Metadata.Name)
		}
		if m.Metadata.Version != item.Version {
			return Index{}, fmt.Errorf("plugin index: item %d (%s) lists version %q but its manifest "+
				"declares %q", i, item.Name, item.Version, m.Metadata.Version)
		}
	}
	return idx, nil
}

// Plugins projects the index onto the catalog's own type.
//
// The Root is the item's URL: a catalog row that has not been fetched has no
// directory, and a Plugin with an empty Root would be a package the host
// believes it has on disk. Nothing in the catalog path reads it, and the
// install path re-reads the real manifest from the real directory — but a
// value that means "nowhere" should say so rather than point at a URL a
// reader might mistake for a path.
func (idx Index) Plugins() []Plugin {
	out := make([]Plugin, 0, len(idx.Items))
	for _, item := range idx.Items {
		m, err := sdk.Decode([]byte(item.ManifestYAML))
		if err != nil {
			// ParseIndex already refused this document. A decode failure
			// here is therefore unreachable, and skipping is still the
			// right answer over panicking: this function may be handed a
			// hand-built Index by a producer, and a producer's bad manifest
			// is a skip with a name, not a crash in a listing path.
			continue
		}
		out = append(out, Plugin{
			Root:     "",
			Origin:   OriginRegistry,
			Manifest: m,
		})
	}
	return out
}
