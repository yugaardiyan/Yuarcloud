package handler

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
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
func (h *FileHandler) maxFileSize() int64  { return h.storage.GetLimits().MaxFileSize() }
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
const internalPrefix = storage.InternalPrefix

// reservedName reports whether a single path element is server-owned.
func reservedName(name string) bool {
	return strings.HasPrefix(name, internalPrefix)
}

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
func (h *FileHandler) pathTaken(sc scope.Scope, path string) bool {
	var n int64
	scoped(sc, h.db.Model(&model.File{})).
		Where("path = ? AND is_trashed = ?", path, false).
		Count(&n)
	return n > 0
}

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

// subtreeSize totals the bytes of every non-dir row beneath root.
func (h *FileHandler) subtreeSize(userID uuid.UUID, root string) int64 {
	var total int64
	whereSubtree(h.db.Model(&model.File{}).Where("user_id = ? AND is_dir = ?", userID, false), "path", root).
		Select("COALESCE(SUM(size), 0)").Scan(&total)
	return total
}

// quotaExceeded reports whether adding additional bytes would push the user past
// their quota, and says by how much so the caller can put it in the message.
//
// `inflight` is the total size of uploads already in progress. Counting it is
// what stops the obvious hole in a check that only looks at used_bytes: a user
// with 10 GB free could open twenty sessions of 9 GB each, and every one of
// them would pass a check that only knew about what had already been finished.
// Summing the declared sizes is the reservation — it needs no separate reserve
// to release, because a session's bytes are only counted while the session
// itself is still alive. That is what makes a session-count cap unnecessary.
//
// A quota of zero means unlimited, matching how max_file_size already treats
// zero in the chunked settings.
//
// A package function rather than a FileHandler method because the share-link
// upload has to ask the same question about the link's owner, and ShareHandler
// has no reason to hold a FileHandler.
func quotaExceeded(db *gorm.DB, userID uuid.UUID, additional, inflight int64) (bool, int64) {
	var user model.User
	if err := db.Select("used_bytes", "quota_bytes").First(&user, "id = ?", userID).Error; err != nil {
		// Cannot read the quota. Refusing is the safe direction: letting it
		// through turns a transient database hiccup into an unlimited upload.
		return true, 0
	}
	if user.QuotaBytes <= 0 {
		return false, 0
	}

	used := user.UsedBytes + inflight + additional
	if used <= user.QuotaBytes {
		return false, 0
	}
	return true, used - user.QuotaBytes
}

// inflightUploadBytes totals what the user has declared but not yet finished.
// Expired sessions are left out: the sweeper may not have run yet, and their
// disk space is going away regardless, so counting them would lock the user out
// of their own remaining quota until the cleanup tick lands.
func inflightUploadBytes(db *gorm.DB, userID uuid.UUID) int64 {
	var total int64
	db.Model(&model.UploadSession{}).
		Where("user_id = ? AND status = ? AND expires_at > ?", userID, "uploading", time.Now()).
		Select("COALESCE(SUM(total_size), 0)").Scan(&total)
	return total
}

// rejectOverQuota answers the request with the shortfall spelled out, and
// reports whether it did. 507 is the honest status for "the server cannot store
// this", and it keeps the error shape — {error, quota, short_by} — separate from
// the 4xx that means the request itself was wrong.
func rejectOverQuota(c *gin.Context, db *gorm.DB, userID uuid.UUID, additional, inflight int64) bool {
	over, by := quotaExceeded(db, userID, additional, inflight)
	if !over {
		return false
	}
	body := gin.H{"error": "storage quota exceeded", "quota": true}
	if by > 0 {
		body["short_by"] = by
	}
	c.JSON(http.StatusInsufficientStorage, body)
	return true
}

// adjustUsage keeps the cached used_bytes counter from going negative.
//
// Two things used to be wrong here. The guard query passed only `delta` for a
// WHERE clause with two placeholders, so GORM bound the UUID column to delta
// and matched nothing — the clamp never fired. And even when it did fire, the
// unconditional update afterwards re-applied the same negative delta to the
// value just written, turning a clamp into the exact value it was preventing.
// A counter that can go negative hands out free quota to anyone who deletes
// the same file twice.
func (h *FileHandler) adjustUsage(userID uuid.UUID, delta int64) {
	if delta == 0 {
		return
	}

	if delta < 0 {
		res := h.db.Model(&model.User{}).
			Where("id = ? AND used_bytes + ? < 0", userID, delta).
			UpdateColumn("used_bytes", 0)
		if res.Error != nil {
			// Nothing charged either way. Skipping the delta is the safe
			// direction: a replayed delete must not push the counter further
			// below zero.
			return
		}
		if res.RowsAffected > 0 {
			// Clamped. Applying the delta again would defeat it.
			return
		}
	}

	h.db.Model(&model.User{}).
		Where("id = ?", userID).
		UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", delta))
}

