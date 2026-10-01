// Package web ships the web_read Pi extension. read.ts and the defuddle bundle
// it imports are compiled in and written beside the guard once per run, so no
// machine needs a Pi package installed and the target repo cannot supply its own.
package web

import (
	_ "embed"
	"os"
	"path/filepath"
)

//go:generate node build.mjs

// ExtensionFile is the name Write gives read.ts in the run directory; the
// bundle sits next to it under the name read.ts imports.
const (
	ExtensionFile = "read.ts"
	bundleFile    = "defuddle.mjs"
)

//go:embed read.ts
var extension []byte

//go:embed defuddle.mjs
var bundle []byte

// Write puts the extension and its bundle into dir and returns the extension's
// path.
func Write(dir string) (string, error) {
	if err := os.WriteFile(filepath.Join(dir, bundleFile), bundle, 0o600); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ExtensionFile)
	return path, os.WriteFile(path, extension, 0o600)
}
