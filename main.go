// Command lathe orchestrates bounded agent work in whatever repo you invoke it from.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tyrelh/lathe/dashboard"
	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/install"
	"github.com/tyrelh/lathe/internal/manager"
	"github.com/tyrelh/lathe/internal/process"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

const usage = `lathe — a software factory you run from any repo

usage: lathe <command> [flags] [args]

commands:
  scout "<request>"   investigate the current repo and report what is there
  plan "<request>"    plan a change to the current repo; write nothing
  implement "<req>"   plan, implement and test a change; save it as a patch
  build "<request>"   implement it, then branch, commit and open a pull request
  revise <id> "<change>"
                      plan, validate and push another change to a build's open
                      pull request, as the next iteration of the same run
  resume <id> "<guidance>"
                      continue a failed, cancelled or lost run as a new
                      iteration; writing runs plan again from the earlier
                      work and your guidance (--restart [--base <ref>] starts
                      over without restoring that work)
  manager             run the scheduler and the dashboard in the foreground
  runs                list recent runs, from every repo
  show <id>           state, outcome, iterations and reports for one run
                      (--iteration <n> for an earlier iteration's reports)
  wait <id>           block until a run is terminal; exit with its outcome
                      (--iteration <n> waits for that iteration instead)
  cancel <id>         ask a run's current iteration to stop
  install             link this repo into each agent's skills directory
  version             print the version of this binary
  help                show this message

flags (before the request, as Go's flag package stops at the first argument):
  --repo <dir>        act on this repository instead of the working directory
  --issue <ref>       use a GitHub issue instead of a request (plan, implement, build)
  --detach            record the request, print the run ID and exit
  --provider <name>   override the provider for this run
  --model <name>      override the model for this run
  --thinking <level>  override the thinking level; ignored by models without one

Workflow commands record a run and block until it finishes. The work happens
in a worker process, so interrupting the wait detaches from the run rather than
cancelling it; use lathe cancel for that. A manager is started automatically if
none is running, and serves the dashboard.

Agent configuration lives in ~/.lathe: agents/<name>/config.yaml, system.md
and user.md, and routing/<agent>.md. Missing files are seeded from this binary
when a run is submitted.

implement, build and revise run in a worktree of their own under the data
directory, created at the commit HEAD was on when you submitted (a revision's
at the commit lathe last pushed). Your checkout is never changed: it may be
dirty or on another branch, and several of these runs may go at once.
implement saves its result as result.patch in the run directory; lathe show
prints where, and git apply --index <patch> applies it from a checkout at
the run's base commit. build pushes a new branch and opens a pull request; a
failure after its commit leaves the commit on that branch. Commits nothing
else reaches are kept under refs/lathe/runs/<id>/iteration-<n>.

revise needs a build whose latest iteration succeeded and whose pull request is
still open, with origin's branch at the commit lathe last pushed. It
publishes only a change validation accepts, and leaves the pull request's
title and description alone.

environment:
  Workers inherit the manager's environment: credentials such as
  MOONSHOT_API_KEY, PATH, test settings. An auto-started manager takes the
  environment of the submission that started it; restart it to change that.
  LATHE_CAPACITY      how many runs may execute at once (default 1)
  TYPESAFE_API_KEY    lets routing pick tiers set in ~/.lathe; without it,
                      routed agents use their default tier

Run install from a checkout of the lathe repo; it links the checkout itself,
so SKILL.md stays discoverable.
`

