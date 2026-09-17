// Package run is the orchestration primitive: a run owns its directory, its
// trace, and the phases inside it. A phase is a closure, because Go has no
// `with` and the closure is the only form that makes "the trace always closes"
// something the compiler helps with rather than something you remember.
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/pi"
	"github.com/tyrelh/lathe/internal/trace"
)

// maxCorrections is how many times a bad envelope is handed back to the same
// session before the phase fails. Two is enough for a formatting slip and few
// enough that a confused agent stops burning tokens.
const maxCorrections = 2

// Params names a phase. Kind is engineer | agent | code; Owner is the agent
// name from the roster, or "engineer" for a phase lathe performs itself.
type Params struct{ Name, Kind, Owner string }

// Envelope is an agent's typed output. Validate returns what is missing —
// encoding/json zero-fills absent fields, so required ones must be pointers
// for Validate to be able to tell. Artifacts is what the gates check.
type Envelope interface {
	Validate() []string
	Artifacts() []string
}

// Gate checks an agent's claims mechanically and returns violations. It never
// judges the work — only whether what the agent said it did is true.
type Gate func(Envelope, *Run) []string

// Run is one invocation of one workflow against one repository.
type Run struct {
	ID       string
	Workflow string
	Repo     string // absolute target root: what makes one global database legible
	Dir      string // per-run directory in the data root; raw.jsonl and the Pi session live here

	// PiBin overrides the `pi` found on PATH. Tests set it; production does not.
	PiBin string
	// Out is where the closing banner goes.
	Out io.Writer

	cfg    config.Config
	ov     config.Overrides
	db     *trace.DB
	raw    *os.File
	seq    int
	tokens int
	cost   float64
	err    error // the first phase failure, which decides the run's status
}

// New creates the run's directory, opens the trace, and records the run as
// running. The caller must reach Finish, which closes both.
func New(cfg config.Config, ov config.Overrides, workflow, repo, request string) (*Run, error) {
	dataRoot, err := trace.DataRoot()
	if err != nil {
		return nil, err
	}
	id := trace.NewRunID(workflow)
	r := &Run{
		ID:       id,
		Workflow: workflow,
		Repo:     repo,
		Dir:      filepath.Join(dataRoot, "runs", id),
		Out:      os.Stdout,
		cfg:      cfg,
		ov:       ov,
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return nil, err
	}
	if r.raw, err = os.Create(filepath.Join(r.Dir, "raw.jsonl")); err != nil {
		return nil, err
	}
	if r.db, err = trace.Open(dataRoot); err != nil {
		r.raw.Close()
		return nil, err
	}
	if err := r.db.RunStart(r.ID, workflow, repo, request); err != nil {
		r.raw.Close()
		r.db.Close()
		return nil, err
	}
	r.onSignal()
	return r, nil
}

// onSignal settles the run row on Ctrl-C. Finish is an ordinary call at the
// end of a workflow, so without this a signal leaves the run 'running'
// forever — which the dashboard is what makes visible. Pi is in the same
// process group and takes the signal itself.
func (r *Run) onSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if err := r.db.RunInterrupt(r.ID); err != nil {
			fmt.Fprintln(r.Out, "lathe: recording the interrupt failed:", err)
		}
		fmt.Fprintf(r.Out, "\nlathe %s %s: interrupted\n  %s\n", r.Workflow, r.ID, r.Dir)
		os.Exit(130)
	}()
}

// Phase runs fn as a traced phase. The phase exists at status 'fail' from the
// moment it is created; success is earned by returning nil. The named return
// plus defer is what makes that hold through a panic too.
func (r *Run) Phase(p Params, fn func(*Handle) error) (err error) {
	r.seq++
	ph := trace.NewPhase(r.ID, r.seq, p.Name, p.Kind, p.Owner)
	if err := r.db.PhaseUpsert(ph); err != nil {
		return r.fail(err)
	}
	r.db.Event(r.ID, ph.ID, "phase_start", p.Name, nil)

	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic in phase %s: %v", p.Name, rec)
		}
		status, msg := "success", ""
		if err != nil {
			status, msg = "fail", err.Error()
			r.db.Event(r.ID, ph.ID, "error", p.Name, msg)
		}
		ph.Finish(status, msg)
		r.db.PhaseUpsert(ph)
		r.db.Event(r.ID, ph.ID, "phase_end", p.Name, map[string]string{"status": status})
		if err != nil {
			r.fail(err)
		}
	}()
	return fn(&Handle{run: r, phase: ph})
}

