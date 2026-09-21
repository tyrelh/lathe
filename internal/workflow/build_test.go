package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
)

func gitBuild(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return string(b)
}

func buildRepo(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg, repo := load(t)
	gitBuild(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "hello.txt"), "hello\n")
	gitBuild(t, repo, "add", ".")
	gitBuild(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "initial")
	return cfg, repo
}

// Exercise the production CLI lookup, prompt handoff, scopes and enforcement.
// The stub can bypass the extension, so out-of-scope writes test defense in depth.
func buildStub(t *testing.T, action, report string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "test"), piReply(t, "```json\n"+`{"summary":"discovered","command":"true","failures":[],"artifacts":[]}`+"\n```"))
	write(t, filepath.Join(dir, "plan"), planReply(t, `"hello.txt"`))
	write(t, filepath.Join(dir, "build"), piReply(t, "```json\n"+report+"\n```"))
	script := fmt.Sprintf(`#!/bin/sh
n=0
[ ! -f %[1]q/count ] || n=$(cat %[1]q/count)
echo $((n+1)) > %[1]q/count
printf '%%s\n' "$@" > %[1]q/args$n
cp "$LATHE_PERMIT" %[1]q/scope$n
if [ "$n" = 0 ]; then
  cat %[1]q/plan
elif [ "$n" = 2 ] && [ -f %[1]q/success ]; then
  cat %[1]q/test
else
  %[2]s
  cat %[1]q/build
fi
`, dir, action)
	onPath(t, dir, script)
	return dir
}

func TestBuild(t *testing.T) {
	for _, tc := range []struct {
		name, action, changed, needed string
		want                          int
		calls                         string
	}{
		{"success", "printf 'hello world\\n' > hello.txt", `["hello.txt"]`, `[]`, 0, "3"},
		{"needed is terminal", "printf 'hello world\\n' > hello.txt", `["hello.txt"]`, `["missing.txt"]`, 1, "2"},
		{"unplanned write reverted", "printf 'hello world\\n' > hello.txt; echo bad > outside.txt", `["hello.txt"]`, `[]`, 1, "2"},
		{"false claim", ":", `["hello.txt"]`, `[]`, 1, "4"},
		{"omitted change", "echo changed > hello.txt", `[]`, `[]`, 1, "4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo := buildRepo(t)
			report := fmt.Sprintf(`{"summary":"updated greeting","changed":%s,"needed":%s,"artifacts":[]}`, tc.changed, tc.needed)
			stub := buildStub(t, tc.action, report)
			if tc.want == 0 {
				write(t, filepath.Join(stub, "success"), "")
			}
			if got := execute(t, cfg, "build", repo, "greet the world"); got != tc.want {
				t.Fatalf("code = %d, want %d", got, tc.want)
			}
			count, _ := os.ReadFile(filepath.Join(stub, "count"))
			if strings.TrimSpace(string(count)) != tc.calls {
				t.Fatalf("spawn count = %s", count)
			}
			if _, err := os.Stat(filepath.Join(repo, "outside.txt")); !os.IsNotExist(err) {
				t.Fatalf("out-of-scope file survived: %v", err)
			}
			dir := latestRunDir(t)
			if _, err := os.Stat(filepath.Join(dir, "plan.json")); err != nil {
				t.Fatal(err)
			}
			args, _ := os.ReadFile(filepath.Join(stub, "args1"))
			for _, want := range []string{"greet the world", "edit fetch.go", "hello.txt", "read,grep,find,ls,write,edit", "--no-extensions"} {
				if !strings.Contains(string(args), want) {
					t.Errorf("builder arguments missing %q: %s", want, args)
				}
			}
			for n, want := range []string{`"allow":[]`, `"allow":["hello.txt"]`} {
				scope, _ := os.ReadFile(filepath.Join(stub, fmt.Sprintf("scope%d", n)))
				if !strings.Contains(string(scope), want) {
					t.Errorf("scope = %s; want %s", scope, want)
				}
			}
			db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := db.Recent(1)
			if err != nil || len(rows) != 1 {
				t.Fatalf("runs: %v %v", rows, err)
			}
			status := "ok"
			if tc.want != 0 {
				status = "fail"
			}
			if rows[0].Status != status {
				t.Fatalf("status = %s", rows[0].Status)
			}
			if tc.want == 0 || tc.needed != `[]` {
				b, err := os.ReadFile(filepath.Join(dir, "build.json"))
				if err != nil {
					t.Fatal(err)
				}
				var out BuildOutput
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatal(err)
				}
				if got := gitBuild(t, repo, "diff", "--", "hello.txt"); !strings.Contains(got, "+hello world") {
					t.Fatalf("missing uncommitted diff: %s", got)
				}
				if got := gitBuild(t, repo, "diff", "--cached"); got != "" {
					t.Fatalf("staged changes: %s", got)
				}
			}
		})
	}
}

