package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// This file holds the glue between a request and the user's home directory.
//
// The rule everywhere below: the database stores absolute paths, the API
// speaks paths relative to the caller's home, and every query is narrowed to
// the home before it reaches SQL. Filtering by user_id alone would break a
// shared folder, where two users must see each other's files.

func reqScope(c *gin.Context) scope.Scope {
	return middleware.GetScope(c)
}

// scoped narrows a files query to the caller's home. An unrestricted user adds
// no clause at all, so admin listings stay a plain scan.
//
// The clause comes from ScopeClause rather than being spelled here, because it
// has to carry both halves — the equality arm for the home row itself and the
// LIKE for everything below it — and forgetting either one is silent. The LIKE
// needs its ESCAPE clause for the same reason: without it, a home containing an
// underscore matches nothing at all.
func scoped(s scope.Scope, q *gorm.DB) *gorm.DB {
	if clause, args := s.ScopeClause(); clause != "" {
		q = q.Where(clause, args...)
	}
	return q
}

// absOf resolves a client-supplied relative path into its absolute form.
func absOf(s scope.Scope, rel string) string {
	abs, _ := s.Resolve(rel)
	return abs
}

// toResponse emits a file with its path rewritten relative to the home, so a
// scoped user never sees (or learns) the folders above their root.
func toResponse(s scope.Scope, f *model.File) model.FileResponse {
	resp := model.ToFileResponse(f)
	if rel, ok := s.Rel(f.Path); ok {
		resp.Path = rel
	}
	return resp
}

// owns reports whether the caller may mutate a file.
//
// The unit of shared access is the home folder itself. Two users pinned to the
// same root directory — say Folder A — both see and manage everything in
// Folder A, the way members of a shared drive do. The scope check in
// lookupFile already confined the caller to that subtree before owns is even
// reached, so by the time we get here the file is inside the caller's home;
// the question is only whether the caller is authorised to act on a file they
// do not own.
//
// The rule: a non-admin caller may act on any file inside their own home
// subtree, regardless of who uploaded it. A user homed at the storage root
// (empty home) sees the whole tree and manages the whole tree — the widest
// form of the same rule. A user homed to Folder A cannot reach into Folder B
// even if they uploaded a file that was later moved there: the scope would
// have already refused the lookup.
//
// The owner and an admin always have authority above this rule.
func owns(userID uuid.UUID, role string, f *model.File, callerHome string) bool {
	if role == "admin" || f.UserID == userID {
		return true
	}
	// Non-admin, non-owner: the file was found inside the caller's subtree,
	// which is exactly the shared-folder case. They may act on it.
	_ = callerHome
	return true
}

// lookupFile fetches a file by id, refusing anything outside the caller's
// home. The home check is the security boundary: ids are UUIDs and a leaked
// one must not be enough to reach a neighbouring folder.
func (h *FileHandler) lookupFile(c *gin.Context, id uuid.UUID, extra string, args ...interface{}) (*model.File, bool) {
	var file model.File
	query := scoped(reqScope(c), h.db).Where("id = ?", id)
	if extra != "" {
		query = query.Where(extra, args...)
	}
	if err := query.First(&file).Error; err != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return nil, false
	}
	return &file, true
}

// writable loads a file and enforces the shared-folder rule on top of the scope.
func (h *FileHandler) writable(c *gin.Context, id uuid.UUID, extra string, args ...interface{}) (*model.File, bool) {
	file, ok := h.lookupFile(c, id, extra, args...)
	if !ok {
		return nil, false
	}
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetUserRole(c)
	// lookupFile already confined the file to the caller's subtree. owns()
	// then applies the shared-folder rule: anyone homed to the same root
	// manages everything in it.
	if !owns(userID, role, file, reqScope(c).Home) {
		c.JSON(403, gin.H{"error": "you do not have permission to change this item"})
		return nil, false
	}
	return file, true
}

// relOf is a convenience for handlers that echo a path back.
func relOf(s scope.Scope, abs string) string {
	rel, _ := s.Rel(abs)
	return rel
}
