package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// read_test.ts holds the tool's rules; running it from here keeps it part of
// `go test ./...`.
func TestReadTS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the extension runs under Pi's own runtime in production")
	}
	cmd := exec.Command(node, "read_test.ts")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read_test.ts failed: %v\n%s", err, out)
	}
}

// The extension imports the bundle by relative path, so the two have to land
// side by side under the names read.ts expects.
func TestWritePutsTheBundleBesideTheExtension(t *testing.T) {
	dir := t.TempDir()
	path, err := Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, ExtensionFile) {
		t.Fatalf("path = %q", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `from "./`+bundleFile+`"`) || !strings.Contains(string(body), `name: "web_read"`) {
		t.Fatalf("%s is not the web_read extension", path)
	}
	if _, err := os.Stat(filepath.Join(dir, bundleFile)); err != nil {
		t.Fatal("the bundle was not written:", err)
	}
}
