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

// Chunked upload: session init, chunk writes, assembly, status and cancel.
//
// Separate from the plain upload because the lifecycle differs. A plain upload
// is one request that finishes or fails. This one spans many, can be paused and
// resumed, can be abandoned and cleaned up later, and carries state that has to
// survive between them — which is why the session is re-read under its lock
// rather than trusted from whatever the caller happened to hold.

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

	// The declared chunk count is the only thing that bounds how many temp
	// files a session may create, and it was never compared with the size it
	// claims to describe: total_size: 1 with total_chunks: 1000 opened a
	// thousand max-sized chunks while every quota check saw one byte. The
	// count has to be exactly what the file needs.
	if req.TotalSize <= 0 || req.ChunkSize <= 0 || req.TotalChunks <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid upload size"})
		return
	}
	if want := (req.TotalSize + req.ChunkSize - 1) / req.ChunkSize; int64(req.TotalChunks) != want {
		c.JSON(http.StatusBadRequest, gin.H{"error": "total_chunks does not match total_size and chunk_size"})
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

	// A chunked upload briefly needs room for two copies: the chunk files, and
	// the assembled file they are copied into. The final file is only renamed
	// into place after the chunks have all been read, so it is not possible to
	// reclaim them early without losing the ability to retry a failed assembly.
	// Check before a single byte is sent — discovering this at the end wastes
	// hours of the client's uplink and leaves the chunks behind.
	//
	// A failure to stat the volume is not treated as "no space": that would
	// refuse every upload on a platform where GetDiskUsage is unavailable, so
	// the check only acts on a positive, successful reading.
	required := req.TotalSize
	if req.TotalSize < (1 << 62) {
		required = req.TotalSize * 2
	}
	if _, free, err := h.storage.GetDiskUsage(context.Background(), ""); err == nil && required > free {
		c.JSON(http.StatusInsufficientStorage, gin.H{
			"error":    fmt.Sprintf("not enough free disk space: this upload needs %d bytes free for chunks and assembly", required),
			"disk":     true,
			"free":     free,
			"required": required,
		})
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
		// The in-memory session was created first; without this it lingers
		// until the sweeper, holding its temp directory for no reason.
		h.storage.DeleteUploadSession(uploadID)
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

	// A chunk is small by contract, but the link carrying it can still be slow
	// enough to outlive a 30s deadline — especially the first one, before the
	// connection has warmed up.
	liftTransferDeadlines(c)

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

	// Every chunk except the last has exactly the declared chunk_size. Nothing
	// enforced that before: a session declaring a 1 KB file could send
	// max_chunk_size worth of bytes per chunk, filling the volume while the
	// quota check saw only 1 KB. Assembly rejects the size mismatch at the end,
	// but the disk is occupied until cleanup.
	offset := int64(chunkIndex) * session.ChunkSize
	if offset >= session.TotalSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "chunk index is out of range"})
		return
	}
	expected := session.ChunkSize
	if remaining := session.TotalSize - offset; remaining < expected {
		expected = remaining
	}
	if fileHeader.Size != expected {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":    fmt.Sprintf("chunk %d must be %d bytes", chunkIndex, expected),
			"expected": expected,
		})
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

// chunkedAsyncAssemblyThreshold is where completion stops waiting for the
// assembled bytes before answering.
//
// Assembly has to read every chunk, write the final file and hash it. At
// spinning-disk speeds a 50 GB file is minutes of work, and Cloudflare closes a
// proxied request at roughly 100–125 seconds no matter what the origin's own
// timeouts say. A synchronous complete therefore turned a successful upload
// into a 524 at the very last step. Up to this size it is still fast enough to
// answer in the request; above it, the work moves to a goroutine and the client
// polls /status. Tests lower this to exercise the async path with tiny files.
var chunkedAsyncAssemblyThreshold int64 = 2 * 1024 * 1024 * 1024 // 2 GiB

