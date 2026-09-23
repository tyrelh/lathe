// Command lathe orchestrates bounded agent work in whatever repo you invoke it from.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/install"
	"github.com/tyrelh/lathe/internal/manager"
	"github.com/tyrelh/lathe/internal/permit"
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
  implement "<req>"   plan, implement and test a change; leave it uncommitted
  build "<request>"   implement it, then branch, commit and open a pull request
  manager             run the scheduler and the dashboard in the foreground
  runs                list recent runs, from every repo
  show <id>           state, outcome, reports and artifact locations for one run
  wait <id>           block until a run is terminal; exit with its outcome
  cancel <id>         ask a run to stop
  install             link this repo into each agent's skills directory
  version             print the version of this binary
  help                show this message

flags (before the request, as Go's flag package stops at the first argument):
  --repo <dir>        act on this repository instead of the working directory
  --detach            record the request, print the run ID and exit
  --provider <name>   override the provider for this run
  --model <name>      override the model for this run
  --thinking <level>  override the thinking level; ignored by models without one

Every command records a run and then blocks until it finishes. The work happens
in a worker process, so interrupting the wait detaches from the run rather than
cancelling it; use lathe cancel for that. A manager is started automatically if
none is running, and serves the dashboard.

implement and build need a clean checkout and own it until they stop. build
leaves the checkout on the branch it created, and a failure after its commit
leaves that commit there rather than losing the work.

environment:
  Workers inherit the manager's environment: credentials such as
  MOONSHOT_API_KEY, PATH, test settings. An auto-started manager takes the
  environment of the submission that started it; restart it to change that.
  LATHE_CAPACITY      how many runs may execute at once (default 1)

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
	switch cmd := args[0]; cmd {
	case "scout", "plan", "implement", "build":
		return submit(cmd, args[1:])
	case "manager":
		return managerCmd()
	case "worker":
		return workerCmd(args[1:])
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
	ov := overrideFlags(fs)
	fs.Parse(args)

	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		fmt.Fprintf(os.Stderr, "lathe %s: needs a request, e.g. lathe %s %q\n",
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
	root, err := config.TargetRoot(dir)
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(Assets)
	if err != nil {
		return fail(err)
	}
	// The target's lathe.toml is read here, at submission, so its values are
	// frozen into the snapshot with everything else. A broken file is a
	// warning, not a failure: the run goes ahead on the roster.
	if loaded, err := cfg.LoadProject(root); err != nil {
		fmt.Fprintln(os.Stderr, "lathe: ignoring", err)
	} else if loaded {
		fmt.Fprintln(os.Stderr, "lathe: loaded", filepath.Join(root, "lathe.toml"))
	}
	// Resolving now turns an unknown agent, a bad timeout or a missing prompt
	// into an error before a run row exists — and the resolved roster is what
	// the worker executes from, so editing a prompt cannot change queued work.
	roster, err := cfg.Capture(workflow.Agents[name], *ov)
	if err != nil {
		return fail(err)
	}
	ws, err := workspace.Local(root)
	if err != nil {
		return fail(err)
	}
	branch, err := workspace.Branch(ws.Path)
	if err != nil {
		return fail(err)
	}
	// Checked here for early feedback and again when the worker starts; a
	// checkout that goes dirty in between fails the run without discarding
	// anything.
	if workflow.Writes[name] {
		if err := permit.Clean(ws.Path); err != nil {
			return fail(err)
		}
	}

	spec, err := worker.Spec{
		Version: worker.SpecVersion, Workflow: name, Request: request,
		Workspace: ws, Roster: roster, Overrides: *ov,
	}.Marshal()
	if err != nil {
		return fail(err)
	}

	dataRoot, err := trace.DataRoot()
	if err != nil {
		return fail(err)
	}
	db, err := trace.Open(dataRoot)
	if err != nil {
		return fail(err)
	}
	id, err := db.Submit(trace.Request{
		Workflow: name, Repo: ws.Path, Request: request, Branch: branch, Spec: spec,
		Exclusive: workflow.Writes[name],
	})
	if err != nil {
		db.Close()
		return fail(err)
	}
	if err := manager.Start(dataRoot); err != nil {
		fmt.Fprintln(os.Stderr, "lathe: starting a manager:", err)
	}
	if workflow.Writes[name] {
		fmt.Printf("lathe %s %s: %s belongs to lathe until this run stops.\n"+
			"  Do not edit files or switch branches there; cleanup can revert changes made during the run.\n",
			name, id, ws.Path)
		if name == "build" {
			fmt.Printf("  It ends on the branch it creates, and a failure after its commit leaves that commit there.\n")
		}
	}
	fmt.Printf("%s %s queued\n", name, id)
	if *detach {
		db.Close()
		return 0
	}
	defer db.Close()
	return block(db, dataRoot, id, false)
}

// block waits for a terminal state and prints the run's report. Interrupting
// it detaches: a detached run is a success of the command that was typed, so
// the exit code is 0 and a shell chain keeps going.
func block(db *trace.DB, dataRoot, id string, asJSON bool) int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		row, err := db.Get(id)
		if err != nil {
			return fail(err)
		}
		if trace.Terminal(row.Status) {
			return report(row, dataRoot, asJSON)
		}
		select {
		case <-sig:
			fmt.Printf("\ndetached from %s; it keeps running\n  lathe wait %s\n  lathe cancel %s\n", id, id, id)
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
		return 130
	default:
		return 1
	}
}

// reportFiles are the structured results a workflow leaves in its run
// directory, in the order they are produced. build.json is what the builder's
// report was called before the rename, kept so lathe show still reads runs from
// before it.
var reportFiles = []string{"result.json", "plan.json", "implement.json", "build.json", "test.json", "pr.json"}

// report prints what a finished run produced, including the partial reports
// of one that failed, was cancelled, or was lost.
func report(row trace.Row, dataRoot string, asJSON bool) int {
	dir := filepath.Join(dataRoot, "runs", row.ID)
	var present []string
	for _, name := range reportFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			present = append(present, name)
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"run": row, "dir": dir, "reports": present})
		return outcome(row.Status)
	}

	fmt.Printf("\nlathe %s %s: %s  %d tokens  $%.5f\n  %s\n",
		row.Workflow, row.ID, row.Status, row.Tokens, row.Cost, dir)
	if row.Reason != "" {
		fmt.Printf("  %s\n", row.Reason)
	}
	if row.Commit != "" {
		fmt.Printf("  %s @ %s\n", row.Branch, row.Commit[:min(8, len(row.Commit))])
	}
	for _, name := range present {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var out struct {
			Summary string `json:"summary"`
		}
		json.Unmarshal(b, &out)
		fmt.Printf("  %s  %s\n", name, out.Summary)
	}
	return outcome(row.Status)
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
	if err := manager.Run(ctx, dataRoot, version, os.Stdout); err != nil {
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

// runs lists what is in the one global database, queued and active runs
// included. Repos show as basenames, which is what makes a cross-project list
// readable. Queue time and execution time are separate columns: a run that sat
// waiting is not a run that took a long time.
func runs(args []string) int {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	n := fs.Int("n", 20, "how many runs to show")
	fs.Parse(args)

	dataRoot, err := trace.DataRoot()
	if err != nil {
		return fail(err)
	}
	db, err := trace.Open(dataRoot)
	if err != nil {
		return fail(err)
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
	fmt.Fprintln(w, "SUBMITTED\tSTATUS\tWORKFLOW\tREPO\tQUEUED\tRAN\tTOKENS\tCOST\tRUN")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t$%.5f\t%s\n",
			r.Submitted, r.Status, r.Workflow, filepath.Base(r.Repo),
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
	return report(row, dataRoot, *asJSON)
}

func waitCmd(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the result as JSON")
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
	return block(db, dataRoot, id, *asJSON)
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

func openData() (string, *trace.DB, int) {
	dataRoot, err := trace.DataRoot()
	if err != nil {
		return "", nil, fail(err)
	}
	db, err := trace.Open(dataRoot)
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
