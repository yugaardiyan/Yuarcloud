package handler

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
	"gorm.io/gorm"
)

type FileHandler struct {
	db      *gorm.DB
	storage storage.StorageBackend
	// Only used to mint the short-lived preview tokens — see preview.go.
	authService *auth.AuthService
}

func NewFileHandler(db *gorm.DB, storage storage.StorageBackend, authService *auth.AuthService) *FileHandler {
	return &FileHandler{
		db:          db,
		storage:     storage,
		authService: authService,
	}
}

// The ceilings are read from storage on every check rather than copied into the
// handler at construction. Holding an int64 here meant an admin's save moved the
// number in the panel while the upload path kept enforcing whatever the process
// booted with.

// The ceilings are read from storage on every check rather than copied into the
// handler at construction. Holding an int64 here meant an admin's save moved the
// number in the panel while the upload path kept enforcing whatever the process
// booted with.
func (h *FileHandler) maxFileSize() int64 { return h.storage.GetLimits().MaxFileSize() }

func (h *FileHandler) maxChunkSize() int64 { return h.storage.GetLimits().MaxChunkSize() }

// internalPrefix marks everything the server owns. Names carrying it are not
// addressable by a client, whatever they contain.
//
// Two directories live under the storage root and neither is a user's data: the
// trash, and — in the container layout, where temp_dir sits beside the user's
// files — the chunk scratch directory holding half-written uploads keyed by
// upload id. The trash had a guard, written per-handler, and the chunk
// directory had none. A caller could name a path under it, and while a
// half-written chunk has no row so nothing would be served, the directory was
// one step from being reachable, and the guarantee depended on that staying
// true. The container config puts it at /srv/.yuarcloud-chunks, directly beside
// user data, precisely so the same volume serves both.
//
// One prefix, checked centrally, means the next internal directory cannot be
// added without also being covered. The value now lives in the storage package,
// which has to filter on it too when listing: this file once carried its own
// copy while the storage layer decided independently with a much broader rule.

// internalPrefix marks everything the server owns. Names carrying it are not
// addressable by a client, whatever they contain.
//
// Two directories live under the storage root and neither is a user's data: the
// trash, and — in the container layout, where temp_dir sits beside the user's
// files — the chunk scratch directory holding half-written uploads keyed by
// upload id. The trash had a guard, written per-handler, and the chunk
// directory had none. A caller could name a path under it, and while a
// half-written chunk has no row so nothing would be served, the directory was
// one step from being reachable, and the guarantee depended on that staying
// true. The container config puts it at /srv/.yuarcloud-chunks, directly beside
// user data, precisely so the same volume serves both.
//
// One prefix, checked centrally, means the next internal directory cannot be
// added without also being covered. The value now lives in the storage package,
// which has to filter on it too when listing: this file once carried its own
// copy while the storage layer decided independently with a much broader rule.
const internalPrefix = storage.InternalPrefix

// reservedName reports whether a single path element is server-owned.

func (h *FileHandler) ListFiles(c *gin.Context) {
	sc := reqScope(c)

	// Normalise the requested path, then lift it into the caller's home. A
	// scoped user asking for "../x" resolves to "<home>/x", never above it.
	path := absOf(sc, c.Query("path"))

	// Internal directories are reachable only through the routes that own
	// them. One check covers the trash and the chunk scratch directory, which
	// previously had none.
	if reservedPath(path) {
		c.JSON(http.StatusForbidden, gin.H{"error": "that location is not browsable"})
		return
	}

	// Resolve the parent folder. parent_id is only a hint; the authoritative
	// source is the path itself, so listing works even without the id.
	var parentFolder *model.File
	if pidStr := c.Query("parent_id"); pidStr != "" {
		if pid, err := uuid.Parse(pidStr); err == nil {
			scoped(sc, h.db).First(&parentFolder, "id = ? AND is_dir = ?", pid, true)
		}
	}
	if parentFolder == nil && path != sc.Home {
		var found model.File
		if err := scoped(sc, h.db).First(&found, "path = ? AND is_dir = ?", path, true).Error; err == nil {
			parentFolder = &found
		}
	}

	// List the directory contents from storage.
	files, err := h.storage.List(context.Background(), path)
	if err != nil {
		if err == storage.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "directory not found"})
			return
		}
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list directory"})
		return
	}

	// Load the DB rows for the direct children of `path` inside the caller's
	// home. Scoping by path prefix (not parent_id) keeps the home listing
	// working, where parent_id is NULL. The NOT LIKE excludes deeper nesting so
	// only immediate children come back.
	var candidates []model.File
	query := scoped(sc, h.db)
	if pattern, nested := childPathPatterns(path); pattern != "" {
		query = query.Where("path LIKE ? ESCAPE '\\'", pattern)
		if nested != "" {
			query = query.Where("path NOT LIKE ? ESCAPE '\\'", nested)
		}
	}
	query.Find(&candidates)

	dbFileMap := make(map[string]*model.File, len(candidates))
	for i := range candidates {
		dbFileMap[candidates[i].Path] = &candidates[i]
	}

	response := make([]model.FileResponse, 0, len(files))
	for _, f := range files {
		// The disk listing spans the whole storage root, so it still contains
		// other users' trees. A database row scoped to this home is what proves
		// the entry belongs in this view; skip anything it does not cover.
		dbFile, exists := dbFileMap[f.Path]
		if !exists || dbFile.IsTrashed {
			continue
		}
		response = append(response, model.FileResponse{
			ID:        dbFile.ID,
			Name:      f.Name,
			Path:      relOf(sc, f.Path),
			Size:      dbFile.Size,
			MimeType:  dbFile.MimeType,
			IsDir:     f.IsDir,
			Checksum:  dbFile.Checksum,
			ParentID:  dbFile.ParentID,
			CreatedAt: dbFile.CreatedAt,
			UpdatedAt: dbFile.UpdatedAt,
		})
	}

	// Skip DB rows whose file is missing on disk. Earlier this loop
	// surfaced those records so the user could see them, but they were
	// unopenable / untrashable ghosts that only ever confused the UI and
	// broke "move to trash" (the rename target doesn't exist). A separate
	// admin/maintenance tool is the right place to reconcile them; the
	// listing itself should only show files that actually live on disk.

	c.JSON(http.StatusOK, gin.H{
		"files": response,
		"path":  relOf(sc, path),
		"parent": func() *model.FileResponse {
			if parentFolder != nil {
				return &model.FileResponse{
					ID:       parentFolder.ID,
					Name:     parentFolder.Name,
					Path:     parentFolder.Path,
					IsDir:    true,
					ParentID: parentFolder.ParentID,
				}
			}
			return nil
		}(),
	})
}

