package permit

import "testing"

// The shipped deny list, so the test is about the patterns lathe actually runs
// with rather than fixtures that could drift away from them.
var shipped = []string{".git/", ".env*", "*.pem", "*.key"}

func TestDenied(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".git/config", true},
		{"vendor/x/.git/HEAD", true}, // a directory name, at any depth
		{".env", true},
		{".env.production", true},
		{"config/.env.local", true},
		{"certs/server.pem", true},
		{"deploy/id.key", true},
		{"internal/run/run.go", false},
		{"gitignore.md", false},   // ".git/" is a segment, not a prefix
		{"env.go", false},         // ".env*" is anchored at the dot
		{"keys/README.md", false}, // "*.key" is the extension, not the directory
	} {
		if _, got := Denied(tc.path, shipped); got != tc.want {
			t.Errorf("Denied(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
	if pattern, _ := Denied(".env", shipped); pattern != ".env*" {
		t.Errorf("pattern = %q; want the rule that matched, for the agent to read", pattern)
	}
}

func TestEscapes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"internal/run/run.go", false},
		{"./main.go", false},
		{"a/../main.go", false}, // lands back inside
		{"..", true},
		{"../outside.go", true},
		{"a/../../outside.go", true},
		{"/etc/passwd", true},
	} {
		if got := Escapes(tc.path); got != tc.want {
			t.Errorf("Escapes(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}
