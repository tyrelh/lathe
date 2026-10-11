package main

import (
	"embed"
	"io/fs"
)

// assets holds the agent configuration seed, laid out as it is on disk under
// ~/.lathe. It is only ever copied there; capture reads the copy.
//
//go:embed seed
var assets embed.FS

// Seed is the embedded seed rooted at its top directory.
var Seed = mustSub(assets, "seed")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
