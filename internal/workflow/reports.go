package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// writeResult puts the accepted envelope beside raw.jsonl, under the name the
// workflow's readers expect. Callers must propagate disk failures to the workflow.
func writeResult(dir, name string, out any) error {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600)
}
