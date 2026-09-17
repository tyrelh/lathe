// Command lathe orchestrates bounded agent work in whatever repo you invoke it from.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/tyrelh/lathe/dashboard"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/install"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
)

const usage = `lathe — a software factory you run from any repo

usage: lathe <command> [flags] [args]

commands:
  scout "<request>"   investigate the current repo and report what is there
  runs                list recent runs, from every repo
  dash                serve the run dashboard on http://127.0.0.1:4700
  install             link this repo into each agent's skills directory
  help                show this message

flags (before the request, as Go's flag package stops at the first argument):
  --repo <dir>        act on this repository instead of the working directory
  --provider <name>   override the provider for this run
  --model <name>      override the model for this run
  --thinking <level>  override the thinking level; ignored by models without one

Run install from a checkout of the lathe repo; it links the checkout itself,
so SKILL.md stays discoverable.
`

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 2
	}
	switch cmd := args[0]; cmd {
	case "scout":
		return scout(args[1:])
	case "runs":
		return runs(args[1:])
	case "dash":
		return dash()
	case "install":
		return installCmd()
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "lathe: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// scout resolves both roots and the roster before anything is spawned, so an
// unknown model or a directory outside a git repo fails in under a second
// rather than after an agent turn.
func scout(args []string) int {
	fs := flag.NewFlagSet("scout", flag.ExitOnError)
	repo := fs.String("repo", "", "repository to act on (default: the git root of the working directory)")
	ov := overrideFlags(fs)
	fs.Parse(args)

	request := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if request == "" {
		fmt.Fprint(os.Stderr, "lathe scout: needs a request, e.g. lathe scout \"where does auth live\"\n")
		return 2
	}

	dir := *repo
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "lathe:", err)
			return 1
		}
		dir = cwd
	}
	root, err := config.TargetRoot(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	cfg, err := config.Load(Assets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	// Resolving now turns an unknown agent, a bad timeout or a missing prompt
	// into an error before a run row exists.
	if _, err := cfg.Resolve("scout", *ov); err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	return workflow.Scout(cfg, *ov, root, request)
}

// runs lists what is in the one global database. Repos show as basenames,
// which is what makes a cross-project list readable.
func runs(args []string) int {
	fs := flag.NewFlagSet("runs", flag.ExitOnError)
	n := fs.Int("n", 20, "how many runs to show")
	fs.Parse(args)

	dataRoot, err := trace.DataRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	db, err := trace.Open(dataRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	defer db.Close()

	rows, err := db.Recent(*n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("no runs yet")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STARTED\tSTATUS\tWORKFLOW\tREPO\tTOKENS\tCOST\tRUN")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t$%.5f\t%s\n",
			r.Started, r.Status, r.Workflow, filepath.Base(r.Repo), r.Tokens, r.Cost, r.ID)
	}
	return flush(w)
}

// dash serves the trace read-only in the foreground; Ctrl-C stops it. The bind
// address is a literal in the dashboard package, not a flag: the server has no
// authentication.
func dash() int {
	dataRoot, err := trace.DataRoot()
	if err == nil {
		err = dashboard.Serve(dataRoot, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe dash:", err)
		return 1
	}
	return 0
}

func flush(w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
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

func overrideFlags(fs *flag.FlagSet) *config.Overrides {
	var ov config.Overrides
	fs.StringVar(&ov.Provider, "provider", "", "override the provider for this run")
	fs.StringVar(&ov.Model, "model", "", "override the model for this run")
	fs.StringVar(&ov.Thinking, "thinking", "", "override the thinking level for this run")
	return &ov
}
