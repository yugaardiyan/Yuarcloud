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
// The trailing separator is the whole point of this function. `Home + "%"`
// also matches every sibling whose name merely starts with the home's: a user
// homed at "team" would match "team-archive", and since SQLite's LIKE ignores
// ASCII case, "users/Bob" would match "users/bobby" as well. That is a
// cross-tenant read on the app's primary security boundary, which is why
// Contains and Rel both insist on Home + "/" and this must too.
// An unrestricted scope yields "" because there is nothing to filter on.
func (s Scope) ScopePattern() string {
	if s.Unrestricted() {
		return ""
	}
	return Escape(s.Home) + "/%"
}

// ScopeClause returns a WHERE fragment and its arguments that match the home
// row and everything below it. Pairing the equality arm here is what stops a
// caller from forgetting it and silently dropping the home folder from its own
// listing.
func (s Scope) ScopeClause() (string, []interface{}) {
	if s.Unrestricted() {
		return "", nil
	}
	return `(path = ? OR path LIKE ? ESCAPE '\')`, []interface{}{s.Home, s.ScopePattern()}
}

// Escape neutralises the LIKE metacharacters that can appear in a folder name,
// so a folder called "50%" cannot widen a scope filter.
func Escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}
