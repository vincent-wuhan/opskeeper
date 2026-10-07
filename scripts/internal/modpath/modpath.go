// Package modpath answers one question: given a directory holding Go files,
// which module contains it, and what is that module's path?
//
// Two tools need it and must not answer it differently. scripts/deadcode
// attributes a reference to the package it reaches, which means turning a
// directory into the import path other files would write; scripts/deadpkg
// needs the same mapping to say which packages nothing imports. When the two
// disagreed, one of them would report a package as dead that the other
// reported as live, and the only way to settle that would be to read the
// import blocks by hand.
//
// The walk stops at the nearest go.mod. This repository has 14 modules and
// some of them are nested, so the nearest one wins: core/manager/go.mod is
// the answer for everything under core/manager, regardless of what the
// repository root declares.
package modpath

import (
	"os"
	"path/filepath"
	"strings"
)

// Answer is one directory's module: the module path, the module root
// directory, and whether a module was found at all.
type Answer struct {
	Path string
	Root string
	OK   bool
}

// Of returns the module containing dir, walking up to the nearest go.mod.
func Of(dir string) Answer {
	return of(dir, map[string]Answer{})
}

func of(dir string, cache map[string]Answer) Answer {
	if hit, seen := cache[dir]; seen {
		return hit
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if mpath, ok := moduleLine(string(raw)); ok {
			ans := Answer{Path: mpath, Root: dir, OK: true}
			cache[dir] = ans
			return ans
		}
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		inherited := of(parent, cache)
		cache[dir] = inherited
		return inherited
	}
	ans := Answer{}
	cache[dir] = ans
	return ans
}

// moduleLine reads the `module <path>` line. It returns false for a go.mod
// with no module line, which is a malformed module file rather than a
// directory that is definitely outside every module — the caller inherits
// from the parent in that case, which is the safer direction: an unmapped
// import makes a report tool cautious, not confident.
func moduleLine(raw string) (string, bool) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			mpath := strings.TrimSpace(rest)
			if mpath != "" {
				return mpath, true
			}
		}
	}
	return "", false
}
