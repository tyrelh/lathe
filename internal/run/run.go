// Package run is the orchestration primitive: a run owns its directory, its
// trace, and the phases inside it. A phase is a closure, because Go has no
// `with` and the closure is the only form that makes "the trace always closes"
// something the compiler helps with rather than something you remember.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/pi"
	"github.com/tyrelh/lathe/internal/trace"
)

// maxCorrections is how many times a bad envelope is handed back to the same
// session before the phase fails. Two is enough for a formatting slip and few
// enough that a confused agent stops burning tokens.
const maxCorrections = 2

// toolBudget is how many tool calls one agent turn may make before lathe ends
// the turn and asks for the report. It is the backstop, deliberately above the
// number prompts/scout/user.md asks the agent to keep to, so a well-behaved
// agent lands on its own and only a spiralling one ever meets this. A run that
// hit it once loops forever without it: the deadline is the only other bound,
// and the deadline produces a failure rather than an answer.
const toolBudget = 40

// errBudget means the turn was cut short by toolBudget rather than by anything
// going wrong, so the caller asks for the report instead of failing the phase.
var errBudget = errors.New("tool call budget spent")

// budgetSpent is what the agent is told when that happens. It goes to the same
// Pi session, so everything it has read is still in its context — it has to
// write the report, not repeat the investigation.
const budgetSpent = "You have used your tool call budget for this phase. " +
	"Do not call any more tools.\n\nReply now with the fenced json block, " +
	"built from what you have already read. Anything you could not determine " +
	"belongs in \"findings\" as an honest gap, phrased as what is still unknown."

// Params names a phase. Owner is the agent name from the roster, or
// "engineer" for a phase lathe performs itself.
type Params struct {
	Name, Owner string
	// SessionID continues an earlier agent phase; empty starts a new session.
	SessionID string
	// RevertOnly tolerates test-generated dirt after enforcement.
	RevertOnly bool
}

// Envelope is an agent's typed output. Validate returns what is missing —
// encoding/json zero-fills absent fields, so required ones must be pointers
// for Validate to be able to tell. Artifacts is what the gates check.
type Envelope interface {
	Validate() []string
	Artifacts() []string
}

// Failing is an Envelope that can report a terminal outcome — a report that is
// well-formed but describes a run that cannot succeed, such as a builder naming
// a file the plan never permitted. It is optional rather than part of Envelope
// because most envelopes have no such outcome, and Call consults it before any
// correction so a later reply cannot erase the failure.
type Failing interface{ Failure() error }

// EnvelopeError is an agent that ran but never returned a usable report:
// corrections exhausted, or tool calls past the budget. A workflow may absorb
// it and go round again. Anything else Call returns — a provider refusing the
// request, Pi failing to start, cancellation — is not the agent's output, and
// asking the agent again cannot fix it.
type EnvelopeError struct{ msg string }

func (e *EnvelopeError) Error() string { return e.msg }

// Gate checks an agent's claims mechanically and returns violations. It never
// judges the work — only whether what the agent said it did is true.
type Gate func(Envelope, *Run) []string

// Run is one invocation of one workflow against one repository.
type Run struct {
	ID       string
	Workflow string
	Request  string
	Issue    string // optional GitHub issue reference, resolved before planning
	Repo     string // absolute target root: what makes one global database legible
	Dir      string // per-run directory in the data root; the structured reports live here
	Work     string // per-attempt directory: raw.jsonl, the guard, the scope and the Pi session
	// Iteration is the submitted request this execution works on, counted
	// from zero. A revision's reports go in its own Dir, so none overwrites an
	// earlier iteration's.
	Iteration int
	// Revision is what a revision inherits from the run it extends; nil for
	// every other workflow.
	Revision *Revision

	// PiBin overrides the `pi` found on PATH. Tests set it; production does not.
	PiBin string
	// Out is where the closing banner goes.
	Out io.Writer

	cfg     config.Snapshot
	attempt string // binds phase and publication writes to this worker's claim
	ctx     context.Context
	db      *trace.DB
	raw     *os.File
	rawW    io.Writer // raw behind a lock: a validation group's workers stream into it at once
	guard   string    // the extension, written once and passed to every spawn
	clean   bool      // EnsureClean passed, which is what licenses a revert

	// mu guards everything below it. A validation group runs phases in
	// goroutines, and each of them numbers a phase, charges spend, settles
	// enforcement and may fail the run.
	mu sync.Mutex
	// frozen is set while a validation group runs: enforce leaves the tree
	// alone, because a reviewer's turn ending must not clean up files the
	// tester's suite is still using. The group sweeps once, after its join.
	frozen bool
	// accepted is every path an earlier phase was allowed to leave behind.
	// Enforce is handed the whole dirty tree, so without carrying this forward
	// the tester's empty allow list would revert the builder's work and then
	// fail the phase for a violation the builder committed. A run accumulates
	// permission across its phases; it does not start each one from nothing.
	accepted []string
	seq      int
	tokens   int
	cost     float64
	err      error  // the first phase failure, which decides the run's status
	status   string // settled by Finish; the worker records it
	reason   string
}

