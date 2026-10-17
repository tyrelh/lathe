package worker

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// SpecVersion is stamped into every recorded specification. A worker that
// does not recognise the version fails the run rather than guessing at it.
// Version 4 added Base: a writing run executes in a worktree at that commit.
const SpecVersion = 4

// legacyVersion is the last version whose writing runs executed in the
// source checkout. Its read-only runs still execute; its writing runs are
// refused, since nothing recorded which commit they should start from.
const legacyVersion = 3

// RecoveryVersion is stamped into a recovery's specification. It is a v4
// specification plus Recovery, which this lathe requires at that version.
const RecoveryVersion = 5

// Spec is everything about a run that configuration could otherwise change
// while it waits in the queue: the workflow, the request, where it executes
// and the effective roster. Repository contents are not in it — a worker reads
// files when it starts — and neither are the worker's secrets.
type Spec struct {
	Version   int              `json:"version"`
	Workflow  string           `json:"workflow"`
	Request   string           `json:"request"`
	Issue     string           `json:"issue,omitempty"`
	Workspace workspace.Desc   `json:"workspace"`
	Roster    config.Snapshot  `json:"roster"`
	Overrides config.Overrides `json:"overrides"`
	// Base is the commit a writing run's worktree starts from: HEAD at
	// submission, or the shipped commit a revision extends.
	Base string `json:"base,omitempty"`
	// Recovery is set, at RecoveryVersion only, on an engineer's recovery
	// of a failed, cancelled or lost iteration.
	Recovery *RecoverySpec `json:"recovery,omitempty"`
}

// Mode is the execution mode a recorded specification dispatches with:
// trace.ModeWorktree for a current writing run, trace.ModeCheckout for a
// read-only one, and trace.ModeReject for a writing run this lathe refuses.
// A specification it cannot read is trace.ModeCheckout.
func Mode(spec []byte) string {
	var s Spec
	if json.Unmarshal(spec, &s) != nil {
		return trace.ModeCheckout
	}
	mode, _ := s.mode("")
	if mode == trace.ModeReject && !workflow.Writes[s.Workflow] {
		return trace.ModeCheckout
	}
	return mode
}

// mode is Mode with the refusal for runID.
func (s Spec) mode(runID string) (string, error) {
	writes := workflow.Writes[s.Workflow]
	current := s.Version == SpecVersion || (s.Version == RecoveryVersion && s.Recovery != nil)
	switch {
	case current && writes:
		return trace.ModeWorktree, nil
	case (current || s.Version == legacyVersion) && !writes:
		return trace.ModeCheckout, nil
	case s.Version == legacyVersion:
		return trace.ModeReject, fmt.Errorf("run %s was queued by an older lathe (spec v%d) that ran %s in your checkout; "+
			"this lathe runs it in a worktree of its own and will not guess which commit to start from. "+
			"Nothing was changed; submit it again", runID, s.Version, s.Workflow)
	}
	return trace.ModeReject, fmt.Errorf("run %s was recorded by an incompatible lathe (spec v%d)", runID, s.Version)
}

// Models is the provider and model a recorded specification assigned each
// agent, which is the part of an iteration's configuration a reader compares.
// An unreadable specification has none.
func Models(spec []byte) map[string]string {
	var s Spec
	if json.Unmarshal(spec, &s) != nil {
		return nil
	}
	models := map[string]string{}
	for name, a := range s.Roster.Agents {
		models[name] = a.Provider + "/" + a.Model
	}
	return models
}

// IterationView is one iteration with the models its recorded configuration
// assigned each agent.
type IterationView struct {
	trace.Iteration
	Models map[string]string `json:"models"`
}

// IterationViews returns its as IterationViews, never nil.
func IterationViews(its []trace.Iteration) []IterationView {
	views := make([]IterationView, len(its))
	for i, it := range its {
		views[i] = IterationView{it, Models(it.Spec)}
	}
	return views
}

// protectedPaths is the protected-path list a recorded specification carries.
func protectedPaths(spec []byte) []string {
	var s Spec
	if json.Unmarshal(spec, &s) != nil {
		return nil
	}
	return s.Roster.Protected
}

// Marshal is how the submitter records a specification.
func (s Spec) Marshal() ([]byte, error) { return json.Marshal(s) }

func unmarshalSpec(b []byte, s *Spec) error {
	if len(b) == 0 {
		return errors.New("this run has no recorded execution specification")
	}
	return json.Unmarshal(b, s)
}