func (h *FileHandler) CompleteChunkedUpload(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	uploadID := c.Param("uploadId")

	// Assembly rewrites every chunk into the final file and re-reads it for the
	// checksum. On a multi-gigabyte upload that outlives the response deadline
	// even though the client is only waiting for a small JSON body.
	liftTransferDeadlines(c)

	var session model.UploadSession
	if err := h.db.First(&session, "id = ? AND user_id = ?", uploadID, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	if session.UploadedChunks != session.TotalChunks {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not all chunks uploaded"})
		return
	}

	// Exactly one request may assemble a session.
	//
	// Two completes used to run the whole path: both passed the name checks,
	// both assembled to the same destination, both inserted, and the loser's
	// insert failed on the unique path index - after which its cleanup removed
	// the destination, deleting the file the winner had just registered. The
	// conditional status update is the claim: the loser matches no row here and
	// stops before it can touch the bytes.
	claim := h.db.Model(&model.UploadSession{}).
		Where("id = ? AND user_id = ? AND status = ?", uploadID, userID, "uploading").
		Update("status", "completing")
	if claim.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalise upload"})
		return
	}
	if claim.RowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "this upload is already being finalised"})
		return
	}
	// Every early return below puts the session back so the client can retry.
	release := func() {
		h.db.Model(&model.UploadSession{}).Where("id = ?", uploadID).
			Update("status", "uploading")
	}

	// Checked again at the end, because the name can have been taken while this
	// upload was in flight — the check at init is a courtesy to the client, not
	// a lock. AssembleChunks renames its result over the target path, so by the
	// time a row can be created it is already too late: the bytes of whatever
	// was there have been replaced, and the failure path below would then remove
	// them too, leaving a row that downloads 404.
	if h.rejectPathTaken(c, reqScope(c), session.TargetPath) || h.rejectPathOnDisk(c, session.TargetPath) {
		release()
		return
	}

	// Large uploads finish out of band. The goroutine gets copies of what it
	// needs from the request, because *gin.Context is not valid once this
	// handler returns.
	if session.TotalSize >= chunkedAsyncAssemblyThreshold {
		ip := c.ClientIP()
		userAgent := c.GetHeader("User-Agent")
		go h.finishChunkedUploadAsync(&session, userID, ip, userAgent)

		c.JSON(http.StatusAccepted, gin.H{
			"upload_id": uploadID,
			"status":    "completing",
			"message":   "upload received; finalising on the server",
		})
		return
	}

	file, err := h.finishChunkedUpload(&session)
	if err != nil {
		// Nothing was registered, so the chunks are still the client's to
		// retry with — put the session back where it was.
		release()
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	logAudit(h.db, c, userID, "upload_chunked", "file", &file.ID, fmt.Sprintf("Uploaded (chunked): %s", session.Filename))
	c.JSON(http.StatusCreated, model.ToFileResponse(file))
}

// finishChunkedUpload turns a fully-received session into a file row.
//
// Shared by the synchronous and background completion paths so the two cannot
// drift: the order of assemble, register, charge and clean up is the part that
// has to stay identical. It reports failures to the caller and never changes
// the session status on them; the sync path releases the claim for a retry, the
// async path marks it failed because there is no request left to retry from.
func (h *FileHandler) finishChunkedUpload(session *model.UploadSession) (*model.File, error) {
	uploadID := session.ID.String()

	// The returned length is what was measured on disk, and it is what gets
	// stored and charged — session.TotalSize is the client's claim, and using it
	// here would let a session that declared a byte write a terabyte while
	// used_bytes recorded one.
	assembledSize, err := h.storage.AssembleChunks(context.Background(), uploadID, session.TargetPath, session.TotalChunks, session.Checksum)
	if err != nil {
		return nil, err
	}

	file := &model.File{
		UserID:      session.UserID,
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
		// Only remove bytes with no row behind them: a concurrent winner may
		// have registered this path, and its file must survive our failure.
		var rows int64
		h.db.Model(&model.File{}).Where("path = ?", session.TargetPath).Count(&rows)
		if rows == 0 {
			h.storage.Remove(context.Background(), session.TargetPath)
		}
		return nil, fmt.Errorf("failed to create file record")
	}

	h.db.Model(&model.User{}).
		Where("id = ?", session.UserID).
		UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", assembledSize))

	h.db.Model(session).Updates(map[string]interface{}{
		"status":        "completed",
		"file_id":       file.ID,
		"error_message": "",
		"updated_at":    time.Now(),
	})

	// Cleanup temp chunks and release the in-memory session — same reason as the
	// cancel path: a finished upload has nothing left for the sweeper to reclaim.
	h.storage.CleanupChunks(context.Background(), uploadID)
	h.storage.DeleteUploadSession(uploadID)

	return file, nil
}

func (h *FileHandler) finishChunkedUploadAsync(session *model.UploadSession, userID uuid.UUID, ip, userAgent string) {
	uploadID := session.ID.String()

	file, err := h.finishChunkedUpload(session)
	if err != nil {
		h.db.Model(&model.UploadSession{}).Where("id = ?", uploadID).Updates(map[string]interface{}{
			"status":        "failed",
			"error_message": err.Error(),
			"updated_at":    time.Now(),
		})
		// A failed assembly cannot be retried through this session: the status
		// is no longer "uploading", and the chunks have already been read once.
		// Leaving up to 50 GB in the temp directory for a session nothing can
		// reach is worse than asking the client to send it again.
		h.storage.CleanupChunks(context.Background(), uploadID)
		h.storage.DeleteUploadSession(uploadID)
		return
	}

	logAuditClient(h.db, userID, "upload_chunked", "file", &file.ID,
		fmt.Sprintf("Uploaded (chunked): %s", session.Filename), ip, userAgent)
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

	resp := gin.H{
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
	}
	if session.ErrorMessage != "" {
		resp["error"] = session.ErrorMessage
	}
	// The completion response for a background assembly is only "accepted", so
	// the file record has to come from here once the work is done. Without it a
	// polling client knows the upload succeeded but not what it created, and
	// cannot refresh the folder or open the file.
	if session.Status == "completed" && session.FileID != nil {
		var file model.File
		if err := scoped(reqScope(c), h.db).
			First(&file, "id = ? AND user_id = ?", *session.FileID, userID).Error; err == nil {
			resp["file"] = toResponse(reqScope(c), &file)
		}
	}

	c.JSON(http.StatusOK, resp)
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
