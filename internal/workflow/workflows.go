package workflow

import (
	"slices"

	"github.com/tyrelh/lathe/internal/run"
)

// Workflow names. They are persisted in specs and run rows and are the CLI's
// subcommands, so they never change.
const (
	NameScout     = "scout"
	NamePlan      = "plan"
	NameImplement = "implement"
	NameBuild     = "build"
	NameRevise    = "revise"
)

// def is one workflow: its graph, every agent it will spawn, in roster order,
// and whether it changes the checkout it runs in.
type def struct {
	name   string
	graph  func(*run.Run) int
	agents []string
	writes bool
}

// defs is every workflow, in the order they are listed.
var defs = []def{
	{NameScout, Scout, []string{"scout"}, false},
	{NamePlan, Plan, []string{"planner", "plan-reviewer"}, false},
	{NameImplement, Implement, validated([]string{"planner", "plan-reviewer", "builder"}), true},
	{NameBuild, Build, validated([]string{"planner", "plan-reviewer", "brancher", "builder"}, "committer", "pr-author"), true},
	// A revision extends a build's branch and pull request, so it names and
	// opens neither.
	{NameRevise, Revise, validated([]string{"planner", "plan-reviewer", "builder"}, "committer"), true},
}

// validated is before, then the validation group and adjudicator, then after.
func validated(before []string, after ...string) []string {
	out := append(slices.Clone(before), "tester")
	out = append(out, reviewers...)
	out = append(out, "adjudicator")
	return append(out, after...)
}

// Graphs is every workflow by name: what the worker executes, and the list a
// submission validates a command against.
var Graphs = func() map[string]func(*run.Run) int {
	m := map[string]func(*run.Run) int{}
	for _, d := range defs {
		m[d.name] = d.graph
	}
	return m
}()

// Names is every workflow in Graphs, in the order they are listed.
var Names = func() []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.name)
	}
	return out
}()

// Writes is every workflow that changes the checkout it runs in. It is what
// decides which runs need a clean tree, which own the checkout while they run,
// and which exclude each other — so those three questions are asked of the
// workflow list rather than of a literal workflow name in three packages.
var Writes = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range defs {
		if d.writes {
			m[d.name] = true
		}
	}
	return m
}()

// Agents is every agent each graph will spawn. Submission resolves all of
// them before recording a run, so an unknown agent or a bad prompt fails in
// under a second rather than after an agent turn — and the resolved roster is
// what the run executes from.
var Agents = func() map[string][]string {
	m := map[string][]string{}
	for _, d := range defs {
		m[d.name] = d.agents
	}
	return m
}()

// Submittable reports whether name is a workflow a new run can be submitted
// as. A revision is submitted against an existing run instead.
func Submittable(name string) bool { return name != NameRevise && Graphs[name] != nil }

// Publishes reports whether name pushes a branch and owns a pull request.
func Publishes(name string) bool { return name == NameBuild || name == NameRevise }
