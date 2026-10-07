package container

import (
	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The port is satisfied or the tree does not build. That is the whole guard,
// and it is the same guard presence.go carries for the edge ports: a method
// nothing in another package asks for by name is a method a later reader sees
// no caller for.
var _ domain.ContainerLoader = ContainerLoader{}

// ContainerLoader adapts this package's container loader to the port the
// importer holds.
//
// It is a zero-size struct with one method and no state, and that is
// deliberate: the loader it wraps is a package of pure functions over a
// directory, so there is nothing for an instance to remember. It exists as a
// named type only so the composition root has something to hand over and so
// the port has a visible implementor.
type ContainerLoader struct{}

// LoadContainer implements domain.ContainerLoader.
//
// The one thing worth reading here is that it calls detectContainerForLoad
// once and passes the result into loadPluginContainer. The importer used to
// call LoadPluginContainer and then DetectContainer, which meant the directory
// was probed twice and the `kind` written into the conversion report came from
// the second reading rather than the one that produced the pack. If the
// directory changed in between, the report described a container the importer
// had not loaded (decision 252).
//
// The four identity fields are forwarded raw. This package is the loader and
// it is not the converter: falling back to the directory name when there is
// no id, and to "0.0.0" when there is no version, are decisions the importer
// makes about the package it is about to write, and they belong on that side
// of the port.
func (ContainerLoader) LoadContainer(dir string) (domain.ContainerSource, error) {
	kind, manifestPath, err := detectContainerForLoad(dir)
	if err != nil {
		return domain.ContainerSource{}, err
	}
	result, err := loadPluginContainer(dir, kind, manifestPath)
	if err != nil {
		return domain.ContainerSource{}, err
	}
	out := domain.ContainerSource{
		Kind:     kind,
		Warnings: result.Warnings,
	}
	if result.Pack != nil {
		out.ID = result.Pack.ID
		out.DisplayName = result.Pack.DisplayName
		out.Version = result.Pack.Version
		out.Description = result.Pack.Description
	}
	return out, nil
}