// Finish settles the database status, the banner and the exit code in one
// call, so the three cannot disagree. accepted is "the run produced an
// acceptable result", which is a different question from "every phase worked";
// v0 always passes true, and a test phase is what will make it earn its keep.
func (r *Run) Finish(accepted bool, reason string) int {
	status, code := "ok", 0
	switch {
	case r.err != nil:
		status, code = "fail", 1
		if reason == "" {
			reason = r.err.Error()
		}
	case !accepted:
		status, code = "fail", 1
	}
	if err := r.db.RunFinish(r.ID, status, r.tokens, r.cost); err != nil {
		fmt.Fprintln(r.Out, "lathe: recording the run's end failed:", err)
	}
	r.raw.Close()
	r.db.Close()

	fmt.Fprintf(r.Out, "\nlathe %s %s: %s  %d tokens  $%.5f\n  %s\n",
		r.Workflow, r.ID, status, r.tokens, r.cost, r.Dir)
	if reason != "" {
		fmt.Fprintf(r.Out, "  %s\n", reason)
	}
	return code
}

// Tokens and Cost are what the run has spent so far.
func (r *Run) Tokens() int   { return r.tokens }
func (r *Run) Cost() float64 { return r.cost }
func (r *Run) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// Handle is what a phase body is given: the only way to write to the trace or
// to call an agent, and both are already scoped to this phase.
type Handle struct {
	run   *Run
	phase *trace.Phase
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
	agent, err := r.cfg.Resolve(h.phase.Owner, r.ov)
	if err != nil {
		return err
	}

	prompt := agent.Prompt(request)
	for attempt := 0; ; attempt++ {
		res, err := r.spawn(h, agent, prompt, attempt)
		if err != nil {
			return err
		}

		violations := decode(res.Text, out)
		if len(violations) == 0 {
			violations = out.Validate()
		}
		// The rejected reply is recorded because it is what the violation is
		// about; an accepted one is already in raw.jsonl and in out, so
		// storing it again would only grow the database.
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
			r.db.Event(r.ID, h.phase.ID, "gate", agent.Name,
				map[string]any{"attempt": attempt, "violations": gateViolations})
			if len(gateViolations) == 0 {
				return nil
			}
			violations = gateViolations
		}

		if attempt >= maxCorrections {
			return fmt.Errorf("%s did not produce a valid envelope after %d corrections: %s",
				agent.Name, maxCorrections, strings.Join(violations, "; "))
		}
		prompt = correction(violations)
	}
}

// spawn is one Pi invocation. Every attempt reuses the phase id as the session
// id, which is what makes a correction cheap.
func (r *Run) spawn(h *Handle, a config.Resolved, prompt string, attempt int) (pi.Result, error) {
	ctx := context.Background()
	if a.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Deadline)
		defer cancel()
	}

	res, err := pi.Run(ctx, pi.Options{
		Bin:          r.PiBin,
		Dir:          r.Repo,
		Provider:     a.Provider,
		Model:        a.Model,
		Thinking:     a.Thinking,
		Tools:        a.Tools,
		SystemPrompt: a.SystemPrompt,
		SessionID:    h.phase.ID,
		SessionDir:   r.Dir, // the session is thrown away with the run that made it
		Raw:          r.raw,
		OnStart: func(pid int) {
			r.db.Event(r.ID, h.phase.ID, "log", "pi_pid", map[string]int{"pid": pid})
		},
	}, prompt, func(ev pi.Event) {
		// ponytail: this writes to SQLite while Pi's stdout is not being read,
		// so a contended database back-pressures the agent for up to
		// busy_timeout. The fix, if a run ever actually stalls here, is a
		// channel in front of the tracer rather than anything in this handler.
		if ev.Type == "tool_execution_end" {
			r.db.Event(r.ID, h.phase.ID, "tool_call", ev.ToolName, map[string]any{
				"id": ev.ToolCallID, "args": ev.Args, "isError": ev.IsError,
			})
		}
	})

	// Spend is real whether or not the call succeeded, so it is recorded first.
	r.tokens += res.Tokens
	r.cost += res.Cost
	r.db.Event(r.ID, h.phase.ID, "usage", a.Name,
		map[string]any{"attempt": attempt, "tokens": res.Tokens, "cost": res.Cost})
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
