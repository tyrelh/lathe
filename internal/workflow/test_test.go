package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/trace"
)

func TestBuildVerifyLoop(t *testing.T) {
	for _, greenAt := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("green-after-%d-fixes", greenAt), func(t *testing.T) {
			cfg, repo := buildRepo(t)
			// Tracked dirt and staged additions must both be undone by test phases.
			write(t, filepath.Join(repo, "fixture.txt"), "original\n")
			gitBuild(t, repo, "add", ".")
			gitBuild(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture")
			dir := t.TempDir()
			write(t, filepath.Join(dir, "plan"), planReply(t, `"hello.txt"`))
			write(t, filepath.Join(dir, "build"), piReply(t, "```json\n"+`{"summary":"implemented","changed":["hello.txt"],"needed":[],"artifacts":["hello.txt"]}`+"\n```"))
			command := "echo verify-dirt > fixture.txt; echo junk > coverage.out; git add fixture.txt coverage.out; echo VERIFY-TAIL >&2; test \"$(cat hello.txt)\" = good"
			report, _ := json.Marshal(map[string]any{"summary": "discovered", "command": command, "failures": []string{"TESTER-OBSERVATION"}, "artifacts": []string{}})
			write(t, filepath.Join(dir, "test"), piReply(t, "```json\n"+string(report)+"\n```"))
			onPath(t, dir, fmt.Sprintf(`#!/bin/sh
n=0
[ ! -f %[1]q/count ] || n=$(cat %[1]q/count)
echo $((n+1)) > %[1]q/count
printf '%%s\n' "$@" > %[1]q/args$n
cp "$LATHE_PERMIT" %[1]q/scope$n
case "$n" in
0) cat %[1]q/plan ;;
2) echo tester-dirt > fixture.txt; echo junk > coverage.out
   git add fixture.txt coverage.out
   cat %[1]q/test ;;
*) round=$((n-2)); [ "$n" != 1 ] || round=0
   if [ "$round" -ge %[2]d ]; then echo good > hello.txt; else echo bad > hello.txt; fi
   cat %[1]q/build ;;
esac
`, dir, greenAt))
			wantCode := 0
			fixes := greenAt
			if greenAt == 3 {
				wantCode, fixes = 1, 2
			}
			if code := execute(t, cfg, "build", repo, "change greeting"); code != wantCode {
				t.Fatalf("code %d want %d", code, wantCode)
			}
			count, _ := os.ReadFile(filepath.Join(dir, "count"))
			if strings.TrimSpace(string(count)) != fmt.Sprint(3+fixes) {
				t.Fatalf("agent calls: %s", count)
			}
			if got := gitBuild(t, repo, "status", "--porcelain"); got != " M hello.txt\n" {
				t.Fatalf("dirty tree: %q", got)
			}
			args := func(n int) string {
				b, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("args%d", n)))
				return string(b)
			}
			session := func(s string) string {
				lines := strings.Split(s, "\n")
				for i, v := range lines {
					if v == "--session-id" {
						return lines[i+1]
					}
				}
				t.Fatal("missing session")
				return ""
			}
			initial := session(args(1))
			if session(args(2)) == initial {
				t.Fatal("tester reused builder session")
			}
			if !strings.Contains(args(2), "read,grep,find,ls,bash") {
				t.Fatal("tester tools")
			}
			for n := 3; n < 3+fixes; n++ {
				if session(args(n)) != initial {
					t.Fatal("fix lost builder session")
				}
				if !strings.Contains(args(n), "VERIFY-TAIL") {
					t.Fatal("fix missing output")
				}
				if strings.Contains(args(n), "TESTER-OBSERVATION") != (n == 3) {
					t.Fatal("tester failures must appear only in first fix")
				}
				scope, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("scope%d", n)))
				if !strings.Contains(string(scope), `"allow":["hello.txt"]`) {
					t.Fatalf("fix scope: %s", scope)
				}
			}
			scope, _ := os.ReadFile(filepath.Join(dir, "scope2"))
			if !strings.Contains(string(scope), `"allow":[]`) {
				t.Fatalf("tester scope: %s", scope)
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
			wantStatus := "ok"
			if wantCode != 0 {
				wantStatus = "fail"
			}
			if rows[0].Status != wantStatus {
				t.Fatalf("run status %s", rows[0].Status)
			}
			phases, err := db.Phases(rows[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{"request", "plan", "build", "test", "verify"}
			for i := 0; i < fixes; i++ {
				names = append(names, "fix", "verify")
			}
			if len(phases) != len(names) {
				t.Fatalf("phases: %v", phases)
			}
			verify := 0
			for i, ph := range phases {
				if ph.Name != names[i] {
					t.Fatalf("phase %d: %s", i, ph.Name)
				}
				if ph.Name == "verify" {
					status := "fail"
					if verify >= greenAt {
						status = "success"
					}
					if ph.Owner != "engineer" || ph.Status != status {
						t.Fatalf("verify: %+v", ph)
					}
					verify++
				}
			}
		})
	}
}

func TestTestOutputRequiredFields(t *testing.T) {
	if v := (&TestOutput{}).Validate(); len(v) != 4 {
		t.Fatalf("violations %v", v)
	}
	summary, command, empty := "suite", "go test ./...", []string{}
	out := &TestOutput{Summary: &summary, Command: &command, Failures: &empty, Wrote: &empty}
	if v := out.Validate(); len(v) != 0 {
		t.Fatal(v)
	}
	command = "  "
	if v := out.Validate(); len(v) != 1 {
		t.Fatal(v)
	}
}
