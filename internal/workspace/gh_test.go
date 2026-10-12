package workspace

import (
	"os"
	"strings"
	"testing"
)

// A recovery adopts a pull request only when it is open and comes from the
// branch in origin's own repository at the expected head.
func TestCheckPublication(t *testing.T) {
	repo, _, sha := shipped(t)
	pr := fakeGH(t)
	view := func(state, oid string, cross bool) {
		c := "false"
		if cross {
			c = "true"
		}
		os.WriteFile(pr, []byte(`{"url":"u","state":"`+state+`","headRefName":"feat/x","baseRefName":"main","isDraft":false,"headRefOid":"`+oid+`","isCrossRepository":`+c+`}`), 0o644)
	}
	view("OPEN", sha, false)
	if _, err := CheckPublication(repo, "feat/x", sha, "u"); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		state, oid string
		cross      bool
		head       string
	}{
		"closed":    {"CLOSED", sha, false, sha},
		"merged":    {"MERGED", sha, false, sha},
		"fork":      {"OPEN", sha, true, sha},
		"other oid": {"OPEN", "abc", false, sha},
		"moved":     {"OPEN", sha, false, "0000000000000000000000000000000000000000"},
		"absent":    {"OPEN", sha, false, ""},
	} {
		view(tc.state, tc.oid, tc.cross)
		if _, err := CheckPublication(repo, "feat/x", tc.head, "u"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := CheckPublication(repo, "feat/new", "", ""); err != nil {
		t.Fatalf("an absent branch expected absent: %v", err)
	}
}

// A fork's pull request with the same branch name makes discovery refuse.
func TestFindPRRefusesForks(t *testing.T) {
	repo, _, sha := shipped(t)
	pr := fakeGH(t)
	own := `{"url":"u1","state":"OPEN","headRefName":"feat/x","headRefOid":"` + sha + `","isCrossRepository":false}`
	fork := `{"url":"u2","state":"OPEN","headRefName":"feat/x","headRefOid":"abc","isCrossRepository":true}`
	os.WriteFile(pr, []byte("["+own+"]"), 0o644)
	if found, ok, err := FindPR(repo, "feat/x"); err != nil || !ok || found.URL != "u1" {
		t.Fatalf("own = %+v %v %v", found, ok, err)
	}
	os.WriteFile(pr, []byte("["+fork+"]"), 0o644)
	if _, _, err := FindPR(repo, "feat/x"); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("fork: %v", err)
	}
	os.WriteFile(pr, []byte("[]"), 0o644)
	if _, ok, err := FindPR(repo, "feat/x"); err != nil || ok {
		t.Fatalf("none: %v %v", ok, err)
	}
}
