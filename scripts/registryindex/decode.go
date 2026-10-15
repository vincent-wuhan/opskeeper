package main

import (
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// decodeStrict parses a manifest with the same known-fields strictness the
// host applies to a manifest it is handed.
//
// It is a three-line function that exists so the producer and the reader
// cannot drift: both call sdk.Decode, so a key added to the manifest schema
// is accepted by both on the same day, and a key that neither knows is
// rejected by both. A producer that used a lenient parser would happily
// publish a package that no node could install, and the symptom would
// appear on a node rather than in a build log.
func decodeStrict(raw []byte) (domain.PluginManifest, error) {
	return sdk.Decode(raw)
}

// joinURL appends a package path to a base URL without doubling the slash.
//
// strings.TrimSuffix is used rather than path.Join because a base URL may
// carry a path prefix ("https://host/registry/") that must be preserved;
// path.Join would eat it as though it were a filesystem root.
func joinURL(base, name string) string {
	return strings.TrimSuffix(base, "/") + "/" + name
}
