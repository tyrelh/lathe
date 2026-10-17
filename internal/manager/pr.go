package manager

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

const prPoll = 5 * time.Minute

type lockedWriter struct {
	mu sync.Mutex
	w io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// pollPRs runs serial passes until cancellation, invoking after after completed passes.
func (m *manager) pollPRs(ctx context.Context, interval time.Duration, after func()) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if !m.pollPRPass(ctx) {
			return
		}
		if after != nil {
			after()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pollPRPass records current states; false means cancellation interrupted the pass.
func (m *manager) pollPRPass(ctx context.Context) bool {
	rows, err := m.db.PRCandidates()
	if err != nil {
		fmt.Fprintln(m.out, "lathe manager: PR candidates:", err)
		return true
	}
	view := m.viewPR
	if view == nil {
		view = workspace.ViewPR
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return false
		}
		url := worker.RecordedPRURL(m.dataRoot, row)
		if url == "" {
			continue
		}
		pr, err := view(row.Repo, url)
		if err == nil {
			var state string
			switch pr.State {
			case "OPEN":
				state = "open"
			case "MERGED":
				state = "merged"
			case "CLOSED":
				state = "closed"
			default:
				err = fmt.Errorf("unknown PR state %q", pr.State)
			}
			if err == nil {
				err = m.db.ObservePR(row.ID, state)
			}
		}
		if err != nil {
			fmt.Fprintf(m.out, "lathe manager: PR %s (%s, %s): %v\n", row.ID, row.Repo, url, err)
		}
	}
	return ctx.Err() == nil
}