// Options is everything a Run needs from the process that owns it. The run
// row already exists: submission recorded it and a worker claimed it, so this
// only attaches execution to it. Ctx carries cancellation and database-loss
// shutdown in from the caller, which is what lets signal handling live in the
// command rather than in here.
type Options struct {
	ID       string
	Workflow string
	Request  string
	Issue    string // optional GitHub issue reference, resolved before planning
	Repo     string
	Dir      string
	Work     string
	Snapshot config.Snapshot
	DB       *trace.DB
	Ctx      context.Context
	Out      io.Writer
	PiBin    string
	// Iteration and Attempt identify this execution. With Attempt set, every
	// phase and publication write lands only while that attempt owns the run,
	// so a stale worker cannot alter a later iteration.
	Iteration int
	Attempt   string
	Revision  *Revision
}

// Revision is the run a revision extends, as the worker established it under
// its claim: the pull request, its branch, the commit last published to it,
// and every earlier iteration.
type Revision struct {
	PR     string // the pull request URL
	Branch string // its head branch, which is checked out
	Base   string // its base branch, what the branch diff is taken against
	Head   string // the commit lathe last published; the push's expected remote head
	Prior  []Prior
}

// Prior is one earlier iteration: what was asked, and where its reports are.
type Prior struct {
	Iteration int
	Request   string
	Dir       string
}

// Open prepares the run directory, the raw stream and the guard for an
// already-recorded run. The caller owns the database handle and the run's
// terminal state; Finish settles the banner and the exit code only.
func Open(o Options) (*Run, error) {
	r := &Run{
		ID:       o.ID,
		Workflow: o.Workflow,
		Request:  o.Request,
		Issue:    o.Issue,
		Repo:     o.Repo,
		Dir:      o.Dir,
		Work:     o.Work,
		PiBin:    o.PiBin,
		Out:      o.Out,
		cfg:      o.Snapshot,
		ctx:      o.Ctx,
		db:       o.DB,

		Iteration: o.Iteration,
		Revision:  o.Revision,
		attempt:   o.Attempt,
	}
	if r.Out == nil {
		r.Out = os.Stdout
	}
	if r.ctx == nil {
		r.ctx = context.Background()
	}
	if r.Work == "" {
		r.Work = r.Dir
	}
	if err := os.MkdirAll(r.Work, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return nil, err
	}
	// Phase numbers carry on from earlier iterations: an ID embeds its
	// number, and a later iteration must never reuse one.
	var err error
	if r.seq, err = r.db.MaxSeq(r.ID); err != nil {
		return nil, err
	}
	if r.raw, err = os.Create(filepath.Join(r.Work, "raw.jsonl")); err != nil {
		return nil, err
	}
	r.rawW = &lockedWriter{w: r.raw}
	// Every agent runs behind the guard, including the read-only ones: their
	// empty allow list is what makes "this agent changes nothing" a rule the
	// tool layer enforces rather than a consequence of the tool list, and the
	// --no-extensions that comes with it is what stops the target repo loading
	// its own extension into the agent reading it.
	if r.guard, err = permit.Guard(r.Work); err != nil {
		r.raw.Close()
		return nil, err
	}
	return r, nil
}

