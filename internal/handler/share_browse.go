package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yuarcloud/yuarcloud/internal/model"
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
// No path means the shared item itself. Anything else has to sit inside the
// shared folder: the prefix check compares whole segments, so "Photos old"
// cannot masquerade as a child of "Photos", and the row lookup carries the
// owner's id, so a crafted path cannot reach another account's tree. Trashed
// rows are excluded for the same reason ValidateShare excludes them — a link
// must not resurrect what its owner threw away.
func (h *ShareHandler) shareTarget(c *gin.Context, root *model.File) (*model.File, bool) {
	p, ok := cleanSharePath(c.Query("path"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
	}

	if p == "" || p == root.Path {
		return root, true
	}

	if !strings.HasPrefix(p, root.Path+"/") {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return nil, false
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

	root := &link.File
	if !root.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this share is not a folder"})
		return
	}

	folder, ok := h.shareTarget(c, root)
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
		if err := adoptDirectoryEntries(h.db, root.UserID, folder.Path, folder, entries); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to index folder"})
			return
		}
	}

	pattern, nested := childPathPatterns(folder.Path)
	var children []model.File
	query := h.db.WithContext(ctx).
		Where("user_id = ? AND is_trashed = ? AND path LIKE ? ESCAPE '\\'", root.UserID, false, pattern)
	if nested != "" {
		query = query.Where("path NOT LIKE ? ESCAPE '\\'", nested)
	}
	if err := query.Order("is_dir DESC, name ASC").Find(&children).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list folder"})
		return
	}

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
