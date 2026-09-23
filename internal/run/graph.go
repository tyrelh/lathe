package run

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/tyrelh/lathe/internal/permit"
)

// Node is one phase in a workflow graph. A graph is an ordered list of them:
// declaration order is the happy path, so a workflow reads top to bottom.
type Node struct {
	Name, Owner string
	// SendBacks is how many times this node may name an earlier node. A target
	// declared later is a forward and costs nothing; only going back is bounded.
	SendBacks int
	// RevertOnly tolerates leavings, as Params.RevertOnly does.
	RevertOnly bool
}

// Entry is one execution of a node. It is the phase's Handle plus the two
// things a node needs to know about its own history.
type Entry struct {
	*Handle
	// Round is 0 on the first entry and increments on each re-entry.
	Round int
	// SendBacksLeft is how many more times this node may name an earlier one.
	// The runner refuses one past that, but the policy at zero belongs to the
	// node: plan review forwards with its objections folded into risks, and the
	// test loop fails the run.
	SendBacksLeft int

	node *node
}

// ResetSession drops this node's session so the next entry starts cold, which
// is what a node wants after an agent returned nothing usable: the garbage
// does not get dragged into the next round.
func (e *Entry) ResetSession() { e.node.session = "" }

// Failed records this phase as failed without ending the run. The node still
// chooses its target.
func (e *Entry) Failed(err error) { e.Handle.failed = err }

// node is a Node plus the state the runner keeps for it between entries.
type node struct {
	Node
	enter   func(*Entry) (string, error)
	session string
	round   int
	sent    int
}

// Graph runs a workflow's nodes. It owns counting, sessions and tracing; the
// data between nodes stays in the workflow's own typed locals.
type Graph struct {
	run   *Run
	nodes []*node
	index map[string]int
}

func NewGraph(r *Run) *Graph {
	return &Graph{run: r, index: map[string]int{}}
}

// Add appends a node. enter returns where to go next: empty forwards to the
// node declared after it, a name jumps to that node, and an error ends the run.
func (g *Graph) Add(n Node, enter func(*Entry) (string, error)) {
	g.index[n.Name] = len(g.nodes)
	g.nodes = append(g.nodes, &node{Node: n, enter: enter})
}

// Run enters the first node and follows targets until one forwards off the end.
func (g *Graph) Run() error {
	for i := 0; i < len(g.nodes); {
		n := g.nodes[i]
		var target string
		err := g.run.Phase(Params{
			Name: n.Name, Owner: n.Owner, SessionID: n.session, RevertOnly: n.RevertOnly,
		}, func(h *Handle) error {
			// The first entry adopts the phase's own id, so every later entry
			// resumes the same Pi session without the workflow threading it.
			if n.session == "" {
				n.session = h.SessionID()
			}
			var err error
			target, err = n.enter(&Entry{
				Handle: h, Round: n.round, SendBacksLeft: n.SendBacks - n.sent, node: n,
			})
			return err
		})
		if err != nil {
			return err
		}
		n.round++
		if target == "" {
			i++
			continue
		}
		j, ok := g.index[target]
		if !ok {
			return fmt.Errorf("node %q named unknown target %q; known nodes are %s",
				n.Name, target, strings.Join(g.names(), ", "))
		}
		// A node naming itself is returning work to its producer too, so it is
		// budgeted like any other send-back: the bound on total executions holds
		// only if every cycle spends something, and a free self-loop spends
		// nothing.
		if j <= i {
			// The guard, not the policy: a node is expected to read
			// SendBacksLeft and decide for itself what to do at zero.
			if n.sent >= n.SendBacks {
				return fmt.Errorf("node %q sent back to %q past its budget of %d",
					n.Name, target, n.SendBacks)
			}
			n.sent++
		}
		i = j
	}
	return nil
}

// Worker is one member of a group: a phase that runs alongside the group's
// other workers against the same checkout. Like a node, it keeps its own
// session and round across every entry of the group.
type Worker struct {
	Name, Owner string
	// Run does the worker's job. An error is the worker failing to produce a
	// usable result — not a finding and not a red suite, both of which are
	// results — so it is retried against the same code.
	Run func(*Entry) error
}

// WorkerRetries is how many more attempts a failed worker gets after its first
// in one group entry. Retries should be rare, and they share one correction
// allowance with the attempts before them, so a worker's turns per entry are
// bounded by 1 + WorkerRetries + maxCorrections rather than their product.
const WorkerRetries = 2