// Phase runs fn as a traced phase. The phase exists at status 'fail' from the
// moment it is created; success is earned by returning nil. The named return
// plus defer is what makes that hold through a panic too.
func (r *Run) Phase(p Params, fn func(*Handle) error) (err error) {
	r.mu.Lock()
	r.seq++
	ph := trace.NewPhase(r.ID, r.seq, p.Name, p.Owner)
	ph.Iteration, ph.Attempt = r.Iteration, r.attempt
	r.mu.Unlock()
	if err := r.db.PhaseUpsert(ph); err != nil {
		return r.fail(err)
	}
	r.db.Event(r.ID, ph.ID, "phase_start", p.Name, nil)

	h := &Handle{run: r, phase: ph, params: p}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic in phase %s: %v", p.Name, rec)
		}
		// h.failed is a failure the phase body chose to absorb: recorded here,
		// invisible to the caller, and never the run's error.
		status, msg := "success", ""
		switch {
		case err != nil:
			status, msg = "fail", err.Error()
		case h.failed != nil:
			status, msg = "fail", h.failed.Error()
		}
		if msg != "" {
			r.db.Event(r.ID, ph.ID, "error", p.Name, msg)
		}
		ph.Finish(status, msg)
		r.db.PhaseUpsert(ph)
		r.db.Event(r.ID, ph.ID, "phase_end", p.Name, map[string]string{"status": status})
		if err != nil {
			// A measured red suite stays in the timeline without poisoning a later success.
			var red *CommandFailure
			if !errors.As(err, &red) {
				r.fail(err)
			}
		}
	}()
	return fn(h)
}

// Finish settles the status, the banner and the exit code in one call, so the
// three cannot disagree. accepted is "the run produced an acceptable result",
// which is a different question from "every phase worked": red verification
// and unusable reviews can be recovered by their workflows.
//
// It does not write the run's terminal state. That write is bound to the
// worker's claim, so the worker makes it — reading Status and Reason here.
func (r *Run) Finish(accepted bool, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, code := trace.StatusOK, 0
	switch {
	case r.err != nil:
		status, code = trace.StatusFail, 1
		if reason == "" {
			reason = r.err.Error()
		}
	case !accepted:
		status, code = trace.StatusFail, 1
	}
	r.status, r.reason = status, reason
	r.raw.Close()

	// The run's own totals, which span every iteration: this execution's
	// counters would report only its own share of a revised run.
	tokens, cost := r.tokens, r.cost
	if row, err := r.db.Get(r.ID); err == nil {
		tokens, cost = row.Tokens, row.Cost
	}
	fmt.Fprintf(r.Out, "\nlathe %s %s: %s  %d tokens  $%.5f\n  %s\n",
		r.Workflow, r.ID, status, tokens, cost, r.Dir)
	if reason != "" {
		fmt.Fprintf(r.Out, "  %s\n", reason)
	}
	return code
}

// Status and Reason are what Finish settled on, for the worker to record.
func (r *Run) Status() string { return r.status }
func (r *Run) Reason() string { return r.reason }

// EnsureClean refuses to start when the target root has uncommitted changes,
// and records that it passed. The check and the record are one call because
// splitting them leaves a flag that says the tree was clean without anything
// having looked: reverting on that lie throws away work that was never the
// agent's. Only a writing workflow calls it.
func (r *Run) EnsureClean() error {
	if err := permit.Clean(r.Repo); err != nil {
		return err
	}
	r.clean = true
	return nil
}

// enforce is the post-turn half of the boundary, run after every spawn. With
// the guard in front of the tool it should find nothing a builder did; what it
// does find is either a guard bug or a test suite's leavings, and the caller
// decides which of those is fatal. It reports nothing at all when Clean did not
// pass, because the tree it would be reverting is not the agent's.
func (r *Run) enforce(h *Handle) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.clean || r.frozen {
		return nil, nil
	}
	reverted, err := r.sweep(h)
	// Tolerance is a property of the phase, so it is settled here rather than
	// at each call site: a later phase that needs it gets it by declaring it,
	// not by remembering to repeat the check.
	if err != nil || h.params.RevertOnly {
		return nil, err
	}
	return reverted, nil
}

// sweep reverts whatever neither h's scope nor an earlier phase accepted, and
// reports what it reverted. The caller holds mu.
func (r *Run) sweep(h *Handle) ([]string, error) {
	scope := h.permit()
	scope.Allow = append(append([]string{}, scope.Allow...), r.accepted...)

	kept, reverted, err := permit.Enforce(r.Repo, scope)
	if err != nil {
		return nil, err
	}
	// kept is self-pruning: a path an earlier phase wrote and a later one put
	// back is no longer dirty, so it drops out rather than being protected
	// forever.
	r.accepted = kept
	r.db.Event(r.ID, h.phase.ID, "permit", h.phase.Name,
		map[string]any{"kept": kept, "reverted": reverted})
	return reverted, nil
}

