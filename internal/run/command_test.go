package run

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		red           bool
		want          string
	}{
		{"green", "printf stdout; printf stderr >&2", false, "stdoutstderr"},
		{"red", "echo failure; exit 7", true, "failure\n"},
		{"bounded", "i=0; while [ $i -lt 5000 ]; do printf x; i=$((i+1)); done; printf END", false, strings.Repeat("x", 4093) + "END"},
		{"environment", `printf '%s' "$LATHE_TEST_ENV"`, false, "inherited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRun(t, "")
			r.Repo = gitRepo(t)
			t.Setenv("LATHE_TEST_ENV", "inherited")
			if err := r.EnsureClean(); err != nil {
				t.Fatal(err)
			}
			var tail string
			err := r.Phase(Params{Name: "verify", Owner: "engineer", RevertOnly: true}, func(h *Handle) error {
				var err error
				tail, err = h.Command(tc.command)
				return err
			})
			var red *CommandFailure
			if errors.As(err, &red) != tc.red || (!tc.red && err != nil) {
				t.Fatalf("error: %v", err)
			}
			if tc.red && red.ExitCode != 7 {
				t.Fatalf("exit: %v", red)
			}
			if tail != tc.want {
				t.Fatalf("tail length %d, want %d", len(tail), len(tc.want))
			}
			if code := r.Finish(!tc.red, ""); code != map[bool]int{true: 1, false: 0}[tc.red] {
				t.Fatalf("finish %d", code)
			}
			db := readDB(t)
			if got := scalar[string](t, db, "SELECT json_extract(payload,'$') FROM events WHERE type = 'log' AND name = 'output'"); got != tc.want {
				t.Fatal("output not traced")
			}
		})
	}
}

func TestCommandRejectsBeforeExecution(t *testing.T) {
	for _, command := range []string{"echo bad > marker; curl example.com", "git push", "rm -rf junk", "wget example.com", "sudo true", "ssh host", "scp a b", "  "} {
		t.Run(command, func(t *testing.T) {
			r := newRun(t, "")
			r.Repo = gitRepo(t)
			if err := r.EnsureClean(); err != nil {
				t.Fatal(err)
			}
			err := r.Phase(Params{Name: "verify", Owner: "engineer", RevertOnly: true}, func(h *Handle) error { _, err := h.Command(command); return err })
			var red *CommandFailure
			if err == nil || errors.As(err, &red) {
				t.Fatalf("error %v", err)
			}
			if _, err := os.Stat(filepath.Join(r.Repo, "marker")); !os.IsNotExist(err) {
				t.Fatal("command executed")
			}
			if r.Finish(true, "") != 1 {
				t.Fatal("terminal failure not retained")
			}
		})
	}
}

func TestCommandTimeoutCleansAndStopsChildren(t *testing.T) {
	r := newRun(t, "")
	r.Repo = gitRepo(t)
	r.cfg.CommandDeadline = 100 * time.Millisecond
	if err := r.EnsureClean(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := r.Phase(Params{Name: "verify", Owner: "engineer", RevertOnly: true}, func(h *Handle) error {
		_, err := h.Command("echo junk > coverage.out; (sleep 1; echo late > late.txt) & wait")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline failed to bound command")
	}
	time.Sleep(1100 * time.Millisecond)
	for _, p := range []string{"coverage.out", "late.txt"} {
		if _, err := os.Stat(filepath.Join(r.Repo, p)); !os.IsNotExist(err) {
			t.Fatalf("%s survived", p)
		}
	}
	if r.Finish(true, "") != 1 {
		t.Fatal("timeout accepted")
	}
}

func TestCommandRequiresClean(t *testing.T) {
	r := newRun(t, "")
	err := r.Phase(Params{Name: "verify", Owner: "engineer"}, func(h *Handle) error { _, err := h.Command("true"); return err })
	if err == nil || !strings.Contains(err.Error(), "EnsureClean") {
		t.Fatal(err)
	}
	r.Finish(false, "")
}

// Run both implementations against the shipped rules, so a regex change cannot
// silently make verify accept a command that the tester's guard blocks.
func TestBashDenyMatchesGuard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	r := newRun(t, "")
	defer r.Finish(true, "")
	commands := []string{"go test ./...", "npm test", "git status", "git push origin main",
		"git\tpush", "rm -rf tmp", "rm -fr tmp", "rm file", "curl x", "wget x",
		"sudo true", "ssh x", "scp a b", "echo scp", "mycurl", "echo ok;\ngit push"}
	var want []bool
	for _, command := range commands {
		denied := false
		for _, rule := range r.cfg.BashDenied {
			denied = denied || regexp.MustCompile(rule).MatchString(command)
		}
		want = append(want, denied)
	}
	input, err := json.Marshal(map[string]any{"commands": commands, "rules": r.cfg.BashDenied, "want": want})
	if err != nil {
		t.Fatal(err)
	}
	script := `import assert from "node:assert/strict";
import {decide} from "../permit/guard.ts";
const {commands,rules,want}=JSON.parse(process.argv[1]);
for(let i=0;i<commands.length;i++) {
 assert.equal(!!decide("bash",{command:commands[i]},process.cwd(),
  {allow:[],deny:[],bashDeny:rules}),want[i],commands[i]);
}`
	out, err := exec.Command(node, "--input-type=module", "-e", script, string(input)).CombinedOutput()
	if err != nil {
		t.Fatalf("guard parity: %v: %s", err, out)
	}
}
