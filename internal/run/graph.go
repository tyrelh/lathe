package run

import (
	"fmt"
	"strings"
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

func (g *Graph) names() []string {
	names := make([]string, len(g.nodes))
	for i, n := range g.nodes {
		names[i] = n.Name
	}
	return names
}