// Shipped records the branch and commit a run's work landed on, for a workflow
// that commits: the run row otherwise still names what it started from.
func (r *Run) Shipped(branch, sha string) error { return r.db.SetShipped(r.ID, r.attempt, branch, sha) }

// Evidence records the commit this iteration made and the branch's remote
// head when last verified, without moving the run's shipped commit.
func (r *Run) Evidence(commit, remote string) error {
	return r.db.SetEvidence(r.ID, r.attempt, commit, remote)
}

// Protected is the roster's deny list, which the plan gate checks a file list
// against before a builder is ever handed one.
func (r *Run) Protected() []string { return r.cfg.Protected }

// Tokens and Cost are what the run has spent so far.
func (r *Run) Tokens() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens
}

func (r *Run) Cost() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cost
}

func (r *Run) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
	return err
}

// lockedWriter serialises writes to raw.jsonl. pi.Scan writes one whole line
// per call, so a lock per write keeps concurrent workers' lines whole.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Handle is what a phase body is given: the only way to write to the trace or
// to call an agent, and both are already scoped to this phase.
type Handle struct {
	run    *Run
	phase  *trace.Phase
	allow  []string
	params Params
	failed error // recorded as a failed phase, but not returned to the caller
	// corrections is the correction allowance Call draws on. Nil is the usual
	// maxCorrections per call; a group worker shares one allowance across its
	// retries, so retries and corrections add rather than multiply.
	corrections *int
}

// SessionID is stable across correction and fix turns.
func (h *Handle) SessionID() string {
	if h.params.SessionID != "" {
		return h.params.SessionID
	}
	return h.phase.ID
}

// Scope is the exact set of repo-relative paths this phase's agent may write.
// Unset, it permits nothing, which is the right default for every agent that
// has no write tool and the only safe one for an agent that does.
func (h *Handle) Scope(allow []string) { h.allow = allow }

// permit is the whole boundary for one phase: its own allow list, plus the
// roster's deny lists. A phase chooses what it may write; what nobody may write
// is not a phase's to pick, so it is not in Scope's signature either.
func (h *Handle) permit() permit.Scope {
	return permit.Scope{
		Allow:    h.allow,
		Deny:     h.run.cfg.Protected,
		BashDeny: h.run.cfg.BashDenied,
	}
}

// Log records something lathe itself did, with no agent involved.
func (h *Handle) Log(name, payload string) error {
	return h.run.db.Event(h.run.ID, h.phase.ID, "log", name, payload)
}

// Call runs the phase's agent, decodes its envelope into out, and checks it
// against Validate plus every gate. A failure is handed back to the *same Pi
// session* as a correction: the agent still has its own context, so it fixes
// its output rather than redoing the investigation.
func (h *Handle) Call(out Envelope, request string, gates ...Gate) error {
	r := h.run
	agent, err := r.cfg.Resolve(h.phase.Owner)
	if err != nil {
		return err
	}

	prompt := agent.Prompt(request)
	nudged := false
	left := h.corrections
	if left == nil {
		n := maxCorrections
		left = &n
	}
	for attempt := 0; ; attempt++ {
		res, err := r.spawn(h, agent, prompt, attempt)
		// Pi may report a killed process instead of the context's error. Keep
		// run cancellation recognizable to workflows with recoverable phases.
		if ctxErr := r.ctx.Err(); err != nil && ctxErr != nil {
			return ctxErr
		}
		switch {
		case errors.Is(err, errBudget) && !nudged:
			nudged, prompt = true, budgetSpent
			continue
		case errors.Is(err, errBudget):
			return &EnvelopeError{fmt.Sprintf("%s kept calling tools after its budget was spent", agent.Name)}
		case err != nil:
			return err
		}

		violations := decode(res.Text, out)
		// A terminal failure is not an output-format slip, so it is checked
		// ahead of Validate: a reply that reports one and also forgets a key
		// must end the phase, not be corrected into a reply that no longer
		// reports it. No guard on violations, because decode zeroes out before
		// it parses — a reply that did not parse reports no failure here.
		if failed, ok := out.(Failing); ok {
			if err := failed.Failure(); err != nil {
				r.db.Event(r.ID, h.phase.ID, "envelope", agent.Name, out)
				return err
			}
		}
		if len(violations) == 0 {
			violations = out.Validate()
		}
		// Rejected replies explain corrections. Validated replies are recorded
		// below as phase outputs, after the mechanical gates pass.
		envelope := map[string]any{"attempt": attempt, "violations": violations}
		if len(violations) > 0 {
			envelope["text"] = res.Text
		}
		r.db.Event(r.ID, h.phase.ID, "envelope", agent.Name, envelope)

		if len(violations) == 0 {
			var gateViolations []string
			for _, g := range gates {
				gateViolations = append(gateViolations, g(out, r)...)
			}
			gate := map[string]any{"attempt": attempt, "violations": gateViolations}
			if len(gateViolations) > 0 {
				gate["text"] = res.Text
			}
			r.db.Event(r.ID, h.phase.ID, "gate", agent.Name, gate)
			if len(gateViolations) == 0 {
				// Keep the report and exact response together so the dashboard can
				// show each phase's result, including plans later revised. A final
				// plan.json alone cannot reconstruct that history.
				return r.db.Event(r.ID, h.phase.ID, "output", agent.Name, map[string]any{
					"attempt": attempt, "report": out, "text": res.Text,
				})
			}
			violations = gateViolations
		}

		if *left <= 0 {
			return &EnvelopeError{fmt.Sprintf("%s did not produce a valid envelope after %d corrections: %s",
				agent.Name, attempt, strings.Join(violations, "; "))}
		}
		*left--
		prompt = correction(violations)
	}
}

