package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/web"
)

func TestPermissions(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Permissions {
		if seen[p.Name] {
			t.Fatalf("%s listed twice", p.Name)
		}
		seen[p.Name] = true
		if slices.Contains(p.Tools, "bash") && slices.Contains(p.Tools, web.Tool) {
			t.Errorf("%s has both bash and %s", p.Name, web.Tool)
		}
		if (slices.Contains(p.Tools, "write") || slices.Contains(p.Tools, "edit")) && p.Name != "builder" {
			t.Errorf("%s can write", p.Name)
		}
	}
	if len(Permissions) != 13 {
		t.Fatalf("%d agents", len(Permissions))
	}
	for name, want := range map[string]string{
		"scout": "read,grep,find,ls", "builder": "read,grep,find,ls,write,edit",
		"tester": "read,grep,find,ls,bash", "plan-reviewer": "read,grep,find,ls,web_read",
	} {
		for _, p := range Permissions {
			if p.Name == name && strings.Join(p.Tools, ",") != want {
				t.Errorf("%s tools = %v", name, p.Tools)
			}
		}
	}
	for _, r := range Routes {
		if !seen[r.Agent] || RoutingKey(r.Agent) != r.Key {
			t.Errorf("route %+v", r)
		}
	}
	if RoutingKey("tester") != "" {
		t.Error("tester is routable")
	}
	if !strings.Contains(strings.Join(Protected, ","), ".git/") {
		t.Errorf("protected = %v", Protected)
	}
}

func TestValidThinking(t *testing.T) {
	for _, ok := range []string{"off", "medium", "xhigh"} {
		if !ValidThinking(ok) {
			t.Errorf("%s rejected", ok)
		}
	}
	for _, bad := range []string{"", "max", "effort", "Medium"} {
		if ValidThinking(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
