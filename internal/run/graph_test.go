package run

import (
	"errors"
	"strings"
	"testing"
)

// visit records one entry of one node: everything a graph test asserts about
// order, rounds, budgets and sessions is a field on it.
type visit struct {
	node    string
	round   int
	left    int
	session string
}

// record adds a node whose body reports its entry and returns the next target
// from targets, one per entry, forwarding once the list runs out.
func record(g *Graph, log *[]visit, n Node, targets ...string) {
	g.Add(n, func(e *Entry) (string, error) {
		*log = append(*log, visit{n.Name, e.Round, e.SendBacksLeft, e.SessionID()})
		if e.Round < len(targets) {
			return targets[e.Round], nil
		}
		return "", nil
	})
}

func TestGraphForwardsOffTheEnd(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	var log []visit
	g := NewGraph(r)
	record(g, &log, Node{Name: "first", Owner: "engineer"})
	record(g, &log, Node{Name: "second", Owner: "engineer"})

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].node != "first" || log[1].node != "second" {
		t.Fatalf("visits: %v", log)
	}
	if got := scalar[int](t, readDB(t), "SELECT count(*) FROM phases WHERE status = 'success'"); got != 2 {
		t.Fatalf("successful phases: %d", got)
	}
}

// A send-back re-enters the earlier node with its round advanced and the same
// session, which is what stops a resumed agent paying for its context twice.
func TestGraphSendBackKeepsRoundAndSession(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	var log []visit
	g := NewGraph(r)
	record(g, &log, Node{Name: "make", Owner: "engineer"})
	record(g, &log, Node{Name: "check", Owner: "engineer", SendBacks: 1}, "make")

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	want := []visit{{"make", 0, 0, ""}, {"check", 0, 1, ""}, {"make", 1, 0, ""}, {"check", 1, 0, ""}}
	for i, v := range want {
		got := log[i]
		if len(log) != len(want) || got.node != v.node || got.round != v.round || got.left != v.left {
			t.Fatalf("visit %d: %+v, want %+v of %v", i, got, v, log)
		}
	}
	if log[0].session != log[2].session || log[1].session != log[3].session {
		t.Fatalf("sessions not held per node: %v", log)
	}
	if log[0].session == log[1].session {
		t.Fatalf("nodes share a session: %v", log)
	}
}

// One node's send-backs must not spend another's: under two overlapping loops
// every forward out of the first passes through the second.
func TestGraphBudgetsAreOwnedByTheirNode(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	var log []visit
	g := NewGraph(r)
	record(g, &log, Node{Name: "build", Owner: "engineer"})
	record(g, &log, Node{Name: "test", Owner: "engineer", SendBacks: 2})
	record(g, &log, Node{Name: "review", Owner: "engineer", SendBacks: 2}, "build", "build")

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	for _, v := range log {
		if v.node == "test" && v.left != 2 {
			t.Fatalf("a review send-back spent the tester's budget: %v", log)
		}
	}
	if last := log[len(log)-1]; last.node != "review" || last.round != 2 || last.left != 0 {
		t.Fatalf("final visit %+v of %v", last, log)
	}
}

func TestGraphRefusesASendBackPastBudget(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(false, "")
	g := NewGraph(r)
	g.Add(Node{Name: "make", Owner: "engineer"}, func(*Entry) (string, error) { return "", nil })
	g.Add(Node{Name: "check", Owner: "engineer"}, func(*Entry) (string, error) { return "make", nil })

	err := g.Run()
	if err == nil || !strings.Contains(err.Error(), `"check"`) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error: %v", err)
	}
}

// A node naming itself re-enters it, and spends budget doing so: otherwise a
// callback that keeps returning its own name runs forever.
func TestGraphBudgetsASelfTarget(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	var log []visit
	g := NewGraph(r)
	record(g, &log, Node{Name: "make", Owner: "engineer", SendBacks: 1}, "make", "make")

	err := g.Run()
	if err == nil || !strings.Contains(err.Error(), `"make"`) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("an unbudgeted self-loop was permitted: %v after %v", err, log)
	}
	if len(log) != 2 || log[1].round != 1 || log[1].left != 0 {
		t.Fatalf("visits: %v", log)
	}
}

func TestGraphRejectsAnUnknownTarget(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(false, "")
	g := NewGraph(r)
	g.Add(Node{Name: "make", Owner: "engineer"}, func(*Entry) (string, error) { return "polish", nil })

	err := g.Run()
	if err == nil || !strings.Contains(err.Error(), `"polish"`) || !strings.Contains(err.Error(), "make") {
		t.Fatalf("error: %v", err)
	}
}

func TestGraphResetSessionStartsTheNextEntryCold(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	var log []visit
	g := NewGraph(r)
	g.Add(Node{Name: "make", Owner: "engineer"}, func(e *Entry) (string, error) {
		log = append(log, visit{e.params.Name, e.Round, e.SendBacksLeft, e.SessionID()})
		e.ResetSession()
		return "", nil
	})
	g.Add(Node{Name: "check", Owner: "engineer", SendBacks: 1}, func(e *Entry) (string, error) {
		if e.Round == 0 {
			return "make", nil
		}
		return "", nil
	})

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].session == log[1].session {
		t.Fatalf("session survived a reset: %v", log)
	}
}

// Failed is what replaced RecoverableError: the phase is recorded failed, the
// node still picks where to go, and the run is not poisoned.
func TestGraphFailedRecordsThePhaseAndContinues(t *testing.T) {
	r := newRun(t, "")
	g := NewGraph(r)
	g.Add(Node{Name: "review", Owner: "engineer"}, func(e *Entry) (string, error) {
		e.Failed(errors.New("reviewer returned nothing usable"))
		return "", nil
	})
	g.Add(Node{Name: "build", Owner: "engineer"}, func(*Entry) (string, error) { return "", nil })

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if code := r.Finish(true, ""); code != 0 {
		t.Fatalf("a recorded failure ended the run: %d", code)
	}
	db := readDB(t)
	if got := scalar[string](t, db, "SELECT status FROM phases WHERE name = 'review'"); got != "fail" {
		t.Fatalf("review status %q", got)
	}
	if got := scalar[string](t, db, "SELECT error FROM phases WHERE name = 'review'"); !strings.Contains(got, "nothing usable") {
		t.Fatalf("review error %q", got)
	}
	if got := scalar[string](t, db, "SELECT status FROM phases WHERE name = 'build'"); got != "success" {
		t.Fatalf("build status %q", got)
	}
}