// version is stamped by the release build with -ldflags "-X main.version=...".
// An ordinary `go build` leaves it at dev, which is what the dashboard shows.
var version = "dev"

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2
	}
	cmd := args[0]
	if workflow.Submittable(cmd) {
		return submit(cmd, args[1:])
	}
	switch cmd {
	case workflow.NameRevise:
		return reviseCmd(args[1:])
	case "resume":
		return resumeCmd(args[1:])
	case "manager":
		return managerCmd()
	case "worker":
		return workerCmd(args[1:])
	// Hidden: the supervisor every subprocess of a writing run starts as.
	case process.HelperArg:
		return process.Helper(args[1:])
	case "runs":
		return runs(args[1:])
	case "show":
		return show(args[1:])
	case "wait":
		return waitCmd(args[1:])
	case "cancel":
		return cancel(args[1:])
	case "install":
		return installCmd()
	// Hidden: a tuning tool for whoever configures routing, not a workflow.
	case "route-calibrate":
		return routeCalibrate(args[1:])
	case "version", "--version":
		fmt.Println(version)
		return 0
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "lathe: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// submit records a run and, unless detached, waits for it. A submission
// succeeds when the database has accepted the request; execution may still
// fail, and that failure comes back through the wait rather than from here.
func submit(name string, args []string) int {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	repo := fs.String("repo", "", "repository to act on (default: the git root of the working directory)")
	detach := fs.Bool("detach", false, "record the request, print the run ID and exit")
	issue := fs.String("issue", "", "GitHub issue number, URL, or owner/repo#number (instead of a request)")
	ov := overrideFlags(fs)
	fs.Parse(args)

	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	var issueRef string
	issueSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "issue" {
			issueSet = true
		}
	})
	if issueSet {
		if name == workflow.NameScout || request != "" {
			fmt.Fprintln(os.Stderr, "lathe: --issue requires plan, implement, or build and cannot be combined with a request")
			return 2
		}
		var err error
		if request, issueRef, err = issueRequest(*issue); err != nil {
			fmt.Fprintln(os.Stderr, "lathe:", err)
			return 2
		}
	}
	if request == "" {
		fmt.Fprintf(os.Stderr, "lathe %s: needs a request or --issue, e.g. lathe %s %q\n",
			name, name, "add retry with backoff to the fetch client")
		return 2
	}

	dir := *repo
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fail(err)
		}
		dir = cwd
	}
	req, err := prepare(name, dir, request, issueRef, *ov, os.Stderr)
	if err != nil {
		return fail(err)
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	id, err := db.Submit(req.Request)
	if err != nil {
		return fail(err)
	}
	ensureManager(dataRoot)
	if workflow.Writes[name] {
		fmt.Printf("lathe %s %s: runs in its own worktree from %s; %s is left as it is.\n",
			name, id, workspace.Short(req.Base), req.Repo)
		if name == workflow.NameImplement {
			fmt.Printf("  Its result is saved as result.patch in %s.\n", worker.ReportDir(dataRoot, id, 0))
		}
	}
	fmt.Printf("%s %s queued\n", name, id)
	if *detach {
		return 0
	}
	return block(db, dataRoot, id, -1, false)
}

// prepare turns a submission into the row that records it: the checkout
// resolved from dir and the agent roster frozen into a spec. It writes nothing
// to the database; the caller submits the request.
func prepare(name, dir, request, issueRef string, ov config.Overrides, warn io.Writer) (submission, error) {
	ws, err := workspace.Discover(dir)
	if err != nil {
		return submission{}, err
	}
	roster, err := capture(name, ov, warn)
	if err != nil {
		return submission{}, err
	}
	// A writing run starts from the commit HEAD is on now, in a worktree of
	// its own; the checkout's changes and later branch switches do not
	// reach it. A read-only run reads the checkout, so it records the branch.
	var branch, base string
	if workflow.Writes[name] {
		if base, err = workspace.Head(ws.Path); err != nil {
			return submission{}, fmt.Errorf("%s has no commit to start a worktree from", ws.Path)
		}
		branch, _ = workspace.Branch(ws.Path)
	} else if branch, err = workspace.Branch(ws.Path); err != nil {
		return submission{}, err
	}
	spec, err := worker.Spec{
		Version: worker.SpecVersion, Workflow: name, Request: request, Issue: issueRef,
		Workspace: ws, Roster: roster, Overrides: ov, Base: base,
	}.Marshal()
	if err != nil {
		return submission{}, err
	}
	return submission{Request: trace.Request{
		Workflow: name, Repo: ws.Path, Request: request, Branch: branch, Spec: spec,
	}, Base: base}, nil
}

