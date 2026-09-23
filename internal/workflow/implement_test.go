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
	gitBuild(t, repo, "config", "user.name", "Test")
	gitBuild(t, repo, "config", "user.email", "test@example.com")
	gitBuild(t, repo, "add", ".")
	gitBuild(t, repo, "commit", "-qm", "initial")
	return cfg, repo
}

// implementStub is codeStub with the builder's action and report replaced. The
// stub can bypass the extension, so out-of-scope writes test defense in depth.
func implementStub(t *testing.T, action, report string) string {
	t.Helper()
	dir := codeStub(t)
	write(t, filepath.Join(dir, "builder.sh"), action+"\n")
	write(t, filepath.Join(dir, "builder"), piReply(t, "```json\n"+report+"\n```"))
	return dir
}

// totalSpawns is every agent's spawns added up.
func totalSpawns(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, agent := range Agents["build"] {
		n += spawnsOf(t, dir, agent)
	}
	return n
}

func TestImplement(t *testing.T) {
	for _, tc := range []struct {
		name, action, changed, needed string
		want, builds, calls           int
	}{
		{"success", "printf 'hello world\\n' > hello.txt", `["hello.txt"]`, `[]`, 0, 1, 8},
		{"needed is terminal", "printf 'hello world\\n' > hello.txt", `["hello.txt"]`, `["missing.txt"]`, 1, 1, 3},
		{"unplanned write reverted", "printf 'hello world\\n' > hello.txt; echo bad > outside.txt", `["hello.txt"]`, `[]`, 1, 1, 3},
		{"false claim", ":", `["hello.txt"]`, `[]`, 1, 3, 5},
		{"omitted change", "echo changed > hello.txt", `[]`, `[]`, 1, 3, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo := buildRepo(t)
			report := fmt.Sprintf(`{"summary":"updated greeting","changed":%s,"needed":%s,"artifacts":[]}`, tc.changed, tc.needed)
			stub := implementStub(t, tc.action, report)
			if got := execute(t, cfg, "implement", repo, "greet the world"); got != tc.want {
				t.Fatalf("code = %d, want %d", got, tc.want)
			}
			if got := spawnsOf(t, stub, "builder"); got != tc.builds {
				t.Fatalf("builder spawns = %d", got)
			}
			if got := totalSpawns(t, stub); got != tc.calls {
				t.Fatalf("spawn count = %d", got)
			}
			if _, err := os.Stat(filepath.Join(repo, "outside.txt")); !os.IsNotExist(err) {
				t.Fatalf("out-of-scope file survived: %v", err)
			}
			dir := latestRunDir(t)
			if _, err := os.Stat(filepath.Join(dir, "plan.json")); err != nil {
				t.Fatal(err)
			}
			args := stubFile(t, stub, "args.builder.0")
			for _, want := range []string{"greet the world", "edit fetch.go", "hello.txt", "read,grep,find,ls,write,edit", "--no-extensions"} {
				if !strings.Contains(args, want) {
					t.Errorf("builder arguments missing %q: %s", want, args)
				}
			}
			for file, want := range map[string]string{
				"scope.planner.0": `"allow":[]`, "scope.plan-reviewer.0": `"allow":[]`, "scope.builder.0": `"allow":["hello.txt"]`,
			} {
				if scope := stubFile(t, stub, file); !strings.Contains(scope, want) {
					t.Errorf("%s = %s; want %s", file, scope, want)
				}
			}
			status := "ok"
			if tc.want != 0 {
				status = "fail"
			}
			if got := latestRun(t).Status; got != status {
				t.Fatalf("status = %s", got)
			}
			if tc.want == 0 || tc.needed != `[]` {
				b, err := os.ReadFile(filepath.Join(dir, "implement.json"))
				if err != nil {
					t.Fatal(err)
				}
				var out ImplementOutput
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

func TestImplementDirtyRepoDoesNotSpawn(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := implementStub(t, ":", `{}`)
	write(t, filepath.Join(repo, "hello.txt"), "my work\n")
	if code := execute(t, cfg, "implement", repo, "change greeting"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if n := totalSpawns(t, stub); n != 0 {
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
	if v := ChangesMatchClaim(&ImplementOutput{Changed: &paths}, r); len(v) != 0 {
		t.Fatalf("rename/newline paths: %v", v)
	}
	paths = []string{"invented.txt"}
	if v := ChangesMatchClaim(&ImplementOutput{Changed: &paths}, r); len(v) != 4 {
		t.Fatalf("bidirectional mismatches: %v", v)
	}
	if v := ChangesMatchClaim(&ImplementOutput{Changed: &paths}, &run.Run{Repo: t.TempDir()}); len(v) != 1 {
		t.Fatalf("git failure: %v", v)
	}
}

func TestImplementOutputRequiresEveryKey(t *testing.T) {
	if v := (&ImplementOutput{}).Validate(); len(v) != 4 {
		t.Fatalf("violations: %v", v)
	}
	summary, empty := "done", []string{}
	b := &ImplementOutput{Summary: &summary, Changed: &empty, Needed: &empty, Wrote: &empty}
	if v := b.Validate(); len(v) != 0 {
		t.Fatalf("empty lists: %v", v)
	}
}

func TestImplementBlockedWriteTrace(t *testing.T) {
	cfg, repo := buildRepo(t)
	dir := implementStub(t, ":", `{"summary":"blocked","changed":[],"needed":["forbidden.txt"],"artifacts":[]}`)
	replyPath := filepath.Join(dir, "builder")
	body, err := os.ReadFile(replyPath)
	if err != nil {
		t.Fatal(err)
	}
	blocked := `{"type":"tool_execution_start","toolCallId":"blocked","toolName":"write","args":{"path":"forbidden.txt","content":"test"}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"blocked","toolName":"write","isError":true,"result":{"content":[{"type":"text","text":"forbidden.txt is not in the plan"}]}}` + "\n"
	write(t, replyPath, blocked+string(body))
	if code := execute(t, cfg, "implement", repo, "boundary fixture"); code != 1 {
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
// Three spawns is what says the builder was stopped rather than asked again.
func TestImplementNeededOutranksAnInvalidEnvelope(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := implementStub(t, ":", `{"summary":"blocked","changed":[],"needed":["missing.txt"]}`)
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d; want 1", code)
	}
	if n := totalSpawns(t, stub); n != 3 {
		t.Fatalf("spawn count = %d; want 3 — the builder was corrected instead of stopped", n)
	}
}
