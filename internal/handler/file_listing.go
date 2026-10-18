package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

// Search, the two folder trees the move and copy pickers need, shares owned by
// the caller, and storage usage.

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (h *FileHandler) Search(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		c.JSON(http.StatusOK, gin.H{"files": []model.FileResponse{}, "query": query, "count": 0})
		return
	}

	limit := 100
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}

	pattern := "%" + escapeLike(query) + "%"

	var files []model.File
	sc := reqScope(c)
	if err := scoped(sc, h.db).
		Where("is_trashed = ? AND name LIKE ? ESCAPE '\\'", false, pattern).
		Order("is_dir DESC, name ASC").
		Limit(limit).
		Find(&files).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})
		return
	}

	response := make([]model.FileResponse, 0, len(files))
	for i := range files {
		response = append(response, toResponse(sc, &files[i]))
	}

	c.JSON(http.StatusOK, gin.H{
		"files":     response,
		"query":     query,
		"limit":     limit,
		"count":     len(response),
		"truncated": len(response) == limit,
	})
}

// ---------------------------------------------------------------------------
// Folder lists (move / copy destination picker)
// ---------------------------------------------------------------------------

func (h *FileHandler) FolderTree(c *gin.Context) {
	sc := reqScope(c)

	var folders []model.File
	if err := scoped(sc, h.db).
		Where("is_dir = ? AND is_trashed = ?", true, false).
		Order("path ASC").
		Find(&folders).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load folders"})
		return
	}

	// Nodes are held by pointer so appending to a parent's Children slice is
	// visible through the map. "path ASC" guarantees parents come first.
	nodes := make(map[string]*model.FolderNode, len(folders))
	order := make([]string, 0, len(folders))
	for i := range folders {
		f := &folders[i]
		nodes[f.Path] = &model.FolderNode{
			ID:       f.ID,
			Name:     f.Name,
			Path:     relOf(sc, f.Path),
			ParentID: f.ParentID,
		}
		order = append(order, f.Path)
	}

	var roots []*model.FolderNode
	for _, p := range order {
		node := nodes[p]
		if parent, ok := nodes[parentDir(p)]; ok && parent != node {
			parent.Children = append(parent.Children, *node)
			continue
		}
		// No parent (true root, or orphaned) — expose at the top level.
		roots = append(roots, node)
	}

	// Children were appended as value copies taken before their own children
	// were filled in, so refresh each subtree from the map.
	var refresh func(dst *model.FolderNode, src *model.FolderNode)
	refresh = func(dst, src *model.FolderNode) {
		if dst == nil || src == nil {
			return
		}
		dst.Children = nil
		for i := range src.Children {
			childPath := src.Children[i].Path
			child, ok := nodes[childPath]
			if !ok {
				continue
			}
			dst.Children = append(dst.Children, *child)
			refresh(&dst.Children[len(dst.Children)-1], child)
		}
	}

	out := make([]model.FolderNode, 0, len(roots))
	for _, root := range roots {
		node := *root
		refresh(&node, root)
		out = append(out, node)
	}

	c.JSON(http.StatusOK, gin.H{"folders": out})
}

func (h *FileHandler) FlatFolderList(c *gin.Context) {
	sc := reqScope(c)

	var folders []model.File
	scoped(sc, h.db).Where("is_dir = ? AND is_trashed = ?", true, false).
		Order("path ASC").Find(&folders)

	// The browser draws its listings from storage, not from here, so a database
	// row without a directory behind it is a folder the user cannot open, cannot
	// see and cannot account for — yet it would still be offered as somebody's
	// home directory, and picked from this list the assignment would fail later
	// with "home folder not found". Rows outlive their directories when a user
	// is deleted with a folder still on disk, or when a create is interrupted
	// between the two writes, so the two sources drift apart on their own.
	//
	// Checking each candidate against storage keeps the two in agreement: this
	// list can only name a directory that My Cloud would actually show.
	response := make([]model.FolderNode, 0, len(folders))
	for i := range folders {
		exists, err := h.storage.Exists(context.Background(), folders[i].Path)
		if err != nil || !exists {
			continue
		}
		response = append(response, model.FolderNode{
			ID:       folders[i].ID,
			Name:     folders[i].Name,
			Path:     relOf(sc, folders[i].Path),
			ParentID: folders[i].ParentID,
		})
	}

	c.JSON(http.StatusOK, gin.H{"folders": response})
}

// ---------------------------------------------------------------------------
// Shared with me
// ---------------------------------------------------------------------------

// ListShared returns shares the caller owns, annotated with the file so the
// client can render a "Shared" view without a second round-trip.
func (h *FileHandler) ListShared(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var shares []model.ShareLink
	if err := h.db.
		Preload("File").
		Where("user_id = ? AND is_active = ? AND file_id IN (?)",
			userID, true,
			h.db.Model(&model.File{}).Select("id").Where("user_id = ? AND is_trashed = ?", userID, false)).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load shared items"})
		return
	}

	type entry struct {
		model.FileResponse
		Token   string     `json:"token"`
		URL     string     `json:"url"`
		Views   int        `json:"views"`
		Expires *time.Time `json:"expires_at"`
		HasPass bool       `json:"has_password"`
		ShareID uuid.UUID  `json:"share_id"`
	}

	out := make([]entry, 0, len(shares))
	for i := range shares {
		s := &shares[i]
		out = append(out, entry{
			FileResponse: model.ToFileResponse(&s.File),
			Token:        s.Token,
			URL:          "/yc/" + s.Token,
			Views:        s.ViewCount,
			Expires:      s.ExpiresAt,
			HasPass:      s.PasswordHash != "",
			ShareID:      s.ID,
		})
	}

	c.JSON(http.StatusOK, gin.H{"shares": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Storage usage
// ---------------------------------------------------------------------------

func (h *FileHandler) StorageStats(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	sc := reqScope(c)

	// Counts describe what this user can actually reach inside the home, so
	// the numbers match the file list rather than the whole disk.
	var stats model.StorageStats
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", false, false).
		Count(&stats.TotalFiles)
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", true, false).
		Count(&stats.TotalFolders)
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", false, false).
		Select("COALESCE(SUM(size), 0)").Scan(&stats.TotalSize)

	var user model.User
	if err := h.db.First(&user, "id = ?", userID).Error; err == nil {
		stats.TotalQuota = user.QuotaBytes
	}

	c.JSON(http.StatusOK, stats)
}
