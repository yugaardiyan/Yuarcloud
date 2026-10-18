package handler

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

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
