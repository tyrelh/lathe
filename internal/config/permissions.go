package config

import (
	"slices"
	"time"

	"github.com/tyrelh/lathe/internal/web"
)

// Agent permissions are compiled in and never written to disk: agent
// configuration can change what an agent is told and which model runs it,
// never what it may do.

// readOnly is every agent's floor. No `bash`, no `write`, no `edit`: with no
// write-capable tool and no shell a read-only agent cannot modify the repo it
// was pointed at.
//
// `web_read` fetches one public https URL as markdown, and an agent can put
// anything it has read into that URL. The guard holds a web_read agent's reads
// to the repo and the Go module cache. It does not hold for an agent that also
// has `bash`: a shell reads anything and has its own network tools. No agent
// has both; the first one that does needs an OS sandbox around Pi.
var readOnly = []string{"read", "grep", "find", "ls"}

func with(extra ...string) []string {
	return append(append([]string{}, readOnly...), extra...)
}

// Permission is one agent the binary knows and the tools it may use.
type Permission struct {
	Name  string
	Tools []string
}

// Permissions is every agent, in roster order.
var Permissions = []Permission{
	{"scout", with()},
	{"planner", with(web.Tool)},
	{"plan-reviewer", with(web.Tool)},
	{"builder", with("write", "edit")},
	{"tester", with("bash")},
	{"code-review-general", with(web.Tool)},
	{"code-review-security", with(web.Tool)},
	{"code-review-slop", with(web.Tool)},
	{"code-review-adversarial", with(web.Tool)},
	{"adjudicator", with()},
	{"brancher", with("bash")},
	{"committer", with("bash")},
	{"pr-author", with("bash")},
}

// Protected is the paths no agent may write and no plan may name. An entry
// ending in "/" is a directory name, matched at any depth; anything else is a
// glob matched against the basename.
var Protected = []string{".git/", ".env*", "*.pem", "*.key"}

// BashDenied is the commands the guard refuses to run, as Go regexps. It
// narrows the obvious ways out of the repo without closing them. `git push`
// and `git commit` are here because lathe performs both itself.
var BashDenied = []string{
	`\bgit\s+push\b`,
	`\bgit\s+commit\b`,
	`\brm\s+-[a-zA-Z]*r`,
	`\bcurl\b`,
	`\bwget\b`,
	`\bsudo\b`,
	`\bssh\b`,
	`\bscp\b`,
}

// CommandTimeout bounds a code phase's command, which is lathe's own work
// rather than an agent's.
const CommandTimeout = 10 * time.Minute

// Route is one agent that may have tiers: its routing phase, which is that
// phase's graph node name, and the path of its routing instructions.
type Route struct{ Agent, Phase, Key string }

// Routes is every routable agent, in the order the graphs route them.
var Routes = []Route{
	{"planner", "route-plan", "routing/planner.md"},
	{"builder", "route-build", "routing/builder.md"},
}

// RoutingKey returns agent's routing instructions path, or "" when it cannot
// be routed.
func RoutingKey(agent string) string {
	for _, r := range Routes {
		if r.Agent == agent {
			return r.Key
		}
	}
	return ""
}

// Thinking is every thinking level Pi accepts.
var Thinking = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// ValidThinking reports whether level is one of Thinking.
func ValidThinking(level string) bool {
	return slices.Contains(Thinking, level)
}