func (h *FileHandler) RenameFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	name := strings.TrimSpace(req.Name)
	if !validEntryName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
		return
	}

	sc := reqScope(c)
	file, ok := h.writable(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	oldPath := file.Path
	newPath := cleanPath(filepath.Join(parentDir(oldPath), name))

	// Renaming never crosses a boundary: the parent is unchanged by definition,
	// so this is a belt-and-braces check against a home that moved meanwhile.
	if !sc.Contains(newPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "outside your folder"})
		return
	}

	if newPath == oldPath {
		c.JSON(http.StatusOK, toResponse(sc, file)) // nothing to do
		return
	}
	if newPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
		return
	}
	if reservedPath(newPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot rename into a reserved location"})
		return
	}

	var existing int64
	scoped(sc, h.db.Model(&model.File{})).
		Where("path = ? AND is_trashed = ?", newPath, false).
		Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
		return
	}

	if err := h.storage.Rename(context.Background(), oldPath, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "rename")
		return
	}

	// One transaction with every row checked, for the same reason as MoveFile: the
	// rewrites were individually committed and unchecked, so a failure left the
	// disk rolled back and the descendant rows pointing at a path that was no
	// longer there.
	// Read before the transaction, for the same reason as MoveFile: a read on
	// h.db while the tx holds SQLite's write lock returns SQLITE_BUSY, Find
	// ignores it, and the subtree's rows silently keep their old paths.
	descendants := h.descendantsOf(file.UserID, oldPath)

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// The on-disk rename covered the subtree, so descendants only need DB
		// paths rewritten.
		for _, d := range descendants {
			if e := tx.Model(&model.File{}).
				Where("id = ?", d.ID).
				Update("path", newPath+strings.TrimPrefix(d.Path, oldPath)).Error; e != nil {
				return e
			}
		}

		prevName, prevPath := file.Name, file.Path
		file.Name = name
		file.Path = newPath
		file.UpdatedAt = time.Now()
		if e := tx.Save(file).Error; e != nil {
			file.Name, file.Path = prevName, prevPath
			return e
		}
		return nil
	}); err != nil {
		h.storage.Rename(context.Background(), newPath, oldPath) // roll back disk
		c.Error(err)                                             //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update file record"})
		return
	}

	logAudit(h.db, c, userID, "rename", "file", &file.ID, "Renamed to: "+name)

	c.JSON(http.StatusOK, toResponse(sc, file))
}