func (h *FileHandler) GetFile(c *gin.Context) {
	sc := reqScope(c)
	fileID := c.Param("id")

	var file model.File
	if err := scoped(sc, h.db).First(&file, "id = ? AND is_trashed = ?", fileID, false).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	c.JSON(http.StatusOK, toResponse(sc, &file))
}

func (h *FileHandler) CreateFolder(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var req struct {
		Name       string     `json:"name" binding:"required"`
		ParentID   *uuid.UUID `json:"parent_id"`
		ParentPath string     `json:"parent_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Reject path separators and traversal attempts in the folder name.
	name := strings.TrimSpace(req.Name)
	if !validEntryName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid folder name"})
		return
	}

	parentPath, parentID, ok := h.resolveDestination(c, userID, req.ParentID, req.ParentPath)
	if !ok {
		return
	}

	newPath := cleanPath(filepath.Join(parentPath, name))
	if newPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid folder name"})
		return
	}

	// Refuse to create anything at or inside a server-owned directory.
	if reservedPath(newPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot create items in a reserved location"})
		return
	}

	// Don't clobber an existing entry. Any row at this path counts, not just the
	// caller's own, because in a shared folder a sibling may already be there.
	var existing int64
	scoped(reqScope(c), h.db.Model(&model.File{})).
		Where("path = ? AND is_trashed = ?", newPath, false).
		Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
		return
	}

	if err := h.storage.Mkdir(context.Background(), newPath); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create folder"})
		return
	}

	folder := &model.File{
		UserID:   userID,
		ParentID: parentID,
		Name:     name,
		Path:     newPath,
		IsDir:    true,
		Size:     0,
		MimeType: "inode/directory",
	}

	if err := h.db.Create(folder).Error; err != nil {
		h.storage.Remove(context.Background(), newPath) // roll back the directory
		c.Error(err)                                    //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create folder record"})
		return
	}

	logAudit(h.db, c, userID, "create_folder", "file", &folder.ID, "Created folder: "+newPath)

	c.JSON(http.StatusCreated, model.ToFileResponse(folder))
}

func (h *FileHandler) UploadFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	// Single file upload (non-chunked)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeUploadReadError(c, err, "no file provided")
		return
	}

	parentIDStr := c.PostForm("parent_id")
	var parentID *uuid.UUID
	if parentIDStr != "" {
		if pid, err := uuid.Parse(parentIDStr); err == nil {
			parentID = &pid
		}
	}

	// Determine target path. Without a parent the upload lands in the
	// caller's home, which is their root as far as the API is concerned.
	sc := reqScope(c)
	parentPath := sc.Home
	if parentID != nil {
		var parent model.File
		if err := scoped(sc, h.db).First(&parent, "id = ? AND is_dir = ?", *parentID, true).Error; err == nil {
			parentPath = parent.Path
		}
	}

	safeName, ok := safeFilename(fileHeader.Filename)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}

	targetPath := filepath.Join(parentPath, safeName)
	targetPath = strings.TrimPrefix(targetPath, "/")

	// Check file size
	if max := h.maxFileSize(); max > 0 && fileHeader.Size > max {
		// 413, not 400: the request is well-formed, the payload is simply
		// too big. 400 tells a client it sent something the server could not
		// parse, which is not true and sends the caller looking for a syntax
		// error that does not exist.
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
		return
	}

	// Quota is enforced here rather than after the bytes are on disk. The
	// counter only ever grew before, so it recorded what had happened instead
	// of bounding what could — a user was told about their limit on a page that
	// was never consulted before writing.
	if rejectOverQuota(c, h.db, userID, fileHeader.Size, inflightUploadBytes(h.db, userID)) {
		return
	}

	// Before the write, not after. storage.Write truncates whatever is at the
	// target, so an occupied path here means the existing bytes are destroyed
	// before the unique index gets a chance to refuse the insert — and the
	// rollback would then delete the path outright.
	if h.rejectPathTaken(c, sc, targetPath) || h.rejectPathOnDisk(c, targetPath) {
		return
	}

	// Open uploaded file
	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open uploaded file"})
		return
	}
	defer src.Close()

	// Write to storage
	if err := h.storage.Write(context.Background(), targetPath, src, fileHeader.Size); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}

	// Calculate checksum
	checksum := ""
	if reader, err := h.storage.Read(context.Background(), targetPath); err == nil {
		// In real implementation, calculate SHA256
		reader.Close()
	}

	// Create DB record. Name and mime both read off the sanitised name, not the
	// raw header — otherwise the row could disagree with the path it points at.
	mimeType := fileHeader.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = mime.TypeByExtension(filepath.Ext(safeName))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	file := &model.File{
		UserID:      userID,
		ParentID:    parentID,
		Name:        safeName,
		Path:        targetPath,
		Size:        fileHeader.Size,
		MimeType:    mimeType,
		IsDir:       false,
		Checksum:    checksum,
		StorageType: "local",
		StoragePath: targetPath,
	}

	if err := h.db.Create(file).Error; err != nil {
		h.storage.Remove(context.Background(), targetPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file record"})
		return
	}

	// Update user storage usage
	h.db.Model(&model.User{}).Where("id = ?", userID).UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", fileHeader.Size))

	logAudit(h.db, c, userID, "upload", "file", &file.ID, fmt.Sprintf("Uploaded file: %s", safeName))

	c.JSON(http.StatusCreated, toResponse(sc, file))
}

func (h *FileHandler) DownloadFile(c *gin.Context) {
	sc := reqScope(c)
	fileID := c.Param("id")

	var file model.File
	if err := scoped(sc, h.db).First(&file, "id = ? AND is_trashed = ?", fileID, false).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	if file.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot download folder"})
		return
	}

	reader, err := h.storage.Read(context.Background(), file.Path)
	if err != nil {
		if err == storage.ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found in storage"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file"})
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", contentDisposition("attachment", file.Name))
	c.Header("Content-Type", file.MimeType)
	c.Header("Content-Length", fmt.Sprintf("%d", file.Size))
	c.DataFromReader(http.StatusOK, file.Size, file.MimeType, reader, nil)
}

func (h *FileHandler) DeleteFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	// permanent=true bypasses the trash; otherwise the item is recoverable.
	permanent := c.Query("permanent") == "true"

	// Visible inside the home, and only the owner (or an admin) may remove it —
	// a shared folder must not let one user delete another's work.
	var file *model.File
	var ok bool
	if permanent {
		file, ok = h.writable(c, fileID, "")
	} else {
		file, ok = h.writable(c, fileID, "is_trashed = ?", false)
	}
	if !ok {
		return
	}

	if permanent {
		if err := h.storage.Remove(context.Background(), file.Path); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete from storage"})
			return
		}
		// Release the quota while the rows still exist.
		//
		// releaseSubtreeUsage measures by querying the subtree, so running it
		// after the delete found nothing and released nothing — a live file
		// deleted outright left its uploader charged for bytes that are gone,
		// and used_bytes ratcheted up a little on every permanent delete until
		// the account could not upload anything more. Trash gets this right by
		// measuring before it rewrites the paths; the delete path has to
		// measure before it removes them.
		h.releaseSubtreeUsage(file.Path, -1)

		// Subtree rows belong to whoever created them, so drop by path rather
		// than by owner. One transaction and one error check: the bulk write used
		// to be fire-and-forget while only the root row was checked, so a
		// failure left descendants alive with their quota already released.
		if err := h.db.Transaction(func(tx *gorm.DB) error {
			if e := whereSubtree(tx, "path", file.Path).Delete(&model.File{}).Error; e != nil {
				return e
			}
			return tx.Delete(file).Error
		}); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete file record"})
			return
		}
		logAudit(h.db, c, userID, "delete_permanent", "file", &file.ID, "Deleted: "+file.Name)
		c.JSON(http.StatusOK, gin.H{"message": "deleted"})
		return
	}

	if err := h.MoveToTrashRecord(c, userID, file); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to move item to trash"})
		return
	}

	logAudit(h.db, c, userID, "trash", "file", &file.ID, "Moved to trash: "+file.Name)
	c.JSON(http.StatusOK, gin.H{"message": "moved to trash", "id": fileID})
}
