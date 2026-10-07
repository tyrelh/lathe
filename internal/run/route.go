package run

import (
	"context"
	"maps"
	"os"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/jev"
	"github.com/tyrelh/lathe/internal/trace"
)

// Decision is which tier routing picked and why: "routed", "low-confidence",
// or "error" with Err set and the default tier picked. Answer is nil on error.
// Raw is Jev's response body, when one arrived.
type Decision struct {
	Tier   int
	Reason string
	Answer *jev.Answer
	Err    error
	Raw    string
}

// Decide asks Jev how demanding the task in state is against rt's tiers and
// picks one. key is the TypeSafe API key. It never fails: any error falls
// back to the default tier.
func Decide(ctx context.Context, key string, rt config.Routing, floor float64, state any) Decision {
	a, err := jev.Score(ctx, key, rt.Instructions, rt.Criteria(), state)
	if err != nil {
		return Decision{Tier: rt.Default(), Reason: "error", Err: err, Raw: a.Raw}
	}
	tier, reason := choose(a.Probabilities, a.Confidence, floor)
	return Decision{Tier: tier, Reason: reason, Answer: &a, Raw: a.Raw}
}

// choose returns the most probable level, ties going to the higher one, moved
// up one level (capped at the top) when confidence is under floor.
func choose(probabilities []float64, confidence, floor float64) (int, string) {
	top := 0
	for i, p := range probabilities {
		if p >= probabilities[top] {
			top = i
		}
	}
	if confidence < floor {
		return min(top+1, len(probabilities)-1), "low-confidence"
	}
	return top, "routed"
}

// Routes reports whether agent has tiers in this run's captured roster.
func (r *Run) Routes(agent string) bool {
	_, ok := r.cfg.Routing[agent]
	return ok
}

// RouteReport is a routing phase's output: the decision, and every tier with
// the probability Jev gave it. Confidence, Score and Jev are unset on error.
type RouteReport struct {
	Agent      string       `json:"agent"`
	Tier       int          `json:"tier"`
	Reason     string       `json:"reason"`
	Floor      float64      `json:"floor"`
	Confidence *float64     `json:"confidence,omitempty"`
	Score      *float64     `json:"score,omitempty"`
	Jev        string       `json:"jev,omitempty"`
	Error      string       `json:"error,omitempty"`
	Levels     []RouteLevel `json:"levels"`
}

// RouteLevel is one tier as Jev was asked about it.
type RouteLevel struct {
	When        string   `json:"when"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Thinking    string   `json:"thinking,omitempty"`
	Default     bool     `json:"default,omitempty"`
	Probability *float64 `json:"probability,omitempty"`
}

// Route picks one of agent's tiers from state and makes it the agent's config
// for the rest of this execution. It traces the request sent to Jev as the
// phase's input, the RouteReport and Jev's raw response as its output, and
// Jev's spend. Only a trace write or cancellation returns an error.
func (h *Handle) Route(agent string, state any) error {
	r := h.run
	rt, ok := r.cfg.Routing[agent]
	if !ok {
		return nil
	}
	if err := r.db.Event(r.ID, h.phase.ID, "input", "jev", map[string]any{
		"model": jev.Model, "instructions": rt.Instructions, "criteria": rt.Criteria(), "state": state,
	}); err != nil {
		return err
	}
	floor := r.cfg.ConfidenceFloor
	d := Decide(r.ctx, os.Getenv("TYPESAFE_API_KEY"), rt, floor, state)
	if err := r.ctx.Err(); err != nil {
		return err
	}

	report := RouteReport{Agent: agent, Tier: d.Tier, Reason: d.Reason, Floor: floor}
	for i, t := range rt.Tiers {
		level := RouteLevel{When: t.When, Provider: t.Resolved.Provider, Model: t.Resolved.Model,
			Thinking: t.Resolved.Thinking, Default: t.Default}
		if d.Answer != nil {
			level.Probability = &d.Answer.Probabilities[i]
		}
		report.Levels = append(report.Levels, level)
	}
	if d.Err != nil {
		report.Error = d.Err.Error()
	}
	if a := d.Answer; a != nil {
		report.Confidence, report.Score, report.Jev = &a.Confidence, &a.Score, a.Model
		if err := r.db.RecordUsage(trace.Usage{
			RunID: r.ID, PhaseID: h.phase.ID, Agent: h.phase.Owner, Seq: 1,
			Provider: "typesafe", Model: a.Model,
			Tokens: a.InputTokens + a.OutputTokens, Cost: a.Cost(),
		}); err != nil {
			return err
		}
	}

	r.mu.Lock()
	// Cloned so the captured snapshot the caller handed Open stays as captured.
	r.cfg.Agents = maps.Clone(r.cfg.Agents)
	r.cfg.Agents[agent] = rt.Tiers[d.Tier].Resolved
	if a := d.Answer; a != nil {
		r.tokens += a.InputTokens + a.OutputTokens
		r.cost += a.Cost()
	}
	r.mu.Unlock()
	return r.db.Event(r.ID, h.phase.ID, "output", "jev", map[string]any{"report": report, "text": d.Raw})
}