func TestBuildDirtyRepoDoesNotSpawn(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := buildStub(t, ":", `{}`)
	write(t, filepath.Join(repo, "hello.txt"), "my work\n")
	if code := execute(t, cfg, "build", repo, "change greeting"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(stub, "count")); !os.IsNotExist(err) {
		t.Fatal("spawned on a dirty repo")
	}
	b, _ := os.ReadFile(filepath.Join(repo, "hello.txt"))
	if string(b) != "my work\n" {
		t.Fatal("changed user work")
	}
}

func TestChangesMatchClaim(t *testing.T) {
	_, repo := buildRepo(t)
	gitBuild(t, repo, "mv", "hello.txt", "new name.txt")
	write(t, filepath.Join(repo, "new\nfile.txt"), "new\n")
	r := &run.Run{Repo: repo}
	paths := []string{"hello.txt", "./new name.txt", "new\nfile.txt"}
	if v := ChangesMatchClaim(&BuildOutput{Changed: &paths}, r); len(v) != 0 {
		t.Fatalf("rename/newline paths: %v", v)
	}
	paths = []string{"invented.txt"}
	if v := ChangesMatchClaim(&BuildOutput{Changed: &paths}, r); len(v) != 4 {
		t.Fatalf("bidirectional mismatches: %v", v)
	}
	if v := ChangesMatchClaim(&BuildOutput{Changed: &paths}, &run.Run{Repo: t.TempDir()}); len(v) != 1 {
		t.Fatalf("git failure: %v", v)
	}
}

func TestBuildOutputRequiresEveryKey(t *testing.T) {
	if v := (&BuildOutput{}).Validate(); len(v) != 4 {
		t.Fatalf("violations: %v", v)
	}
	summary, empty := "done", []string{}
	b := &BuildOutput{Summary: &summary, Changed: &empty, Needed: &empty, Wrote: &empty}
	if v := b.Validate(); len(v) != 0 {
		t.Fatalf("empty lists: %v", v)
	}
}

func TestBuildBlockedWriteTrace(t *testing.T) {
	cfg, repo := buildRepo(t)
	dir := buildStub(t, ":", `{"summary":"blocked","changed":[],"needed":["forbidden.txt"],"artifacts":[]}`)
	replyPath := filepath.Join(dir, "build")
	body, err := os.ReadFile(replyPath)
	if err != nil {
		t.Fatal(err)
	}
	blocked := `{"type":"tool_execution_start","toolCallId":"blocked","toolName":"write","args":{"path":"forbidden.txt","content":"test"}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"blocked","toolName":"write","isError":true,"result":{"content":[{"type":"text","text":"forbidden.txt is not in the plan"}]}}` + "\n"
	write(t, replyPath, blocked+string(body))
	if code := execute(t, cfg, "build", repo, "boundary fixture"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Recent(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("runs: %v %v", rows, err)
	}
	events, err := db.Events(rows[0].ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == "tool_call" {
			if !strings.Contains(string(ev.Payload), "forbidden.txt is not in the plan") || !strings.Contains(string(ev.Payload), `"isError":true`) {
				t.Fatalf("blocked reason absent: %s", ev.Payload)
			}
			return
		}
	}
	t.Fatal("no blocked tool call in trace")
}

// A report that is both terminal and malformed ends the run rather than being
// corrected: the corrected reply is free to come back without the needed list.
// Two spawns is what says the builder was stopped rather than asked again.
func TestBuildNeededOutranksAnInvalidEnvelope(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := buildStub(t, ":", `{"summary":"blocked","changed":[],"needed":["missing.txt"]}`)
	if code := execute(t, cfg, "build", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d; want 1", code)
	}
	count, _ := os.ReadFile(filepath.Join(stub, "count"))
	if strings.TrimSpace(string(count)) != "2" {
		t.Fatalf("spawn count = %s; want 2 — the builder was corrected instead of stopped", count)
	}
}