// GroupResult is what a group entry leaves for the node that judges it.
type GroupResult struct {
	// Failed is every worker that exhausted its retries, with its last error.
	Failed map[string]error
	// Mutated is every source path that changed while the workers ran, found
	// before the cleanup that would hide it. A nonempty list means the round
	// judged code that is no longer the code in the tree.
	Mutated []string
}

// FailedNames is Failed's keys in a stable order.
func (g GroupResult) FailedNames() []string { return slices.Sorted(maps.Keys(g.Failed)) }

// AddGroup appends a node that runs every worker concurrently, waits for all
// of them to finish or exhaust their retries, and hands the result to done,
// which returns the target as any node would. Each worker is traced as its own
// phase; each retry is another phase, so a failed attempt stays in the trace
// after a retry recovers it.
//
// Implementation is frozen while the workers run: enforcement is deferred to a
// single sweep after the join, and the source is fingerprinted before and
// compared after, ahead of that sweep. The group itself never sends back.
func (g *Graph) AddGroup(n Node, workers []Worker, done func(*Entry, GroupResult) (string, error)) {
	states := make([]*node, len(workers))
	for i, w := range workers {
		states[i] = &node{Node: Node{Name: w.Name, Owner: w.Owner}}
	}
	g.Add(n, func(e *Entry) (string, error) {
		res, err := g.parallel(e, workers, states)
		if err != nil {
			return "", err
		}
		return done(e, res)
	})
}

func (g *Graph) parallel(e *Entry, workers []Worker, states []*node) (GroupResult, error) {
	r := g.run
	res := GroupResult{Failed: map[string]error{}}
	var before map[string]string
	if r.clean {
		var err error
		if before, err = permit.Fingerprint(r.Repo); err != nil {
			return res, err
		}
	}

	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = g.work(workers[i], states[i])
		}()
	}
	wg.Wait()
	r.mu.Lock()
	r.frozen = false
	r.mu.Unlock()

	// A cancelled run has nothing worth judging, and the phases that saw it
	// have already failed the run.
	if err := r.ctx.Err(); err != nil {
		return res, err
	}
	for i, err := range errs {
		if err != nil {
			res.Failed[workers[i].Name] = err
		}
	}
	if !r.clean {
		return res, nil
	}
	// Evidence first, cleanup second: the sweep reverts a changed file that
	// nobody accepted, and after it the change would be gone.
	mutated, err := permit.Mutations(r.Repo, before)
	if err != nil {
		return res, err
	}
	res.Mutated = mutated
	if len(mutated) > 0 {
		e.Log("mutated", strings.Join(mutated, "\n"))
	}
	r.mu.Lock()
	_, err = r.sweep(e.Handle)
	r.mu.Unlock()
	return res, err
}

// work runs one worker for one group entry, retrying a failure up to
// WorkerRetries times. A retry starts a fresh session: the one that failed may
// be the thing that is broken, and the prompt carries the round's evidence.
func (g *Graph) work(w Worker, n *node) error {
	left := maxCorrections
	var failed error
	for try := 0; try <= WorkerRetries; try++ {
		failed = nil
		err := g.run.Phase(Params{Name: w.Name, Owner: w.Owner, SessionID: n.session}, func(h *Handle) error {
			if n.session == "" {
				n.session = h.SessionID()
			}
			h.corrections = &left
			if try > 0 {
				if err := h.Log("retry", fmt.Sprintf("attempt %d of %d", try+1, WorkerRetries+1)); err != nil {
					return err
				}
			}
			failed = w.Run(&Entry{Handle: h, Round: n.round, node: n})
			// Cancellation is the run's failure, not the worker's. Anything
			// else fails this attempt without failing the run: a retry may yet
			// recover it, and the attempt stays in the trace either way.
			if failed != nil && g.run.ctx.Err() == nil {
				h.failed = failed
				return nil
			}
			return failed
		})
		if err != nil {
			return err
		}
		if failed == nil {
			break
		}
		n.session = ""
	}
	n.round++
	return failed
}

func (g *Graph) names() []string {
	names := make([]string, len(g.nodes))
	for i, n := range g.nodes {
		names[i] = n.Name
	}
	return names
}
