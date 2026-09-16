// Package install links the lathe repo into each agent's skills directory.
package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// skillDirs are the agent skill directories lathe links itself into, relative
// to the user's home directory. A directory that does not exist is skipped.
var skillDirs = []string{".claude/skills", ".codex/skills", ".pi/agent/skills"}

// Run creates (or verifies) a `lathe` symlink in every existing skill
// directory, pointing at repoRoot. It never replaces anything it did not
// create. Progress is written to out.
func Run(repoRoot string, out io.Writer) error {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "SKILL.md")); err != nil {
		return fmt.Errorf("%s is not a lathe repo (no SKILL.md)", repoRoot)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	linked := 0
	for _, dir := range skillDirs {
		dir = filepath.Join(home, dir)
		if _, err := os.Stat(dir); err != nil {
			fmt.Fprintf(out, "skip  %s (does not exist)\n", dir)
			continue
		}
		link := filepath.Join(dir, "lathe")
		switch target, err := os.Readlink(link); {
		case err == nil && target == repoRoot:
			fmt.Fprintf(out, "ok    %s\n", link)
			linked++
		case err == nil:
			return fmt.Errorf("%s already links to %s, not %s", link, target, repoRoot)
		default:
			if _, err := os.Lstat(link); err == nil {
				return fmt.Errorf("%s exists and is not a symlink", link)
			}
			if err := os.Symlink(repoRoot, link); err != nil {
				return err
			}
			fmt.Fprintf(out, "link  %s -> %s\n", link, repoRoot)
			linked++
		}
	}
	if linked == 0 {
		return fmt.Errorf("no agent skill directories found under %s", home)
	}

	// A binary that is not on PATH fails identically to a broken skill, so say so.
	if bin := filepath.Join(home, ".local/bin"); !onPath(bin) {
		fmt.Fprintf(out, "warn  %s is not on PATH; agents will not find `lathe`\n", bin)
	}
	return nil
}

func onPath(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}
