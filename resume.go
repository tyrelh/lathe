package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/manager"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workflow"
)

// submitRecovery is the one recovery submission; the CLI and the dashboard
// both call it. The configuration is resolved now and recorded with the
// iteration.
func submitRecovery(db *trace.DB, dataRoot, runID, guidance string, o worker.ResumeOptions, warn io.Writer) (int, error) {
	return worker.Resume(db, dataRoot, runID, guidance, o, func(name string, ov config.Overrides) (config.Snapshot, error) {
		return capture(name, ov, warn)
	})
}

// resumeCmd submits a recovery and, unless detached, waits for the
// iteration it submitted.
func resumeCmd(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	detach := fs.Bool("detach", false, "record the recovery, print the run ID and iteration, and exit")
	restart := fs.Bool("restart", false, "continue without restoring the failed iteration's work")
	base := fs.String("base", "", "with --restart: the commit to start from when none is recorded")
	asJSON := fs.Bool("json", false, "print the outcome as JSON")
	ov := overrideFlags(fs)
	fs.Parse(args)
	id := fs.Arg(0)
	guidance := strings.TrimSpace(strings.Join(fs.Args()[min(1, fs.NArg()):], " "))
	if id == "" || guidance == "" {
		fmt.Fprintf(os.Stderr, "lathe resume: needs a run ID and guidance, e.g. lathe resume <run-id> %q\n",
			"re-plan using the builder's feedback; include the integration tests")
		return 2
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	n, err := submitRecovery(db, dataRoot, id, guidance, worker.ResumeOptions{Restart: *restart, Base: *base, Overrides: *ov}, os.Stderr)
	if err != nil {
		return fail(err)
	}
	if err := manager.Start(dataRoot); err != nil {
		fmt.Fprintln(os.Stderr, "lathe: starting a manager:", err)
	}
	kind := trace.KindRecovery
	if *restart {
		kind = trace.KindRestart
	}
	fmt.Printf("resume %s iteration %d queued (%s); writing runs plan again from the earlier work and your guidance\n", id, n, kind)
	if *detach {
		return 0
	}
	return block(db, dataRoot, id, n, *asJSON)
}

// dashboardResume is the dashboard's recovery, through the CLI's path.
func dashboardResume(runID, guidance string) (int, error) {
	dataRoot, db, err := openWriter()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return submitRecovery(db, dataRoot, runID, guidance, worker.ResumeOptions{}, io.Discard)
}

// recoveryLines is what show adds about recovery: each iteration's
// provenance, the latest iteration's scope requests, and whether the run may
// be resumed and why not.
func recoveryLines(db *trace.DB, row trace.Row, its []trace.Iteration, dataRoot string, n int) []string {
	var lines []string
	for _, it := range its {
		if it.Kind == trace.KindRecovery || it.Kind == trace.KindRestart {
			lines = append(lines, fmt.Sprintf("iteration %d is a %s of iteration %d: %s", it.N, it.Kind, it.Source, clip(it.Guidance, 72)))
		}
	}
	dir := worker.ReportDir(dataRoot, row.ID, n)
	reqs, found := workflow.ReadScopeRequests(dir)
	spent := 0
	for _, req := range reqs {
		lines = append(lines, fmt.Sprintf("scope request %d (%s): needed %s", req.N, req.Outcome, strings.Join(req.Needed, ", ")))
		if req.Spent() {
			spent++
		}
	}
	if found > 0 {
		lines = append(lines, fmt.Sprintf("scope send-backs spent: %d", spent))
	}
	if trace.Terminal(row.Status) {
		if _, _, err := worker.CheckRecoverable(db, row, its); err != nil {
			lines = append(lines, "recovery: "+err.Error())
		} else {
			lines = append(lines, fmt.Sprintf("recovery: may be resumed; lathe resume %s \"guidance\"", row.ID))
		}
	}
	return lines
}