// submission is a prepared request and the base commit it records.
type submission struct {
	trace.Request
	Base string
}

// capture resolves the configuration a workflow's agents run with, as it is
// now in ~/.lathe, and freezes it, prompt text included. A missing or invalid
// file refuses the capture with an error naming it. Edited files whose seed has
// changed are reported to warn.
func capture(name string, ov config.Overrides, warn io.Writer) (config.Snapshot, error) {
	cfg, err := loadRosterWarn(warn)
	if err != nil {
		return config.Snapshot{}, err
	}
	return cfg.Capture(workflow.Agents[name], ov)
}

// loadRoster seeds ~/.lathe and loads it.
func loadRoster() (config.Config, error) {
	return loadRosterWarn(io.Discard)
}

// loadRosterWarn seeds ~/.lathe, loads it, and names each drifted file on
// warn.
func loadRosterWarn(warn io.Writer) (config.Config, error) {
	home, err := agentconfig.Home()
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := agentconfig.Open(home, Seed, version)
	if err != nil {
		return config.Config{}, err
	}
	for _, f := range cfg.Files {
		if f.Drift {
			fmt.Fprintln(warn, "lathe: edited, and its seed has changed since:", f.Path)
		}
	}
	return cfg, nil
}

// submitRevision is the one revision submission; the CLI and the dashboard
// both call it. The configuration is resolved now and recorded with the
// iteration.
func submitRevision(db *trace.DB, dataRoot, runID, request string, ov config.Overrides, warn io.Writer) (int, error) {
	return worker.Revise(db, dataRoot, runID, request, ov, func(name string, ov config.Overrides) (config.Snapshot, error) {
		return capture(name, ov, warn)
	})
}