// spawn is one Pi invocation. Corrections and fix phases reuse the original
// session id, which keeps the builder context across the red loop.
func (r *Run) spawn(h *Handle, a config.Resolved, prompt string, attempt int) (pi.Result, error) {
	// Rewritten before every spawn rather than once per run: each agent's allow
	// list differs, and the guard re-reads the file per tool call. One file per
	// phase, because a validation group's workers are spawned at once.
	scope := h.permit()
	scopeDir := filepath.Join(r.Work, h.phase.ID)
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		return pi.Result{}, err
	}
	scopeFile, err := permit.Write(scopeDir, scope)
	if err != nil {
		return pi.Result{}, err
	}

	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	if a.Deadline > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, a.Deadline)
		defer stop()
	}
	calls := 0

	opts := pi.Options{
		Bin:          r.PiBin,
		Dir:          r.Repo,
		Provider:     a.Provider,
		Model:        a.Model,
		Thinking:     a.Thinking,
		Tools:        a.Tools,
		SystemPrompt: a.SystemPrompt,
		SessionID:    h.SessionID(),
		SessionDir:   r.Work, // the session is thrown away with the attempt that made it
		Extension:    r.guard,
		Env:          []string{"LATHE_PERMIT=" + scopeFile},
		Raw:          r.rawW,
		OnStart: func(pid int) {
			r.db.Event(r.ID, h.phase.ID, "log", "pi_pid", map[string]int{"pid": pid})
		},
	}

	// Recorded off the options actually passed to Pi, so the trace cannot drift
	// from what was sent. Session history stays in Pi's transcript; this is the
	// input we supply, including corrections and resumed fix prompts.
	r.db.Event(r.ID, h.phase.ID, "input", a.Name, map[string]any{
		"attempt": attempt, "system": opts.SystemPrompt, "prompt": prompt,
		"session_id": opts.SessionID, "allow": scope.Allow,
	})

	// Spend is persisted per completed model response rather than per attempt,
	// so an interrupted run keeps what it already spent and the dashboard sees
	// a live run's cost move. seq is assigned before the write and never
	// reused, which is what makes the response's identity stable.
	seq := 0
	var usageErr error
	res, err := pi.Run(ctx, opts, prompt, func(ev pi.Event) bool {
		// ponytail: this writes to SQLite while Pi's stdout is not being read,
		// so a contended database back-pressures the agent for up to
		// busy_timeout. The fix, if a run ever actually stalls here, is a
		// channel in front of the tracer rather than anything in this handler.
		if ev.Type == "message_end" {
			// Streaming partial updates are message_update, not message_end, so
			// they never reach here; a response that ended in a tool call does,
			// and it spent real money.
			if m, err := ev.DecodeMessage(); err == nil && m.Role == "assistant" && m.Usage != nil {
				seq++
				if err := r.db.RecordUsage(trace.Usage{
					RunID: r.ID, PhaseID: h.phase.ID, Agent: a.Name,
					Attempt: attempt, Seq: seq,
					Provider: a.Provider, Model: a.Model,
					Tokens: m.Usage.TotalTokens, Cost: m.Usage.Cost.Total,
				}); err != nil {
					usageErr = errors.Join(usageErr, err)
				}
			}
		}
		if ev.Type == "tool_execution_end" {
			payload := map[string]any{
				"id": ev.ToolCallID, "args": ev.Args, "isError": ev.IsError,
			}
			// Keep the reason for a blocked call in the queryable trace. Full
			// successful tool output remains in raw.jsonl rather than duplicated.
			if ev.IsError {
				payload["result"] = ev.Result
			}
			r.db.Event(r.ID, h.phase.ID, "tool_call", ev.ToolName, payload)
			// Ending the read is what ends the turn: killing Pi alone leaves
			// its own tool children holding the pipe open. The session on disk
			// keeps the context, so the next prompt resumes it rather than
			// starting cold.
			if calls++; calls == toolBudget {
				r.db.Event(r.ID, h.phase.ID, "log", "budget_spent",
					map[string]int{"attempt": attempt, "calls": calls})
				cancel()
				return false
			}
		}
		return true
	})

	// The stream's own totals stay the CLI's accounting and a consistency check
	// against what was persisted; the database was charged response by response
	// as the stream ran, so nothing is written here.
	r.mu.Lock()
	r.tokens += res.Tokens
	r.cost += res.Cost
	r.mu.Unlock()

	// Enforced before the envelope is read, and after a failed turn too: the
	// turn that ended badly is the one most likely to have left something
	// behind, and a correction round must not build on it. A revert outranks
	// whatever else went wrong, including the tool budget, because it means the
	// guard did not hold — that is not a thing to nudge the agent about.
	reverted, enforceErr := r.enforce(h)
	switch {
	case len(reverted) > 0:
		return res, fmt.Errorf("%s wrote outside its scope; reverted %s",
			a.Name, strings.Join(reverted, ", "))
	case enforceErr != nil:
		return res, enforceErr
	// Losing a write is losing money from the record, so it fails the phase
	// rather than leaving the dashboard quietly short.
	case usageErr != nil:
		return res, fmt.Errorf("recording %s spend: %w", a.Name, usageErr)
	}

	// A killed Pi reports as a failed command, so the budget has to claim its
	// own cancellation before the error is read as one.
	if err != nil && calls >= toolBudget {
		return res, errBudget
	}
	// Returned as an error rather than an empty reply, so Call stops here
	// instead of spending its corrections asking a missing model again.
	if err == nil && res.Failure != "" {
		return res, fmt.Errorf("%s (%s/%s): provider error: %s", a.Name, a.Provider, a.Model, res.Failure)
	}
	return res, err
}

