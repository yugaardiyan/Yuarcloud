// Package scope implements per-user directory confinement, the FileBrowser
// "home directory" model.
//
// A user is pinned to a subtree of the storage root. Paths that arrive from a
// client are *relative to that subtree*: a user homed at "work/notes" asking
// for "today.md" means "work/notes/today.md", and has no way to name anything
// outside it. Paths stored in the database stay absolute, so the on-disk layout
// is a single flat tree and the scope is purely a view over it.
//
// Two users homed at the same directory therefore see the same files, which is
// what makes a shared folder work: the confinement is by path, not by owner.
package scope

import "strings"

// Scope is the resolved home directory for one request.
type Scope struct {
	// Home is the absolute, cleaned path the user is confined to. The empty
	// string means the storage root, i.e. no confinement.
	Home string
}

// New builds a Scope from a stored home path. Anything empty, "/" or "." is
// treated as "unrestricted".
func New(home string) Scope {
	return Scope{Home: Clean(home)}
}

// Unrestricted reports whether the user can reach the whole storage tree.
func (s Scope) Unrestricted() bool {
	return s.Home == ""
}

// Clean normalises a slash-separated path: backslashes become slashes, and
// empty / "." / ".." segments are dropped so a stored home cannot itself
// contain traversal.
func Clean(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")

	segments := strings.Split(raw, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".", "..":
			continue
		}
		cleaned = append(cleaned, segment)
	}
	return strings.Join(cleaned, "/")
}

// Resolve turns a client-supplied relative path into an absolute path inside
// the scope. It reports false when the result would fall outside the home.
//
// Because Clean strips ".." before the prefix is applied, a client cannot walk
// upward: "../secret" collapses to "secret" and lands inside the home, which is
// the same behaviour the storage layer already relies on.
func (s Scope) Resolve(rel string) (string, bool) {
	rel = Clean(rel)
	if s.Unrestricted() {
		return rel, true
	}
	if rel == "" {
		return s.Home, true
	}
	return s.Home + "/" + rel, true
}

// Rel converts an absolute path back to the client-facing relative path. It
// reports false when the path lies outside the scope, which callers use to
// refuse leaking names the user should not see.
func (s Scope) Rel(abs string) (string, bool) {
	abs = Clean(abs)
	if s.Unrestricted() {
		return abs, true
	}
	if abs == s.Home {
		return "", true
	}
	if !strings.HasPrefix(abs, s.Home+"/") {
		return "", false
	}
	return strings.TrimPrefix(abs, s.Home+"/"), true
}

// Contains reports whether an absolute path is the home itself or below it.
func (s Scope) Contains(abs string) bool {
	abs = Clean(abs)
	if s.Unrestricted() {
		return true
	}
	return abs == s.Home || strings.HasPrefix(abs, s.Home+"/")
}

// ScopePattern returns a SQL LIKE pattern (ESCAPE '\') matching everything
// strictly *below* the scope's home. The home row itself is not matched — a
// folder and its contents are both rows, and callers that need the home use
// ScopeClause, which adds the equality arm.
//
// Kept for callers that genuinely need a LIKE pattern. ScopeClause no longer
// uses it: SQLite's LIKE folds ASCII case, so `LIKE 'users/Bob/%'` matches
// `users/bobby/...` — a cross-tenant read on the app's primary security
// boundary. The trailing separator below is necessary but not sufficient.
func (s Scope) ScopePattern() string {
	if s.Unrestricted() {
		return ""
	}
	return Escape(s.Home) + "/%"
}

// ScopeRange returns the half-open, case-sensitive bounds of everything
// strictly *below* the home. A BINARY string comparison (the SQLite default)
// keeps case-variant siblings apart, which LIKE cannot do: home "team" must
// not match "Team/x", and both directories can exist on a case-sensitive
// volume.
//
// The upper bound is Home + "0" rather than Home + "\xff": '/' is the byte
// immediately before '0', so every path with the prefix Home+"/" sorts into
// [Home+"/", Home+"0"), and nothing else does. An unrestricted scope yields
// empty bounds because there is nothing to filter on.
func (s Scope) ScopeRange() (string, string) {
	if s.Unrestricted() {
		return "", ""
	}
	return s.Home + "/", s.Home + "0"
}

// ScopeClause returns a WHERE fragment and its arguments that match the home
// row and everything below it. Pairing the equality arm here is what stops a
// caller from forgetting it and silently dropping the home folder from its own
// listing.
func (s Scope) ScopeClause() (string, []interface{}) {
	if s.Unrestricted() {
		return "", nil
	}
	lower, upper := s.ScopeRange()
	return `(path = ? OR (path >= ? AND path < ?))`, []interface{}{s.Home, lower, upper}
}

// Escape neutralises the LIKE metacharacters that can appear in a folder name,
// so a folder called "50%" cannot widen a scope filter.
func Escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