// reviseCmd submits a revision and, unless detached, waits for the iteration
// it submitted — not the run's latest, which a later submission could move.
func reviseCmd(args []string) int {
	fs := flag.NewFlagSet("revise", flag.ExitOnError)
	detach := fs.Bool("detach", false, "record the revision, print the run ID and iteration, and exit")
	ov := overrideFlags(fs)
	fs.Parse(args)
	id := fs.Arg(0)
	request := strings.TrimSpace(strings.Join(fs.Args()[min(1, fs.NArg()):], " "))
	if id == "" || request == "" {
		fmt.Fprintf(os.Stderr, "lathe revise: needs a run ID and the change, e.g. lathe revise <run-id> %q\n",
			"handle the empty result without showing an error")
		return 2
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	n, err := submitRevision(db, dataRoot, id, request, *ov, os.Stderr)
	if err != nil {
		return fail(err)
	}
	ensureManager(dataRoot)
	row, _ := db.Get(id)
	fmt.Printf("lathe revise %s: runs in its own worktree at %s; %s is left as it is.\n"+
		"  It pushes to %s only once validation accepts the change.\n",
		id, workspace.Short(row.Commit), row.Repo, row.Branch)
	fmt.Printf("revise %s iteration %d queued\n", id, n)
	if *detach {
		return 0
	}
	return block(db, dataRoot, id, n, false)
}

// prepareAndSubmit submits a run from exactly one of prompt or issue, with no
// overrides, refusing if repo no longer resolves to the checkout it names. It
// returns the run ID.
func prepareAndSubmit(name, repo, prompt, issue string) (string, error) {
	request, issueRef := prompt, ""
	if issue != "" {
		var err error
		if request, issueRef, err = issueRequest(issue); err != nil {
			return "", err
		}
	}
	req, err := prepare(name, repo, request, issueRef, config.Overrides{}, io.Discard)
	if err != nil {
		return "", err
	}
	if req.Repo != repo {
		return "", fmt.Errorf("%s now resolves to %s", repo, req.Repo)
	}
	_, db, err := openWriter()
	if err != nil {
		return "", err
	}
	defer db.Close()
	return db.Submit(req.Request)
}

// issueRequest is the request recorded for a run of issue raw, and its
// normalized reference.
func issueRequest(raw string) (request, ref string, err error) {
	if ref, err = workspace.IssueReference(raw); err != nil {
		return "", "", err
	}
	return "GitHub issue " + ref, ref, nil
}

// ensureManager starts a manager for dataRoot if none is running, warning on
// stderr when it cannot.
func ensureManager(dataRoot string) {
	if err := manager.Start(dataRoot); err != nil {
		fmt.Fprintln(os.Stderr, "lathe: starting a manager:", err)
	}
}

// dashboardRevise submits a revision through a writer handle.
func dashboardRevise(runID, request string) (int, error) {
	dataRoot, db, err := openWriter()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	return submitRevision(db, dataRoot, runID, request, config.Overrides{}, io.Discard)
}

func dashboardCancel(runID string) (string, error) {
	_, db, err := openWriter()
	if err != nil {
		return "", err
	}
	defer db.Close()
	return db.RequestCancel(runID)
}

func dashboardArchiveTask(taskID string) error {
	_, db, err := openWriter()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.ArchiveTask(taskID)
}

func dashboardUnarchiveTask(taskID string) error {
	_, db, err := openWriter()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.UnarchiveTask(taskID)
}

// openWriter opens lathe's data root and database.
func openWriter() (string, *trace.DB, error) {
	dataRoot, err := trace.DataRoot()
	if err != nil {
		return "", nil, err
	}
	db, err := trace.Open(dataRoot)
	return dataRoot, db, err
}

// block waits for a terminal state and prints the report. iteration is the
// one to wait for, or -1 for the run as a whole: a run is terminal when its
// latest iteration is, and a revision submitted meanwhile makes it live again.
// Interrupting it detaches: a detached run is a success of the command that
// was typed, so the exit code is 0 and a shell chain keeps going.
func block(db *trace.DB, dataRoot, id string, iteration int, asJSON bool) int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	again := "lathe wait " + id
	if iteration >= 0 {
		again = fmt.Sprintf("lathe wait --iteration %d %s", iteration, id)
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		row, err := db.Get(id)
		if err != nil {
			return fail(err)
		}
		n, status := row.Iteration, row.Status
		if iteration >= 0 {
			its, err := db.Iterations(id)
			if err != nil {
				return fail(err)
			}
			if iteration >= len(its) {
				return fail(fmt.Errorf("%s has no iteration %d", id, iteration))
			}
			n, status = iteration, its[iteration].Status
		}
		if trace.Terminal(status) {
			return report(db, row, dataRoot, n, asJSON)
		}
		select {
		case <-sig:
			fmt.Printf("\ndetached from %s; it keeps running\n  %s\n  lathe cancel %s\n", id, again, id)
			return 0
		case <-tick.C:
		}
	}
}

// outcome maps a terminal state to an exit code. A cancelled run reports 130,
// the conventional "stopped by a signal", so a caller can tell it from work
// that ran and failed.
func outcome(status string) int {
	switch status {
	case trace.StatusOK:
		return 0
	case trace.StatusCancelled:
		return trace.ExitCancelled
	default:
		return 1
	}
}