func (h *FileHandler) MoveFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		TargetParentID *uuid.UUID `json:"target_parent_id"`
		TargetPath     string     `json:"target_path"` // optional alternative to the id
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sc := reqScope(c)
	file, ok := h.writable(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	targetParentPath, targetParentID, ok := h.resolveDestination(c, userID, req.TargetParentID, req.TargetPath)
	if !ok {
		return
	}

	// Refuse to move a folder into itself or one of its own descendants.
	if file.IsDir && (targetParentPath == file.Path || strings.HasPrefix(targetParentPath, file.Path+"/")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot move a folder into itself"})
		return
	}

	newPath := cleanPath(path.Join(targetParentPath, file.Name))
	if newPath == "" || newPath == file.Path {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source and destination are the same"})
		return
	}

	// RenameFile and CreateFolder both check this; MoveFile did not, and
	// os.Rename replaces the destination atomically. So a file dropped into a
	// folder that already had something of that name destroyed it on disk while
	// leaving the victim's row behind — two live rows on one path, one of them
	// now describing bytes that no longer exist. The victim could then trash it
	// and take the mover's file with it.
	if h.rejectPathTaken(c, sc, newPath) || h.rejectPathOnDisk(c, newPath) {
		return
	}

	if err := h.storage.Mkdir(context.Background(), parentDir(newPath)); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "prepare destination")
		return
	}

	if err := h.storage.Rename(context.Background(), file.Path, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "move")
		return
	}

	oldPath := file.Path

	// The DB half is one transaction, and every row's error is checked.
	//
	// The descendant rewrites were separate committed writes whose errors were
	// thrown away, and the rollback moved the bytes back without undoing them. A
	// failure on the root Save therefore produced: bytes back at oldPath, root
	// row back at oldPath, and every descendant row still pointing at
	// newPath/… — a location that no longer existed. The whole subtree below the
	// root vanished from every listing and could not be selected to repair.
	// Read before the transaction. descendantsOf goes through h.db, which inside
	// a transaction is a different pooled connection — and SQLite serialises
	// writers, so the read returns SQLITE_BUSY, GORM swallows it, and the loop
	// iterates over nothing. Every descendant row keeps its old path while the
	// bytes moved, which is a subtree that cannot be listed, opened, moved or
	// deleted because none of those can find it any more.
	descendants := h.descendantsOf(file.UserID, oldPath)

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// A single on-disk rename relocated the whole subtree, so descendants only
		// need their DB paths rewritten.
		for _, d := range descendants {
			if e := tx.Model(&model.File{}).
				Where("id = ?", d.ID).
				Update("path", newPath+strings.TrimPrefix(d.Path, oldPath)).Error; e != nil {
				return e
			}
		}

		previous, parent := file.Path, file.ParentID
		file.Path = newPath
		file.ParentID = targetParentID
		file.UpdatedAt = time.Now()
		if e := tx.Save(file).Error; e != nil {
			file.Path, file.ParentID = previous, parent
			return e
		}
		return nil
	}); err != nil {
		h.storage.Rename(context.Background(), newPath, oldPath)
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update file record"})
		return
	}

	logAudit(h.db, c, userID, "move", "file", &file.ID, "Moved to: "+newPath)

	c.JSON(http.StatusOK, toResponse(sc, file))
}

// resolveDestination turns the request into a validated destination folder
// path and parent id. Accepts either target_parent_id or target_path.
// It writes the error response itself and reports ok=false on failure.
func (h *FileHandler) resolveDestination(c *gin.Context, userID uuid.UUID, parentID *uuid.UUID, parentPath string) (string, *uuid.UUID, bool) {
	sc := reqScope(c)

	// The destination must land inside the caller's home. An unrestricted user
	// keeps the storage root; a scoped one keeps their own folder.
	if parentID != nil {
		var target model.File
		if err := scoped(sc, h.db).First(&target, "id = ? AND is_dir = ? AND is_trashed = ?",
			*parentID, true, false).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target folder not found"})
			return "", nil, false
		}
		// is_trashed already excludes the trash itself, but not the chunk
		// scratch directory or any future internal directory. Those have no
		// rows at all, so this is belt-and-braces — the point is that one
		// check covers them rather than each needing its own clause.
		if reservedPath(target.Path) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot target a reserved location"})
			return "", nil, false
		}
		return target.Path, &target.ID, true
	}

	if parentPath != "" {
		cleaned := absOf(sc, parentPath)
		if cleaned == sc.Home {
			return sc.Home, nil, true // the home itself
		}
		if reservedPath(cleaned) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot target a reserved location"})
			return "", nil, false
		}
		var target model.File
		if err := scoped(sc, h.db).First(&target, "path = ? AND is_dir = ? AND is_trashed = ?",
			cleaned, true, false).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target folder not found"})
			return "", nil, false
		}
		return target.Path, &target.ID, true
	}

	// Neither supplied — the destination is the caller's root.
	return sc.Home, nil, true
}

