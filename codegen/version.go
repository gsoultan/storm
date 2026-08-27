package codegen

import (
	"runtime/debug"
	"sync"
)

// Version is the storm that generated a file, stamped into every header.
//
// The question it answers is the one a support conversation opens with —
// "which storm wrote this?" — and which nothing in the file could answer
// before. `storm verify -stale` already catches real skew, because it
// regenerates with the new binary and compares bytes; this is the forensic
// half, readable without running anything.
//
// It costs an upgrade its regeneration: bump storm, and every generated
// header differs until you regenerate, which `verify -stale` will tell you
// about. That is the same bargain sqlc and protoc-gen-go make, and it is the
// right one — a generated file that does not say what made it is a file
// nobody can reason about a year later.
func Version() string { return versionOnce() }

var versionOnce = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	const mod = "github.com/gsoultan/storm"
	// Imported as a dependency: the version is pinned in the consumer's
	// go.mod, which is exactly what should be stamped.
	for _, d := range info.Deps {
		if d.Path == mod {
			if d.Replace != nil {
				// A filesystem replace has no version worth pretending
				// about. Say so rather than stamping the replaced module's
				// empty string.
				return "(replaced)"
			}
			if d.Version != "" {
				return d.Version
			}
		}
	}
	// Running from storm's own tree (go run ./cmd/storm, or the test suite),
	// where Go reports "(devel)" for an untagged main module.
	if info.Main.Path == mod && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
})