// report prints what an iteration produced, including the partial reports of
// one that failed, was cancelled, or was lost, and the run's history around
// it. Tokens and cost are the run's, across every iteration. An iteration
// still queued or running has written nothing yet, so the reports shown are
// the previous iteration's, and say so.
func report(db *trace.DB, row trace.Row, dataRoot string, n int, asJSON bool) int {
	its, err := db.Iterations(row.ID)
	if err != nil {
		return fail(err)
	}
	it := trace.Iteration{N: n, Request: row.Request, Status: row.Status, Reason: row.Reason}
	if n < len(its) {
		it = its[n]
	}
	from := n
	if !trace.Terminal(it.Status) && n > 0 {
		from = n - 1
	}
	dir := worker.ReportDir(dataRoot, row.ID, from)
	var present []string
	for _, name := range run.Reports {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			present = append(present, name)
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"run": row, "iteration": n, "iterations": worker.IterationViews(its),
			"dir": dir, "reports_iteration": from, "reports": present,
			"recovery": recoveryLines(db, row, its, dataRoot, n)})
		return outcome(it.Status)
	}

	label := ""
	if len(its) > 1 {
		label = fmt.Sprintf(" iteration %d", n)
	}
	run.Banner(os.Stdout, row.Workflow, row.ID, label, it.Status, row.Tokens, row.Cost, dir, it.Reason)
	if row.Commit != "" {
		fmt.Printf("  %s @ %s\n", row.Branch, workspace.Short(row.Commit))
	}
	if it.Worktree != "" {
		fmt.Printf("  worktree %s (%s)\n", it.Worktree, it.WorktreeState)
		if it.CleanupError != "" {
			fmt.Printf("  cleanup pending: %s\n", it.CleanupError)
		}
	}
	worker.PrintResult(os.Stdout, it, it.Status == trace.StatusOK)
	if len(its) > 1 {
		fmt.Println("  iterations:")
		for _, x := range its {
			commit := ""
			if x.Commit != "" {
				commit = "  @ " + workspace.Short(x.Commit)
				if x.Remote != "" && x.Remote != x.Commit {
					commit += " (not on origin, which is at " + workspace.Short(x.Remote) + ")"
				}
			}
			fmt.Printf("    %d  %-9s %s%s\n", x.N, x.Status, clip(x.Request, 72), commit)
		}
	}
	for _, line := range recoveryLines(db, row, its, dataRoot, n) {
		fmt.Printf("  %s\n", line)
	}
	if from != n {
		fmt.Printf("  iteration %d is %s; the reports below are prior, from iteration %d\n", n, it.Status, from)
	}
	for _, name := range present {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var out struct {
			Summary string `json:"summary"`
			Title   string `json:"title"`
			URL     string `json:"url"`
			Draft   bool   `json:"draft"`
		}
		json.Unmarshal(b, &out)
		if name == run.IssueFile {
			out.Summary = out.Title
		}
		// A draft is unaccepted work, and says so rather than passing as shipped.
		if name == run.PRFile && out.Draft {
			out.Summary = "draft, validation not accepted: " + out.URL
		}
		fmt.Printf("  %s  %s\n", name, out.Summary)
	}
	return outcome(it.Status)
}

func managerCmd() int {
	dataRoot, err := trace.DataRoot()
	if err != nil {
		return fail(err)
	}
	// Signal handling lives here rather than inside the packages: a library
	// that calls os.Exit cannot be reused by a second command.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The dashboard validates each request's shape; these check everything
	// that needs the filesystem.
	actions := dashboard.Actions{
		Submit: prepareAndSubmit, Revise: dashboardRevise, Resume: dashboardResume, Cancel: dashboardCancel,
		ArchiveTask: dashboardArchiveTask, UnarchiveTask: dashboardUnarchiveTask,
		SavePrompt: func(key, body, baseHash string) error {
			home, err := agentconfig.Home()
			if err != nil {
				return err
			}
			return agentconfig.SaveFile(home, key, body, baseHash)
		},
		ResetPrompt: func(key, baseHash string) error {
			home, err := agentconfig.Home()
			if err != nil {
				return err
			}
			return agentconfig.ResetFile(home, Seed, version, key, baseHash)
		},
		SaveConfig: func(agent, provider, model, thinking, baseHash string) error {
			home, err := agentconfig.Home()
			if err != nil {
				return err
			}
			return agentconfig.SaveSettings(home, agent, agentconfig.Settings{Provider: provider, Model: model, Thinking: thinking}, baseHash)
		},
	}
	sources := dashboard.Sources{Roster: loadRoster}
	if err := manager.Run(ctx, dataRoot, version, os.Stdout, actions, sources); err != nil {
		fmt.Fprintln(os.Stderr, "lathe manager:", err)
		return 1
	}
	return 0
}

