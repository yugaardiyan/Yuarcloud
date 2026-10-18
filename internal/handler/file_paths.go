package handler

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// What a name may be, what is refused, and how a raw client string becomes a
// path that is safe to touch.
//
// Together because spreading them is the problem. Each handler writing its own
// check means two endpoints can treat the same input differently, and the one
// that forgot is only ever caught by the test that forgot too.

// reservedName reports whether a single path element is server-owned.
func reservedName(name string) bool {
	return strings.HasPrefix(name, internalPrefix)
}

// reservedPath reports whether any element of an already-normalised path is
// server-owned. Checked at the *top* level only: a user folder may legitimately
// be called "notes", and a file inside a user folder named ".yuarcloud-notes"
// is the user's own business. What must never happen is the path *entering* a
// top-level internal directory, since that is how a client addresses one.

// reservedPath reports whether any element of an already-normalised path is
// server-owned. Checked at the *top* level only: a user folder may legitimately
// be called "notes", and a file inside a user folder named ".yuarcloud-notes"
// is the user's own business. What must never happen is the path *entering* a
// top-level internal directory, since that is how a client addresses one.
func reservedPath(path string) bool {
	if path == "" {
		return false
	}
	head, _, _ := strings.Cut(path, "/")
	return reservedName(head)
}

// safeFilename reduces a client-supplied name to a single path element.
//
// filepath.Base only recognises the host separator, so a name carrying
// backslashes survives whole on Linux and becomes a traversal the moment the same
// code runs where those are separators. Normalising them first closes that gap,
// and the explicit ".." rejection is what Base alone does not catch — Base("..")
// returns "..", which is not a filename at all. A NUL byte is refused outright:
// it truncates the path in every C library the storage layer will pass it to.
//
// Returns ok=false for anything that cannot be a filename.

// safeFilename reduces a client-supplied name to a single path element.
//
// filepath.Base only recognises the host separator, so a name carrying
// backslashes survives whole on Linux and becomes a traversal the moment the same
// code runs where those are separators. Normalising them first closes that gap,
// and the explicit ".." rejection is what Base alone does not catch — Base("..")
// returns "..", which is not a filename at all. A NUL byte is refused outright:
// it truncates the path in every C library the storage layer will pass it to.
//
// Returns ok=false for anything that cannot be a filename.
func safeFilename(raw string) (string, bool) {
	name := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." || name == "/" || strings.ContainsRune(name, 0) {
		return "", false
	}
	if reservedName(name) {
		return "", false
	}
	// Control characters and DEL have no business in a filename. They cannot
	// survive an HTTP header, so every download of such a file emits a
	// Content-Disposition the receiver has to guess at; several shells treat a
	// NUL as a terminator and would truncate the name. The stored name would
	// also be impossible to type or match, which makes the file unreachable
	// through the search box but still present on disk. Reject at creation so
	// the problem never exists, rather than sanitising at every read.
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return name, true
}

// pathTaken reports whether a live row already claims `path`.
//
// Every write path that creates bytes needs this *before* touching storage.
// `LocalStorage.Write` is an os.Create, which truncates, and the unique index
// on files(path) then rejects the insert — at which point the rollback the
// handler reaches for removes the path, destroying the very file it was trying
// to protect. The user sees a 500 and their data is gone. Checking first turns
// that into a 409 with nothing written.
//
// Scoped to the caller so a path outside their home never reports occupied.

// pathTaken reports whether a live row already claims `path`.
//
// Every write path that creates bytes needs this *before* touching storage.
// `LocalStorage.Write` is an os.Create, which truncates, and the unique index
// on files(path) then rejects the insert — at which point the rollback the
// handler reaches for removes the path, destroying the very file it was trying
// to protect. The user sees a 500 and their data is gone. Checking first turns
// that into a 409 with nothing written.
//
// Scoped to the caller so a path outside their home never reports occupied.
func (h *FileHandler) pathTaken(sc scope.Scope, path string) bool {
	var n int64
	scoped(sc, h.db.Model(&model.File{})).
		Where("path = ? AND is_trashed = ?", path, false).
		Count(&n)
	return n > 0
}

