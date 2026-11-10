package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// cleanSharePath normalises the `path` parameter on a public share request.
//
// The value comes from the visitor, so it is not trusted to stay inside the
// shared folder. Empty means "the shared item itself". A "." or ".." segment,
// or an empty one from "a//b", is rejected rather than resolved: paths in the
// database never contain either, and silently collapsing them would turn a
// malformed request into a different, valid-looking one.
func cleanSharePath(raw string) (string, bool) {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" {
		return "", true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}

	return p, true
}

// shareTarget resolves the item a public download, preview or browse request
// refers to.
//
// No path means the first shared item. Anything else has to sit inside one of
// the shared roots: the prefix check compares whole segments, so "Photos old"
// cannot masquerade as a child of "Photos", and the row lookup carries the
// owner's id, so a crafted path cannot reach another account's tree. Trashed
// rows are excluded for the same reason ValidateShare excludes them — a link
// must not resurrect what its owner threw away.
//
// `roots` is the live item set of the link: one entry for an ordinary share,
// every item for a multi-item one, which is what lets a folder inside any
// bundle member be browsed and downloaded by path.
func (h *ShareHandler) shareTargetIn(c *gin.Context, roots []model.File) (*model.File, bool) {
	p, ok := cleanSharePath(c.Query("path"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}
	if len(roots) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}
	if p == "" {
		return &roots[0], true
	}

	var root *model.File
	for i := range roots {
		if p == roots[i].Path || strings.HasPrefix(p, roots[i].Path+"/") {
			root = &roots[i]
			break
		}
	}
	if root == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}
	if p == root.Path {
		return root, true
	}

	// A deep link can name a folder or file whose row does not exist yet
	// because nobody has browsed to it. Adopt the chain below the shared root
	// before looking it up. A symlink or a missing path is treated exactly
	// like a missing row.
	if _, err := adoptPathFrom(h.db, h.storage, root.UserID, root.Path, root, p); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}

	var target model.File
	if err := h.db.Where("user_id = ? AND path = ? AND is_trashed = ?", root.UserID, p, false).
		First(&target).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}

	return &target, true
}

// ListSharedFolder returns the direct children of a shared folder (or of a
// folder inside it) to a visitor with no account.
//
// The storage directory is listed first so files that already existed on disk
// can be adopted into rows owned by the link's creator. The SQL query is still
// path-scoped to the folder, which is what keeps other trees out; the owner
// filter then matches the rows adoption creates.
func (h *ShareHandler) ListSharedFolder(c *gin.Context) {
	ctx := c.Request.Context()

	link, err := h.shareService.ValidateShare(ctx, c.Param("token"), c.Query("password"))
	if err != nil {
		h.writeShareAccessError(c, err)
		return
	}

	roots := share.LiveShareFiles(link)
	if len(roots) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "share is no longer available"})
		return
	}

	// A multi-item link has no single folder to open at its root: its "root"
	// is the list of shared items itself. The page gets that list from the
	// access response, but answering here too keeps the endpoint honest for a
	// reload or a hand-built request. A path still opens a folder inside any
	// member, which is what the browse rows ask for.
	if len(roots) > 1 {
		p, ok := cleanSharePath(c.Query("path"))
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder is no longer available"})
			return
		}
		if p == "" {
			foldersFirst(roots)
			items := make([]model.FileResponse, 0, len(roots))
			for i := range roots {
				items = append(items, fileResponseOf(&roots[i]))
			}
			c.JSON(http.StatusOK, gin.H{"items": items})
			return
		}
	}

	folder, ok := h.shareTargetIn(c, roots)
	if !ok {
		return
	}
	if !folder.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this path is not a folder"})
		return
	}

	// A shared folder is often the first place someone points at a library
	// that already existed on disk. Its children need rows before the query
	// below can return them, and the visitor has no account to own those rows;
	// the link's owner does, just as for an upload through the share.
	if h.storage != nil {
		entries, err := h.storage.List(ctx, folder.Path)
		if err != nil {
			if err == storage.ErrNotFound {
				c.JSON(http.StatusNotFound, gin.H{"error": "folder is no longer available"})
				return
			}
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list folder"})
			return
		}
		if err := adoptDirectoryEntries(h.db, folder.UserID, folder.Path, folder, entries); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index folder"})
			return
		}
	}

	pattern, nested := childPathPatterns(folder.Path)
	var children []model.File
	query := h.db.WithContext(ctx).
		Where("user_id = ? AND is_trashed = ? AND path LIKE ? ESCAPE '\\'", folder.UserID, false, pattern)
	if nested != "" {
		query = query.Where("path NOT LIKE ? ESCAPE '\\'", nested)
	}
	if err := query.Order("is_dir DESC, name ASC").Find(&children).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list folder"})
		return
	}

	// LIKE folds ASCII case, so the prefix above can also match a sibling folder
	// whose name differs only by case (`Docs/` vs `docs/`). The SQL narrows the
	// scan; this exact, case-sensitive check is what makes the listing contain
	// the shared folder's own children and nothing else. The nested LIKE has the
	// same hazard for subfolders, so the direct-child test doubles as the fix.
	prefix := ""
	if folder.Path != "" {
		prefix = folder.Path + "/"
	}
	direct := make([]model.File, 0, len(children))
	for _, child := range children {
		if !strings.HasPrefix(child.Path, prefix) {
			continue
		}
		if strings.Contains(strings.TrimPrefix(child.Path, prefix), "/") {
			continue
		}
		direct = append(direct, child)
	}
	children = direct

	items := make([]model.FileResponse, 0, len(children))
	for i := range children {
		items = append(items, fileResponseOf(&children[i]))
	}

	c.JSON(http.StatusOK, gin.H{
		"folder": fileResponseOf(folder),
		"items":  items,
	})
}

// fileResponseOf is the single shape every public browse response uses, so a
// child and the folder holding it cannot drift apart.
func fileResponseOf(f *model.File) model.FileResponse {
	return model.FileResponse{
		ID:        f.ID,
		Name:      f.Name,
		Path:      f.Path,
		Size:      f.Size,
		MimeType:  f.MimeType,
		IsDir:     f.IsDir,
		Checksum:  f.Checksum,
		ParentID:  f.ParentID,
		IsTrashed: f.IsTrashed,
		TrashedAt: f.TrashedAt,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}
