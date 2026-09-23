package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssueReference(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"41", "41"}, {" #41 ", "41"},
		{"tyrelh/lathe#41", "https://github.com/tyrelh/lathe/issues/41"},
		{"https://github.com/tyrelh/lathe/issues/41/", "https://github.com/tyrelh/lathe/issues/41"},
		{"https://git.example.com/team/repo/issues/2", "https://git.example.com/team/repo/issues/2"},
		{"", ""}, {"0", ""}, {"-1", ""}, {"--web", ""}, {"team/repo#0", ""},
		{"https://github.com/team/repo/pull/41", ""}, {"41; echo bad", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := IssueReference(tc.input)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFetchIssue(t *testing.T) {
	for _, tc := range []struct{ name, script, wantErr string }{
		{"success", `printf '%s' '{"number":41,"title":"Add retry","body":"Keep existing behavior.\nThen retry.","url":"https://github.com/team/repo/issues/41"}'`, ""},
		{"empty body", `printf '%s' '{"number":41,"title":"Add retry","body":"","url":"https://github.com/team/repo/issues/41"}'`, ""},
		{"auth failure", "echo 'gh auth login required' >&2; exit 1", "gh auth login required"},
		{"malformed", "echo nope", "decode gh issue view"},
		{"incomplete", "echo '{}'", "incomplete issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\" > invocation\n" + tc.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			got, err := FetchIssue(context.Background(), dir, "team/repo#41")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Request(), "GitHub issue #41: Add retry") || !strings.Contains(got.Request(), got.Body) {
				t.Fatalf("request = %q", got.Request())
			}
			invocation, err := os.ReadFile(filepath.Join(dir, "invocation"))
			if err != nil {
				t.Fatal(err)
			}
			want := dir + "\nissue\nview\nhttps://github.com/team/repo/issues/41\n--json\nnumber,title,body,url\n"
			if string(invocation) != want {
				t.Fatalf("invocation = %q; want %q", invocation, want)
			}
		})
	}
}

func TestFetchIssueDeadlineAndMissingGH(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if _, err := FetchIssue(context.Background(), dir, "41"); err == nil || !strings.Contains(err.Error(), "gh issue view") {
		t.Fatalf("missing gh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := FetchIssue(ctx, dir, "41"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}
