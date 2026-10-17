package workspace

import (
	"os"
	"path/filepath"
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

// Every way the pull request or origin's branch can have moved on refuses the
// revision and names what to restore; nothing is switched or repaired. The
// local checkout's branch, HEAD and changes do not matter: a revision runs in
// a worktree of its own.
func TestCheckRevisable(t *testing.T) {
	const open = `{"url":"u","state":"OPEN","headRefName":"feat/x","baseRefName":"main"}`
	commit := func(t *testing.T, repo string) {
		os.WriteFile(filepath.Join(repo, "a.txt"), []byte("mine\n"), 0o644)
		gitTest(t, repo, "commit", "-qam", "manual")
	}
	for _, tc := range []struct {
		name, pr, want string
		change         func(t *testing.T, repo, origin, sha string)
	}{
		{"eligible", open, "", nil},
		{"checkout on another branch", open, "", func(t *testing.T, repo, _, _ string) { gitTest(t, repo, "checkout", "-q", "main") }},
		{"an unpushed local commit", open, "", func(t *testing.T, repo, _, _ string) { commit(t, repo) }},
		{"a dirty checkout", open, "", func(t *testing.T, repo, _, _ string) {
			os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty\n"), 0o644)
		}},
		{"local branch reset back", open, "", func(t *testing.T, repo, _, _ string) { gitTest(t, repo, "reset", "-q", "--hard", "HEAD^") }},
		{"a push to the pull request", open, "is at", func(t *testing.T, repo, _, sha string) {
			commit(t, repo)
			gitTest(t, repo, "push", "-q", "origin", "feat/x")
			gitTest(t, repo, "reset", "-q", "--hard", sha)
		}},
		{"branch deleted on origin", open, "does not exist", func(t *testing.T, _, origin, _ string) {
			gitTest(t, origin, "update-ref", "-d", "refs/heads/feat/x")
		}},
		{"closed", strings.Replace(open, "OPEN", "CLOSED", 1), "is closed", nil},
		{"merged", strings.Replace(open, "OPEN", "MERGED", 1), "is merged", nil},
		{"another head", strings.Replace(open, `"feat/x"`, `"feat/y"`, 1), "comes from feat/y", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := fakeGH(t)
			os.WriteFile(pr, []byte(tc.pr), 0o644)
			repo, origin, sha := shipped(t)
			if tc.change != nil {
				tc.change(t, repo, origin, sha)
			}
			before, _ := Head(repo)
			got, err := CheckRevisable(repo, Evidence{PR: "u", Branch: "feat/x", Commit: sha})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want == "" && got.Base != "main":
				t.Fatalf("pr = %+v", got)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			if after, _ := Head(repo); after != before {
				t.Fatal("the check moved HEAD")
			}
		})
	}
}
