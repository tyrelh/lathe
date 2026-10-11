package run

import (
	"context"
	"fmt"
)

// CommandFailure is a red suite the tester reported, recoverable by a later fix
// phase. Infrastructure, timeout and enforcement errors remain terminal.
type CommandFailure struct{ ExitCode int }

func (e *CommandFailure) Error() string { return fmt.Sprintf("test command exited %d", e.ExitCode) }

// EnvironmentError is a test command that could not execute in the worker environment.
type EnvironmentError struct{ ExitCode int }

func (e *EnvironmentError) Error() string {
	return fmt.Sprintf("test command exited %d: environment failure; install the required executable, check the worker PATH and executable permissions, then rerun", e.ExitCode)
}

// commandContext returns the run's context, bounded by command_timeout when set.
func (r *Run) commandContext() (context.Context, context.CancelFunc) {
	// Derived from the run's context, so a cancelled run kills the command and
	// its whole process group rather than waiting for it. The bound is the
	// roster's command_timeout: a code phase is lathe's own work, so borrowing
	// the tester's deadline would both tie every code phase to that one agent
	// and break any workflow whose captured roster does not include it.
	if d := r.cfg.CommandDeadline; d > 0 {
		return context.WithTimeout(r.ctx, d)
	}
	return context.WithCancel(r.ctx)
}
