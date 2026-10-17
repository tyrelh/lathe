package run

import (
	"context"
	"fmt"
	"time"
)

// EnvironmentError is a test command that could not execute in the worker environment.
type EnvironmentError struct{ ExitCode int }

func (e *EnvironmentError) Error() string {
	return fmt.Sprintf("test command exited %d: environment failure; install the required executable, check the worker PATH and executable permissions, then rerun", e.ExitCode)
}

// commandContext derives from the run's context, bounded by command_timeout,
// else by fallback when positive.
func (r *Run) commandContext(fallback time.Duration) (context.Context, context.CancelFunc) {
	if d := r.cfg.CommandDeadline; d > 0 {
		return context.WithTimeout(r.ctx, d)
	}
	if fallback > 0 {
		return context.WithTimeout(r.ctx, fallback)
	}
	return context.WithCancel(r.ctx)
}
