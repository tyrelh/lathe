package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd is the one test that uses real processes: a submitter that
// starts a manager, a manager that launches a worker, and a worker that runs
// a workflow against a real checkout. Everything else about dispatch is a
// database property and is tested as one — this is here to prove the launcher
// is not lying about any of it.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a manager and a worker")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binary with")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "lathe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building lathe: %v\n%s", err, out)
	}
	// The manager outlives the submitter by design, so the test is what has to
	// stop it. The binary path is unique to this test, which is what makes
	// this safe to match on.
	t.Cleanup(func() { exec.Command("pkill", "-f", bin+" manager").Run() })

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
	} {
		if args[0] == "add" {
			os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello\n"), 0o644)
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	commit := exec.Command("git", "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "initial")
	commit.Dir = repo
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}

	// A fake agent on PATH: the worker resolves pi the way production does,
	// and the manager passes PATH through to it.
	stub := t.TempDir()
	reply := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop",` +
		`"usage":{"totalTokens":10,"cost":{"total":0.001}},"content":[{"type":"text","text":` +
		"\"Looked.\\n\\n```json\\n{\\\"summary\\\":\\\"it dispatches\\\",\\\"findings\\\":[],\\\"artifacts\\\":[]}\\n```\"}]}}"
	os.WriteFile(filepath.Join(stub, "reply.jsonl"), []byte(reply+"\n"), 0o644)
	os.WriteFile(filepath.Join(stub, "pi"), []byte("#!/bin/sh\ncat "+filepath.Join(stub, "reply.jsonl")+"\n"), 0o755)

	env := []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
	}
	lathe := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		var exit *exec.ExitError
		if err != nil {
			if !asExit(err, &exit) {
				t.Fatalf("lathe %v: %v\n%s", args, err, out)
			}
			code = exit.ExitCode()
		}
		return string(out), code
	}

	// The submitter blocks: no manager is running, so it starts one, and the
	// run comes back finished.
	out, code := lathe(t, "scout", "--repo", repo, "what is here")
	if code != 0 {
		t.Fatalf("lathe scout exited %d:\n%s\n%s", code, out, managerLog(home))
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, ": ok") {
		t.Fatalf("scout output:\n%s\n%s", out, managerLog(home))
	}

	if list, _ := lathe(t, "runs"); !strings.Contains(list, "scout") || !strings.Contains(list, "ok") {
		t.Fatalf("lathe runs:\n%s", list)
	}

	// --detach returns immediately with the ID, and wait picks the same run up
	// and reports its outcome.
	out, code = lathe(t, "scout", "--detach", "--repo", repo, "what else is here")
	if code != 0 {
		t.Fatalf("detached submission exited %d:\n%s", code, out)
	}
	fields := strings.Fields(out)
	id := fields[1]
	if !strings.HasSuffix(fields[2], "queued") {
		t.Fatalf("detached output: %q", out)
	}
	if waited, code := lathe(t, "wait", id); code != 0 || !strings.Contains(waited, ": ok") {
		t.Fatalf("lathe wait %s exited %d:\n%s\n%s", id, code, waited, managerLog(home))
	}
	if shown, code := lathe(t, "show", "--json", id); code != 0 || !strings.Contains(shown, `"status": "ok"`) {
		t.Fatalf("lathe show exited %d:\n%s", code, shown)
	}
}

func asExit(err error, out **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*out = e
	}
	return ok
}

// managerLog is what makes a failure here diagnosable: the submitter's own
// output says nothing about why a queue was never drained.
func managerLog(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "data", "lathe", "manager.log"))
	if err != nil {
		return "manager.log: " + err.Error()
	}
	return "manager.log:\n" + string(b)
}
