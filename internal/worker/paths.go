package worker

import (
	"fmt"
	"path/filepath"
)

// ReportDir is where one iteration's reports live. The first iteration's are
// the run directory itself, which is where readers have always looked; each
// revision gets its own beside it, so a new plan or validation report never
// overwrites an earlier one.
func ReportDir(dataRoot, runID string, iteration int) string {
	dir := filepath.Join(dataRoot, "runs", runID)
	if iteration > 0 {
		dir = filepath.Join(dir, fmt.Sprintf("iteration-%d", iteration))
	}
	return dir
}

// AttemptDir is where one attempt's raw stream, guard, scope and Pi session
// live. Reports stay in the iteration's report directory, which is where
// readers look for them.
func AttemptDir(dataRoot, runID, attemptID string) string {
	return filepath.Join(dataRoot, "runs", runID, attemptID)
}

// LogPath is the per-attempt worker log the manager redirects output to.
func LogPath(dataRoot, runID, attemptID string) string {
	return filepath.Join(AttemptDir(dataRoot, runID, attemptID), "worker.log")
}