func (h *FileHandler) CopyFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		TargetParentID *uuid.UUID `json:"target_parent_id"`
		TargetPath     string     `json:"target_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sc := reqScope(c)
	file, ok := h.lookupFile(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	targetParentPath, targetParentID, ok := h.resolveDestination(c, userID, req.TargetParentID, req.TargetPath)
	if !ok {
		return
	}

	if file.IsDir && (targetParentPath == file.Path || strings.HasPrefix(targetParentPath, file.Path+"/")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot copy a folder into itself"})
		return
	}

	// Copying is additive, so a sibling's file may be copied — the copy is
	// owned by the caller, not by whoever holds the original.
	newName, err := h.availableName(sc, targetParentPath, file.Name)
	if err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve destination name"})
		return
	}

	newPath := cleanPath(path.Join(targetParentPath, newName))

	// How much this copy will add, decided before any of it exists.
	//
	// The root's own bytes count: descendantsOf only matches rows strictly
	// below the root, so copying a plain file summed to zero and the charge was
	// skipped entirely. That made copy the one write path with no quota check
	// and no charge — a user could copy the same file a thousand times and
	// used_bytes never moved, so nothing downstream could see it either.
	subtreeBytes := int64(0)
	if !file.IsDir {
		subtreeBytes = file.Size
	}
	for _, d := range h.descendantsOf(file.UserID, file.Path) {
		if !d.IsDir {
			subtreeBytes += d.Size
		}
	}

	if rejectOverQuota(c, h.db, userID, subtreeBytes, inflightUploadBytes(h.db, userID)) {
		return
	}

	// Check the disk, not just the database.
	//
	// availableName only asks the files table, so it is blind to anything sitting
	// at the path with no row behind it — a leftover from a crashed copy, a file
	// dropped into the volume by hand, a partial upload. The copy would then be
	// written over it. Not a theoretical concern: the copy handler's own failure
	// paths used to leave exactly that state, since Copy truncated the destination
	// in place before the rows were inserted.
	if h.rejectPathOnDisk(c, newPath) {
		return
	}

	if err := h.storage.Mkdir(context.Background(), parentDir(newPath)); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "prepare destination")
		return
	}

	if err := h.storage.Copy(context.Background(), file.Path, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "copy")
		return
	}

	// Create the copied root record.
	newFile := &model.File{
		UserID:      userID,
		ParentID:    targetParentID,
		Name:        newName,
		Path:        newPath,
		Size:        file.Size,
		MimeType:    file.MimeType,
		IsDir:       file.IsDir,
		Checksum:    file.Checksum,
		StorageType: file.StorageType,
		StoragePath: newPath,
	}
	// The root's own bytes were counted above, before anything was written.
	// Without that the loop below never ran for a plain file, the total stayed
	// at zero, and the guard skipped the charge.
	addedBytes := subtreeBytes

	// Read the subtree before opening the transaction, not inside it.
	//
	// descendantsOf goes through h.db. Inside a transaction that is a
	// *different* pooled connection than the tx handle, and SQLite serialises
	// writers: the read comes back SQLITE_BUSY, GORM's Find swallows the error,
	// and the loop iterates over nothing. The copy then answers 201 with the
	// root row only — a folder on disk holding the copied bytes, with no rows
	// for any of its contents. They are invisible in every listing, charged to
	// nobody, and cannot be deleted or moved because there is no id for them.
	//
	// Reading first is also simply correct: the source rows are not modified by
	// the copy, so there is nothing to see fresh.
	descendants := h.descendantsOf(file.UserID, file.Path)

	// One transaction, and a failure aborts it. The bytes are already on disk by
	// this point, so a clone that failed to insert left copied files with no row:
	// invisible in every listing, charged to nobody, and unremovable through the
	// API. `continue` swallowed that and still answered 201.
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Create(newFile).Error; e != nil {
			return e
		}
		for _, d := range descendants {
			clone := d
			clone.ID = uuid.UUID{}
			clone.UserID = userID // the copy belongs to whoever made it
			clone.Path = newPath + strings.TrimPrefix(d.Path, file.Path)
			clone.StoragePath = clone.Path
			clone.OriginalPath = ""
			clone.IsTrashed = false
			clone.TrashedAt = nil
			// parent_id is re-derived from paths by reparentCopiedSubtree below.
			clone.ParentID = nil
			if e := tx.Create(&clone).Error; e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		h.storage.Remove(context.Background(), newPath)
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file records"})
		return
	}
	h.reparentCopiedSubtree(userID, newPath)

	if addedBytes > 0 {
		h.db.Model(&model.User{}).Where("id = ?", userID).
			UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", addedBytes))
	}

	logAudit(h.db, c, userID, "copy", "file", &newFile.ID, "Copied to: "+newPath)

	c.JSON(http.StatusCreated, toResponse(sc, newFile))
}

// reparentCopiedSubtree fixes parent_id across a freshly copied subtree by
// matching each cloned row's path to its parent path. Scoped to the copy's new
// root, since that is the only thing being rebuilt.
func (h *FileHandler) reparentCopiedSubtree(userID uuid.UUID, dstRoot string) {
	var clones []model.File
	whereSubtree(h.db.Where("user_id = ?", userID), "path", dstRoot).Find(&clones)

	byPath := make(map[string]uuid.UUID, len(clones)+1)
	var rootRow model.File
	if err := h.db.First(&rootRow, "user_id = ? AND path = ?", userID, dstRoot).Error; err == nil {
		byPath[dstRoot] = rootRow.ID
	}
	for i := range clones {
		byPath[clones[i].Path] = clones[i].ID
	}

	for i := range clones {
		cl := &clones[i]
		parentID, ok := byPath[parentDir(cl.Path)]
		if !ok {
			continue
		}
		h.db.Model(&model.File{}).Where("id = ?", cl.ID).Update("parent_id", parentID)
	}
}

// availableName returns name, or "name (2)" / "name (3)" ... when something with
// that name already exists in dir.
func (h *FileHandler) availableName(sc scope.Scope, dir, name string) (string, error) {
	exists := func(candidate string) (bool, error) {
		candidatePath := cleanPath(path.Join(dir, candidate))
		if candidatePath == "" {
			return false, nil
		}
		var count int64
		// Any row at this path counts: in a shared folder a sibling's file
		// must not be silently overwritten by a copy.
		err := scoped(sc, h.db.Model(&model.File{})).
			Where("path = ? AND is_trashed = ?", candidatePath, false).
			Count(&count).Error
		return count > 0, err
	}

	ok, err := exists(name)
	if err != nil || !ok {
		return name, err
	}

	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		ok, err := exists(candidate)
		if err != nil {
			return "", err
		}
		if !ok {
			return candidate, nil
		}
	}

	return name, nil
}

// Chunked upload endpoints
func (h *FileHandler) InitChunkedUpload(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var req struct {
		Filename    string     `json:"filename" binding:"required"`
		TotalSize   int64      `json:"total_size" binding:"required,gt=0"`
		ChunkSize   int64      `json:"chunk_size" binding:"required,gt=0"`
		TotalChunks int        `json:"total_chunks" binding:"required,gt=0"`
		Checksum    string     `json:"checksum"`
		ParentID    *uuid.UUID `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if max := h.maxFileSize(); max > 0 && req.TotalSize > max {
		// 413, not 400: the request is well-formed, the payload is simply
		// too big. 400 tells a client it sent something the server could not
		// parse, which is not true and sends the caller looking for a syntax
		// error that does not exist.
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
		return
	}
	if max := h.maxChunkSize(); max > 0 && req.ChunkSize > max {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk size too large"})
		return
	}

	// Reduce the name to a single path element before it is ever joined to the
	// parent path.
	filename, ok := safeFilename(req.Filename)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}
	req.Filename = filename

	// Determine target path. With no parent the file lands in the caller's home.
	sc := reqScope(c)
	parentPath := sc.Home
	if req.ParentID != nil {
		var parent model.File
		if err := scoped(sc, h.db).First(&parent, "id = ? AND is_dir = ?", *req.ParentID, true).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "parent folder not found"})
			return
		}
		parentPath = parent.Path
	}

	targetPath := filepath.Join(parentPath, req.Filename)
	targetPath = strings.TrimPrefix(targetPath, "/")

	// Refuse a name that is already taken, here rather than at the end. The
	// plain upload checks too, and without it a chunked upload spent the client's
	// bandwidth — potentially gigabytes — before finding out. More importantly,
	// AssembleChunks writes to the target path, so finding out at completion
	// means the file that was already there has been overwritten by then.
	if h.rejectPathTaken(c, sc, targetPath) || h.rejectPathOnDisk(c, targetPath) {
		return
	}

	// Checked before the session exists, so the declared size counts as
	// in-flight from this moment on. A user who opens twenty sessions of 9 GB
	// against a 10 GB quota gets through the first one and is refused the
	// second — no session-count limit needed to stop that.
	if req.TotalSize < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid total size"})
		return
	}
	if rejectOverQuota(c, h.db, userID, req.TotalSize, inflightUploadBytes(h.db, userID)) {
		return
	}

	// Create upload session. The TTL is a floor, not the real deadline: every chunk
	// pushes ExpiresAt forward (see WriteChunk), so what actually ends an upload
	// is going quiet for one cleanup interval, not this number. That is what lets
	// a slow 10 GB transfer run for hours without a hard-coded limit cutting it
	// off, while an abandoned one is still reclaimed within the interval.
	uploadID := uuid.New().String()
	ttl := h.storage.GetLimits().CleanupInterval()
	session := h.storage.CreateUploadSession(
		uploadID,
		req.Filename,
		targetPath,
		req.TotalSize,
		req.ChunkSize,
		req.TotalChunks,
		ttl,
	)

	// Save to DB
	uploadSession := &model.UploadSession{}
	uploadSession.ID = uuid.MustParse(uploadID)
	uploadSession.UserID = userID
	uploadSession.ParentID = req.ParentID
	uploadSession.Filename = req.Filename
	uploadSession.TargetPath = targetPath
	uploadSession.TotalSize = req.TotalSize
	uploadSession.ChunkSize = req.ChunkSize
	uploadSession.TotalChunks = req.TotalChunks
	uploadSession.UploadedChunks = 0
	uploadSession.Status = "uploading"
	uploadSession.TempDir = session.TempDir
	uploadSession.Checksum = req.Checksum
	// Take the deadline from the storage session rather than recomputing it: the
	// storage layer may have raised it to the sweep interval, and a DB row that
	// expires earlier than the session it describes is what the handler checks
	// before accepting each chunk.
	uploadSession.ExpiresAt = session.ExpiresAt

	if err := h.db.Create(uploadSession).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"upload_id":    uploadID,
		"chunk_size":   req.ChunkSize,
		"total_chunks": req.TotalChunks,
		"expires_at":   uploadSession.ExpiresAt,
	})
}