// workerCmd is the manager's entry point, not a user's. Its data directory is
// explicit because a worker must not depend on the environment of whatever
// shell happened to submit the run.
func workerCmd(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	dataRoot := fs.String("data", "", "data directory (required)")
	runID := fs.String("run", "", "run to execute (required)")
	attempt := fs.String("attempt", "", "attempt to claim (required)")
	fs.Parse(args)
	if *dataRoot == "" || *runID == "" || *attempt == "" {
		fmt.Fprintln(os.Stderr, "lathe worker: --data, --run and --attempt are required")
		return 2
	}
	return worker.Execute(*dataRoot, *runID, *attempt, os.Stdout)
}

// runs lists runs from the global database, queued and active included. Repos
// show as basenames, and queue time and execution time are separate columns.
func runs(args []string) int {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	n := fs.Int("n", 20, "how many runs to show")
	fs.Parse(args)

	_, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()

	rows, err := db.Recent(*n)
	if err != nil {
		return fail(err)
	}
	if len(rows) == 0 {
		fmt.Println("no runs yet")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	// Ordered by latest activity, so a revised run sits where its revision
	// was submitted; SUBMITTED stays when the run was created.
	fmt.Fprintln(w, "SUBMITTED\tSTATUS\tWORKFLOW\tITER\tREPO\tQUEUED\tRAN\tTOKENS\tCOST\tRUN")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\t$%.5f\t%s\n",
			r.Submitted, r.Status, r.Workflow, r.Iteration, filepath.Base(r.Repo),
			span(r.Submitted, r.Started), span(r.Started, r.Ended), r.Tokens, r.Cost, r.ID)
	}
	return flush(w)
}

// span is how long one stage took. An unstarted stage is "-", an unfinished
// one is measured against now, which is what makes a live run's numbers move.
func span(from, to string) string {
	start, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return "-"
	}
	end := time.Now().UTC()
	if to != "" {
		if end, err = time.Parse(time.RFC3339, to); err != nil {
			return "-"
		}
	}
	return end.Sub(start).Truncate(time.Second).String()
}

func show(args []string) int {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the run as JSON")
	iteration := fs.Int("iteration", -1, "show this iteration's outcome and reports (default: the latest)")
	fs.Parse(args)
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "lathe show: needs a run ID")
		return 2
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	row, err := db.Get(id)
	if err != nil {
		return fail(err)
	}
	n := row.Iteration
	if *iteration >= 0 {
		n = *iteration
	}
	return report(db, row, dataRoot, n, *asJSON)
}

// clip shortens s to n runes for a one-line listing.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func waitCmd(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	iteration := fs.Int("iteration", -1, "wait for this iteration rather than the run")
	fs.Parse(args)
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "lathe wait: needs a run ID")
		return 2
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	if _, err := db.Get(id); err != nil {
		return fail(err)
	}
	return block(db, dataRoot, id, *iteration, *asJSON)
}

func cancel(args []string) int {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "lathe cancel: needs a run ID")
		return 2
	}
	_, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	status, err := db.RequestCancel(id)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "lathe cancel: %s: %v (%s)\n", id, err, status)
		return 1
	case status == trace.StatusCancelled:
		fmt.Printf("%s cancelled\n", id)
	default:
		fmt.Printf("%s: cancellation requested; it stops when its worker notices\n", id)
	}
	return 0
}

// openData is openWriter for a command: on failure it reports the error and
// returns a nil db with the exit code.
func openData() (string, *trace.DB, int) {
	dataRoot, db, err := openWriter()
	if err != nil {
		return "", nil, fail(err)
	}
	return dataRoot, db, 0
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	return 0
}

func installCmd() int {
	cwd, err := os.Getwd()
	if err == nil {
		err = install.Run(cwd, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe install:", err)
		return 1
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "lathe:", err)
	return 1
}

func overrideFlags(fs *flag.FlagSet) *config.Overrides {
	var ov config.Overrides
	fs.StringVar(&ov.Provider, "provider", "", "override the provider for this run")
	fs.StringVar(&ov.Model, "model", "", "override the model for this run")
	fs.StringVar(&ov.Thinking, "thinking", "", "override the thinking level for this run")
	return &ov
}
