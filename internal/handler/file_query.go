package handler

import (
	"fmt"
	"path"
	"strings"

	"gorm.io/gorm"
)

// Query fragments shared by more than one file: LIKE escaping, subtree
// matching, and the parent-directory helper.
//
// Separate because whereSubtree is called from CRUD, quota, rename and trash,
// and a helper that four files reach for does not belong inside any one of them.

// escapeLike neutralises LIKE metacharacters so user input matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}

// subtreeMatch builds the WHERE fragment and arguments matching `root` itself
// plus everything below it, in one call.
//
// The ESCAPE clause is not decoration. escapeLike inserts backslashes precisely
// so a name containing `_`, `%` or `\` is compared literally, and SQLite's LIKE
// has *no default escape character* — without the clause the backslash is
// matched literally as well and the pattern matches nothing at all. Seven call
// sites had that omission, and the effect was invisible: a folder called
// `my_files` returned zero descendants from every move, rename and delete, so
// its files kept paths that no longer existed and vanished from listings while
// still occupying quota. Postgres happens to default LIKE's escape to
// backslash, which is why this only ever bit on the shipped database.
//
// Both arms are required. `path = ?` catches the folder row itself; the LIKE
// catches the contents. Either alone is a different bug.
// subtreeMatch matches a row and everything nested below it.
//
// The root itself is included. That is right for "delete this subtree" and
// "relabel this subtree", and wrong for "what is inside this" — so descendantsOf
// excludes it explicitly rather than inheriting the choice from here. It used to
// get that exclusion for free from a user_id filter: the root is owned by
// whoever created it, and the filter happened to drop it along with any
// descendant belonging to someone else. Dropping the filter to make shared
// folders work exposed the assumption underneath, and a copy of a folder then
// tried to clone the folder onto itself.
func subtreeMatch(column, root string) (string, []interface{}) {
	return fmt.Sprintf("(%s = ? OR %s LIKE ? ESCAPE '\\')", column, column),
		[]interface{}{root, escapeLike(root) + "/%"}
}

// whereDescendants applies the same match as whereSubtree without the root row.
//
// Order is deepest-first, so a caller rewriting paths can process a child before
// its parent without either order mattering for correctness.
func (h *FileHandler) whereDescendants(root string) *gorm.DB {
	if root == "" {
		return h.db
	}
	return h.db.Where("path LIKE ? ESCAPE '\\'", escapeLike(root)+"/%").
		Order("LENGTH(path) DESC")
}

// whereSubtree applies subtreeMatch to a query.
//
// This exists as its own function rather than letting callers write
// `q.Where(subtreeMatch(...))`: Go expands a two-value call into two
// arguments, so the slice lands in the variadic `args ...interface{}` as a
// single element and gets bound to the first placeholder, leaving the second
// unbound. The query still compiles and matches nothing — the same silent
// failure this whole change is about.
func whereSubtree(q *gorm.DB, column, root string) *gorm.DB {
	clause, args := subtreeMatch(column, root)
	return q.Where(clause, args...)
}

func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}