// rejectPathTaken answers 409 when `path` is occupied, and reports whether it
// did.

// rejectPathTaken answers 409 when `path` is occupied, and reports whether it
// did.
func (h *FileHandler) rejectPathTaken(c *gin.Context, sc scope.Scope, path string) bool {
	if !h.pathTaken(sc, path) {
		return false
	}
	c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
	return true
}

// rejectPathOnDisk answers 409 when something already exists at `path` on the
// volume, whether or not the database knows about it.
//
// Complements rejectPathTaken, which is scoped and row-based. The gap between
// the two is where unreferenced data lives: a failed upload that wrote bytes
// before its row, a file placed in the volume outside the app, a directory left
// behind by an aborted move. Every write path that creates bytes needs both —
// checking only the rows means an existing user file with no row is silently
// replaced, and checking only the disk means a concurrent request that got as
// far as its row still loses.

// rejectPathOnDisk answers 409 when something already exists at `path` on the
// volume, whether or not the database knows about it.
//
// Complements rejectPathTaken, which is scoped and row-based. The gap between
// the two is where unreferenced data lives: a failed upload that wrote bytes
// before its row, a file placed in the volume outside the app, a directory left
// behind by an aborted move. Every write path that creates bytes needs both —
// checking only the rows means an existing user file with no row is silently
// replaced, and checking only the disk means a concurrent request that got as
// far as its row still loses.
func (h *FileHandler) rejectPathOnDisk(c *gin.Context, path string) bool {
	if path == "" {
		return false
	}
	exists, err := h.storage.Exists(context.Background(), path)
	if err != nil || !exists {
		return false
	}
	c.JSON(http.StatusConflict, gin.H{"error": "something already exists at that location"})
	return true
}

// validEntryName checks a client-supplied name for a new file or folder.
//
// Separators and traversal are rejected outright rather than cleaned, because
// cleaning would silently write the thing somewhere the user did not ask for
// — uploading "a/b.txt" and getting "b.txt" in the current folder is a surprise
// with no error to explain it.

// validEntryName checks a client-supplied name for a new file or folder.
//
// Separators and traversal are rejected outright rather than cleaned, because
// cleaning would silently write the thing somewhere the user did not ask for
// — uploading "a/b.txt" and getting "b.txt" in the current folder is a surprise
// with no error to explain it.
func validEntryName(raw string) bool {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." || reservedName(name) {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// cleanPath normalises a client-supplied path into the canonical form stored in
// the database: relative, slash-separated, no leading/trailing slash, no "." or
// ".." segments. Returns "" for the root.

// cleanPath normalises a client-supplied path into the canonical form stored in
// the database: relative, slash-separated, no leading/trailing slash, no "." or
// ".." segments. Returns "" for the root.
func cleanPath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\\", "/")

	segments := strings.Split(raw, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".", "..":
			// Drop empty/current/parent segments so ".." can never escape the root.
			continue
		}
		cleaned = append(cleaned, segment)
	}

	return strings.Join(cleaned, "/")
}

// childPathPatterns returns two SQL LIKE patterns (ESCAPE '\') for dir:
//   - match: every path that is a direct child of dir
//   - nested: every path that sits *below* a direct child (used with NOT LIKE)
//
// Together they restrict a query to a single directory level, which a plain
// "LIKE prefix%" cannot do on its own.

// childPathPatterns returns two SQL LIKE patterns (ESCAPE '\') for dir:
//   - match: every path that is a direct child of dir
//   - nested: every path that sits *below* a direct child (used with NOT LIKE)
//
// Together they restrict a query to a single directory level, which a plain
// "LIKE prefix%" cannot do on its own.
func childPathPatterns(dir string) (match string, nested string) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	// Escape LIKE metacharacters that may appear in a user-supplied dir name.
	esc := func(s string) string {
		return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
	}

	return esc(prefix) + "%", esc(prefix) + "%/%"
}
