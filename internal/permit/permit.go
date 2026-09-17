// Package permit holds the write boundary. Today that is one question — is
// this path protected — asked of the planner's file list. The rollback and the
// scope file that the writing agents need are the same package's to add.
package permit

import (
	"path/filepath"
	"strings"
)

// Denied reports the first protected pattern path matches, if any. Two forms,
// both stdlib, because the list is short and a matcher is not worth writing:
//
//   - an entry ending in "/" is a directory name, matched against any segment
//     of the path, so ".git/" covers "vendor/x/.git/config" as well as ".git/HEAD"
//   - anything else is a filepath.Match glob against the basename, so "*.pem"
//     covers a key wherever it is
//
// path is repo-relative and slash-separated by the time it gets here; callers
// that start from an absolute path resolve it first.
func Denied(path string, deny []string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(path))
	segments := strings.Split(clean, "/")
	for _, pattern := range deny {
		if dir := strings.TrimSuffix(pattern, "/"); dir != pattern {
			for _, s := range segments {
				if s == dir {
					return pattern, true
				}
			}
			continue
		}
		// A bad pattern in the roster matches nothing rather than panicking;
		// filepath.Match only errors on a malformed glob.
		if ok, _ := filepath.Match(pattern, segments[len(segments)-1]); ok {
			return pattern, true
		}
	}
	return "", false
}

// Escapes reports whether a repo-relative path would land outside the repo.
// An absolute path counts: "every path in the plan is repo-relative" is the
// claim that makes the allow list a string comparison later.
func Escapes(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	clean := filepath.Clean(path)
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
