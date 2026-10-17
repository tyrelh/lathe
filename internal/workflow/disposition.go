package workflow

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/workspace"
)

// Reversal is uncommitted work a revised plan abandons. Lathe saves the work,
// then reverts the path before the builder resumes.
type Reversal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Dismissal is an outstanding adjudicator finding a revised plan makes moot.
// Request is the scope request whose plan dismissed it.
//
// From is the iteration whose validation raised the finding, set when that
// is an earlier iteration; nil means this iteration's own finding. Refs
// repeat across iterations, so a dismissal applies only to its own.
type Dismissal struct {
	Ref     string `json:"ref"`
	Reason  string `json:"reason"`
	Request int    `json:"scope_request,omitempty"`
	From    *int   `json:"from_iteration,omitempty"`
}

// Disposition is what a reviewed plan did with work and findings it found:
// paths it abandoned, where that work was saved, and findings it dismissed.
type Disposition struct {
	Reversed  []string    `json:"reversed,omitempty"`
	Abandoned string      `json:"abandoned,omitempty"`
	Dismissed []Dismissal `json:"dismissed,omitempty"`
}

// dispose applies a reviewed plan's reversals and dismissals before the
// builder runs: the whole uncommitted tree is saved to name in the run
// directory, then the abandoned paths are reverted; dismissed findings leave
// c.outstanding and are recorded in validation with request. Enforcement
// drops reverted paths from retained work at its next sweep, since they are
// no longer dirty. Callers invoke it only after review forwards the plan.
func (c *code) dispose(e *run.Entry, r *run.Run, name string, request int) (Disposition, error) {
	var d Disposition
	if c.plan.Reversals != nil {
		for _, rv := range *c.plan.Reversals {
			d.Reversed = append(d.Reversed, permit.Norm(rv.Path))
		}
	}
	if len(d.Reversed) > 0 {
		dest := filepath.Join(r.Dir, name)
		saved, err := workspace.SavePatch(r.Repo, "HEAD", dest, r.Protected())
		if err != nil {
			return d, fmt.Errorf("saving abandoned work before reverting it: %w; nothing was reverted", err)
		}
		if saved {
			d.Abandoned = dest
		}
		if err := permit.Revert(r.Repo, d.Reversed); err != nil {
			return d, err
		}
		if err := e.Log("reverted", fmt.Sprintf("%s (saved to %s)", strings.Join(d.Reversed, ", "), d.Abandoned)); err != nil {
			return d, err
		}
	}
	if c.plan.Dismissals != nil && len(*c.plan.Dismissals) > 0 {
		dismissed := map[string]bool{}
		for _, dm := range *c.plan.Dismissals {
			dm.Request = request
			// The Dismissible gate admitted only outstanding refs; record
			// whose finding this was.
			for _, f := range c.outstanding {
				if f.Ref == dm.Ref {
					dm.From = f.From
				}
			}
			d.Dismissed = append(d.Dismissed, dm)
			c.v.Dismissals = append(c.v.Dismissals, dm)
			dismissed[dm.Ref] = true
		}
		var keep []Finding
		for _, f := range c.outstanding {
			if !dismissed[f.Ref] {
				keep = append(keep, f)
			}
		}
		c.outstanding = keep
		// Saved now: the builder that follows may fail before validation
		// writes the report again, and a recovery must not resurrect these.
		// dismissals.json keeps every dismissal of this iteration even when
		// it never validates, for later recoveries.
		if err := writeResult(r.Dir, run.DismissalsFile, c.v.Dismissals); err != nil {
			return d, err
		}
		if len(c.v.Rounds) > 0 {
			if err := writeResult(r.Dir, run.ValidationFile, &c.v); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}

// Retained holds a plan to the uncommitted work in the tree: every dirty path
// must stay in files or be abandoned in reversals, and a reversal may name
// only dirty work.
func Retained(dirty []string) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := planOf(e)
		if !ok || p.Files == nil {
			return nil
		}
		inTree := map[string]bool{}
		for _, d := range dirty {
			inTree[permit.Norm(d)] = true
		}
		kept, reversed := map[string]bool{}, map[string]bool{}
		for _, f := range *p.Files {
			kept[permit.Norm(f)] = true
		}
		var v []string
		if p.Reversals != nil {
			for _, rv := range *p.Reversals {
				path := permit.Norm(rv.Path)
				if !inTree[path] || permit.Escapes(rv.Path) {
					v = append(v, fmt.Sprintf(`reversal %q names no uncommitted work; "reversals" may only abandon paths listed as work in the tree`, rv.Path))
					continue
				}
				reversed[path] = true
			}
		}
		for _, d := range dirty {
			if path := permit.Norm(d); !kept[path] && !reversed[path] {
				v = append(v, fmt.Sprintf(`%q holds uncommitted work; keep it in "files" or abandon it in "reversals" with a reason`, d))
			}
		}
		return v
	}
}

// Dismissible lets a plan dismiss only findings still outstanding.
func Dismissible(outstanding []Finding) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := planOf(e)
		if !ok || p.Dismissals == nil {
			return nil
		}
		known := map[string]bool{}
		for _, f := range outstanding {
			known[f.Ref] = true
		}
		var v []string
		seen := map[string]bool{}
		for _, d := range *p.Dismissals {
			switch {
			case !known[d.Ref]:
				v = append(v, fmt.Sprintf(`%q is not an outstanding finding; "dismissals" may only name findings listed as outstanding`, d.Ref))
			case seen[d.Ref]:
				v = append(v, fmt.Sprintf(`%q is dismissed more than once`, d.Ref))
			}
			seen[d.Ref] = true
		}
		return v
	}
}