func (h *FileHandler) UploadChunk(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	uploadID := c.Param("uploadId")
	chunkIndexStr := c.Param("chunkIndex")

	chunkIndex := 0
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk index"})
		return
	}
	if chunkIndex < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid chunk index"})
		return
	}

	// Get upload session from DB
	var session model.UploadSession
	if err := h.db.First(&session, "id = ? AND user_id = ?", uploadID, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	if session.Status != "uploading" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload session not active"})
		return
	}

	if time.Now().After(session.ExpiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upload session expired"})
		return
	}

	// Get chunk from request
	fileHeader, err := c.FormFile("chunk")
	if err != nil {
		writeUploadReadError(c, err, "no chunk provided")
		return
	}

	if max := h.maxChunkSize(); max > 0 && fileHeader.Size > max {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk too large"})
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open chunk"})
		return
	}
	defer src.Close()

	// Write chunk to storage. It reports whether this index was new: a retry
	// overwrites the file but must not advance the counter, or the session would
	// claim to be complete with a different chunk still missing.
	isNew, err := h.storage.WriteChunk(context.Background(), uploadID, chunkIndex, src, fileHeader.Size)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if isNew {
		h.db.Model(&session).UpdateColumns(map[string]interface{}{
			"uploaded_chunks": gorm.Expr("uploaded_chunks + 1"),
			"updated_at":      time.Now(),
		})
	}

	// Push the DB deadline out to match the session's. The expiry check above runs
	// against this column, so leaving it at its original value would reject the
	// next chunk of an upload that storage has already decided is still live —
	// every transfer longer than one interval would die one interval in.
	if live, ok := h.storage.GetUploadSession(uploadID); ok {
		h.db.Model(&session).UpdateColumn("expires_at", live.ExpiresAt)
	}

	// Re-read to get the fresh count
	var fresh model.UploadSession
	h.db.Select("uploaded_chunks", "total_chunks").First(&fresh, "id = ?", uploadID)

	c.JSON(http.StatusOK, gin.H{
		"chunk_index":     chunkIndex,
		"uploaded_chunks": fresh.UploadedChunks,
		"total_chunks":    fresh.TotalChunks,
	})
}