// jsonFence matches a fenced json block. The agent is asked to end its reply
// with one, so the last match is the envelope.
var jsonFence = regexp.MustCompile("(?s)```json\\s*\\n(.*?)```")

// decode parses the last fenced json block into out, zeroing it first so a
// field left over from a rejected attempt cannot stand in for a missing one.
func decode(text string, out Envelope) []string {
	reflect.ValueOf(out).Elem().SetZero()
	m := jsonFence.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return []string{"your reply contained no fenced ```json block"}
	}
	if err := json.Unmarshal([]byte(m[len(m)-1][1]), out); err != nil {
		return []string{"your json block did not parse: " + err.Error()}
	}
	return nil
}

func correction(violations []string) string {
	return "Your last reply did not satisfy the output contract:\n- " +
		strings.Join(violations, "\n- ") +
		"\n\nReply with the corrected fenced json block and nothing after it. " +
		"Do not repeat the investigation; you already have the answers."
}

// ArtifactsExist checks that every path the agent claimed it wrote is really
// there. Relative paths are resolved against the target root.
func ArtifactsExist(e Envelope, r *Run) []string {
	var v []string
	for _, p := range e.Artifacts() {
		if _, err := os.Stat(r.resolve(p)); err != nil {
			v = append(v, fmt.Sprintf("you listed %q as an artifact, but no such file exists", p))
		}
	}
	return v
}

// FilesNonEmpty checks that those files are not zero bytes. A missing file is
// ArtifactsExist's to report, so it is skipped here rather than counted twice.
func FilesNonEmpty(e Envelope, r *Run) []string {
	var v []string
	for _, p := range e.Artifacts() {
		if fi, err := os.Stat(r.resolve(p)); err == nil && fi.Size() == 0 {
			v = append(v, fmt.Sprintf("you listed %q as an artifact, but it is empty", p))
		}
	}
	return v
}

func (r *Run) resolve(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(r.Repo, p)
}
