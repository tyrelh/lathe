package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// routeCalibrate is the hidden route-calibrate command, run from a target repo.
func routeCalibrate(args []string) int {
	fs := flag.NewFlagSet("route-calibrate", flag.ExitOnError)
	agent := fs.String("agent", "", "the routed agent to calibrate: planner or builder")
	limit := fs.Int("limit", 30, "how many past runs to ask about")
	fs.Parse(args)
	if *agent != "planner" && *agent != "builder" {
		fmt.Fprintln(os.Stderr, "lathe route-calibrate: --agent must be planner or builder")
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	dataRoot, db, code := openData()
	if db == nil {
		return code
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := calibrate(ctx, os.Stdout, db, dataRoot, cwd, *agent, *limit); err != nil {
		return fail(err)
	}
	return 0
}

// calibrate asks Jev about up to limit of dir's repo's past runs exactly as
// agent's routing phase would, using the tiers in its lathe.toml, and writes a
// table of each pick beside the run's plan send-backs and builder repairs,
// with a blank column for a hand label. Only each run's first iteration is
// asked about, and a builder sample needs that iteration's plan.json.
func calibrate(ctx context.Context, w io.Writer, db *trace.DB, dataRoot, dir, agent string, limit int) error {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return errors.New("TYPESAFE_API_KEY is not set")
	}
	root, err := config.TargetRoot(dir)
	if err != nil {
		return err
	}
	snap, err := capture("build", root, config.Overrides{}, io.Discard)
	if err != nil {
		return err
	}
	rt, ok := snap.Routing[agent]
	if !ok {
		return fmt.Errorf("%s gives %s no tiers", filepath.Join(root, "lathe.toml"), agent)
	}
	ws, err := workspace.Local(root)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s tiers, confidence floor %.2f:\n", agent, snap.ConfidenceFloor)
	for i, t := range rt.Tiers {
		fmt.Fprintf(w, "  %d  %s/%s  %s\n", i, t.Resolved.Provider, t.Resolved.Model, t.When)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tWORKFLOW\tTIER\tCONFIDENCE\tREASON\tSEND-BACKS\tREPAIRS\tEXPECTED\tREQUEST")

	cost, sampled := 0.0, 0
	var beforeAt, beforeID string
	for sampled < limit {
		rows, more, err := db.ProjectRuns(ws.Path, beforeAt, beforeID, 50)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if sampled == limit {
				break
			}
			s, ok, err := sample(db, dataRoot, row, agent)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			sampled++
			d := run.Decide(ctx, key, rt, snap.ConfidenceFloor, s.state)
			if err := ctx.Err(); err != nil {
				return err
			}
			confidence := "-"
			if d.Err != nil {
				fmt.Fprintf(os.Stderr, "lathe: %s: %v\n", row.ID, d.Err)
			} else {
				confidence = fmt.Sprintf("%.2f", d.Answer.Confidence)
				cost += d.Answer.Cost()
			}
			fmt.Fprintf(tw, "%s\t%s\t%d %s\t%s\t%s\t%d\t%s\t\t%s\n",
				row.ID, row.Workflow, d.Tier, rt.Tiers[d.Tier].Resolved.Model, confidence, d.Reason,
				s.sendBacks, s.repairs, clip(s.request, 60))
		}
		if !more || len(rows) == 0 {
			break
		}
		beforeAt, beforeID = rows[len(rows)-1].Activity, rows[len(rows)-1].ID
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d runs, $%.5f\n", sampled, cost)
	return nil
}

// calibration is one past run as its routing phase would have seen it.
type calibration struct {
	request   string
	state     any
	sendBacks int
	repairs   string
}

// sample rebuilds row's first-iteration routing state for agent and counts its
// plan send-backs and builder repairs. ok is false for a run with nothing to
// route: a scout, or for the builder a run with no usable plan.json.
func sample(db *trace.DB, dataRoot string, row trace.Row, agent string) (calibration, bool, error) {
	var c calibration
	switch {
	case row.Workflow == "scout":
		return c, false, nil
	case agent == "builder" && row.Workflow == "plan":
		return c, false, nil
	}
	its, err := db.Iterations(row.ID)
	if err != nil {
		return c, false, err
	}
	c.request = row.Request
	if len(its) > 0 {
		c.request = its[0].Request
	}
	dir := worker.ReportDir(dataRoot, row.ID, 0)
	var issue workspace.Issue
	if b, err := os.ReadFile(filepath.Join(dir, "issue.json")); err == nil && json.Unmarshal(b, &issue) == nil {
		c.request = issue.Request()
	}

	if agent == "planner" {
		c.state = workflow.PlanRouteState(c.request)
	} else {
		var plan workflow.PlanOutput
		b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err != nil || json.Unmarshal(b, &plan) != nil ||
			plan.Summary == nil || plan.Steps == nil || plan.Files == nil || plan.Risks == nil {
			return c, false, nil
		}
		c.state = workflow.BuildRouteState(c.request, &plan)
	}

	phases, err := db.Phases(row.ID)
	if err != nil {
		return c, false, err
	}
	plans, implements := 0, 0
	for _, ph := range phases {
		switch {
		case ph.Iteration != 0:
		case ph.Name == "plan":
			plans++
		case ph.Name == "implement":
			implements++
		}
	}
	c.sendBacks, c.repairs = max(plans-1, 0), "-"
	if implements > 0 {
		c.repairs = fmt.Sprint(implements - 1)
	}
	return c, true, nil
}