func (h *FileHandler) CompleteChunkedUpload(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	uploadID := c.Param("uploadId")

	var session model.UploadSession
	if err := h.db.First(&session, "id = ? AND user_id = ?", uploadID, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	if session.UploadedChunks != session.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not all chunks uploaded"})
		return
	}

	// Checked again at the end, because the name can have been taken while this
	// upload was in flight — the check at init is a courtesy to the client, not
	// a lock. AssembleChunks renames its result over the target path, so by the
	// time a row can be created it is already too late: the bytes of whatever
	// was there have been replaced, and the failure path below would then remove
	// them too, leaving a row that downloads 404.
	if h.rejectPathTaken(c, reqScope(c), session.TargetPath) || h.rejectPathOnDisk(c, session.TargetPath) {
		return
	}

	// Assemble chunks. The returned length is what was measured on disk, and it
	// is what gets stored and charged — session.TotalSize is the client's claim,
	// and using it here would let a session that declared a byte write a
	// terabyte while used_bytes recorded one.
	assembledSize, err := h.storage.AssembleChunks(context.Background(), uploadID, session.TargetPath, session.TotalChunks, session.Checksum)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Create file record
	file := &model.File{
		UserID:      userID,
		ParentID:    session.ParentID,
		Name:        session.Filename,
		Path:        session.TargetPath,
		Size:        assembledSize,
		MimeType:    mime.TypeByExtension(filepath.Ext(session.Filename)),
		IsDir:       false,
		Checksum:    session.Checksum,
		StorageType: "local",
		StoragePath: session.TargetPath,
	}
	if file.MimeType == "" {
		file.MimeType = "application/octet-stream"
	}

	if err := h.db.Create(file).Error; err != nil {
		h.storage.Remove(context.Background(), session.TargetPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file record"})
		return
	}

	// Update user storage usage
	h.db.Model(&model.User{}).Where("id = ?", userID).UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", assembledSize))

	// Mark session complete
	h.db.Model(&session).Updates(map[string]interface{}{
		"status":     "completed",
		"file_id":    file.ID,
		"updated_at": time.Now(),
	})

	// Cleanup temp chunks and release the in-memory session — same reason as the
	// cancel path: a finished upload has nothing left for the sweeper to reclaim.
	h.storage.CleanupChunks(context.Background(), uploadID)
	h.storage.DeleteUploadSession(uploadID)

	logAudit(h.db, c, userID, "upload_chunked", "file", &file.ID, fmt.Sprintf("Uploaded (chunked): %s", session.Filename))

	c.JSON(http.StatusCreated, model.ToFileResponse(file))
}

func (h *FileHandler) GetUploadStatus(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	uploadID := c.Param("uploadId")

	var session model.UploadSession
	if err := h.db.First(&session, "id = ? AND user_id = ?", uploadID, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	progress := 0.0
	if session.TotalChunks > 0 {
		progress = float64(session.UploadedChunks) / float64(session.TotalChunks) * 100
	}

	c.JSON(http.StatusOK, gin.H{
		"id":              session.ID,
		"filename":        session.Filename,
		"target_path":     session.TargetPath,
		"total_size":      session.TotalSize,
		"chunk_size":      session.ChunkSize,
		"total_chunks":    session.TotalChunks,
		"uploaded_chunks": session.UploadedChunks,
		"status":          session.Status,
		"progress":        progress,
		"created_at":      session.CreatedAt,
		"expires_at":      session.ExpiresAt,
	})
}

func (h *FileHandler) CancelUpload(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	uploadID := c.Param("uploadId")

	var session model.UploadSession
	if err := h.db.First(&session, "id = ? AND user_id = ?", uploadID, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	// Cleanup storage
	h.storage.CleanupChunks(context.Background(), uploadID)

	// Drop the in-memory session too. Without this the entry lingers until its
	// TTL, and every abandoned upload leaves a record behind for the sweeper to
	// find hours later.
	h.storage.DeleteUploadSession(uploadID)

	// Update DB
	h.db.Model(&session).Updates(map[string]interface{}{
		"status":     "cancelled",
		"updated_at": time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{"message": "upload cancelled"})
}

// writeUploadReadError answers a failed multipart read.
//
// The two cases are genuinely different and were conflated: the body hitting
// its limit is the client's file being too large, which is 413 and worth
// saying so, while a malformed body is a 400. Reporting the first as the
// second told a caller uploading a legitimate 40GB file over a 10GB ceiling
// that they had "not provided a file", with nothing to act on.
func writeUploadReadError(c *gin.Context, err error, fallback string) {
	if middleware.BodyTooLarge(err) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds the maximum upload size"})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": fallback})
}
