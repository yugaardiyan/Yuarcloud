package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// tus 1.0 (core + creation + termination).
//
// The chunked uploader works, but it keeps its session in memory and its state
// in a custom protocol: a refreshed tab, a restarted server or a dropped
// connection all mean starting a multi-gigabyte file again. tus exists to make
// that resumable, and it is what FileBrowser speaks, so a browser that works
// there can continue here.
//
// The two upload paths share the same limits and the same destination rules on
// purpose. A tus upload is charged to the user's quota at creation, obeys
// max_file_size, and lands in the same folder tree as any other write; only the
// transport differs.

const (
	tusVersion = "1.0.0"
	// The metadata header is base64 key/value pairs. A filename and a parent id
	// are a few hundred bytes; 16 KiB is generous and still bounded.
	tusMetadataLimit = 16 << 10
	// A completed upload's row is kept so the client can read back the file
	// record after the last PATCH. The file itself is not touched by this.
	tusStatusRetention = 24 * time.Hour
)

// TusOptions advertises the protocol. It is deliberately reachable without
// authentication: a tus client asks before it creates an upload, and a 401 to
// OPTIONS tells it nothing about what the server supports.
func (h *FileHandler) TusOptions(c *gin.Context) {
	c.Header("Tus-Resumable", tusVersion)
	c.Header("Tus-Version", tusVersion)
	c.Header("Tus-Extension", "creation,termination")
	if max := h.maxFileSize(); max > 0 {
		c.Header("Tus-Max-Size", strconv.FormatInt(max, 10))
	}
	c.Status(http.StatusNoContent)
}

// TusCreate starts a resumable upload.
//
// The upload is identified by a server-generated UUID and persisted in
// tus_uploads before the response, so a restart between create and the first
// PATCH leaves a session HEAD can still describe.
func (h *FileHandler) TusCreate(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	if v := strings.TrimSpace(c.GetHeader("Tus-Resumable")); v != "" && v != tusVersion {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "unsupported Tus-Resumable version"})
		return
	}

	// Upload-Defer-Length is not advertised, so a creation without a length is
	// not something this server can store. Say so rather than waiting forever.
	lengthHeader := strings.TrimSpace(c.GetHeader("Upload-Length"))
	if lengthHeader == "" {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Upload-Length is required"})
		return
	}
	totalSize, err := strconv.ParseInt(lengthHeader, 10, 64)
	if err != nil || totalSize <= 0 {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Upload-Length"})
		return
	}
	if max := h.maxFileSize(); max > 0 && totalSize > max {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
		return
	}

	rawMetadata := c.GetHeader("Upload-Metadata")
	if len(rawMetadata) > tusMetadataLimit {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Upload-Metadata is too large"})
		return
	}
	metadata, err := parseTusMetadata(rawMetadata)
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	filename, ok := safeFilename(metadata["filename"])
	if !ok {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}

	var parentID *uuid.UUID
	if raw := strings.TrimSpace(metadata["parent_id"]); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			c.Header("Tus-Resumable", tusVersion)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid parent_id"})
			return
		}
		parentID = &parsed
	}

	sc := reqScope(c)
	parentPath := sc.Home
	if parentID != nil {
		var parent model.File
		if err := scoped(sc, h.db).First(&parent, "id = ? AND is_dir = ?", *parentID, true).Error; err != nil {
			c.Header("Tus-Resumable", tusVersion)
			c.JSON(http.StatusBadRequest, gin.H{"error": "parent folder not found"})
			return
		}
		parentPath = parent.Path
	}

	targetPath := strings.TrimPrefix(filepath.Join(parentPath, filename), "/")

	// Refuse an occupied name before a byte is sent. The check is repeated at
	// completion because a name can be taken while the upload is running.
	if h.rejectPathTaken(c, sc, targetPath) || h.rejectPathOnDisk(c, targetPath) {
		c.Header("Tus-Resumable", tusVersion)
		return
	}

	if rejectOverQuota(c, h.db, userID, totalSize, inflightUploadBytes(h.db, userID)) {
		c.Header("Tus-Resumable", tusVersion)
		return
	}

	// A tus upload grows one data file in place and is then renamed, so unlike
	// the chunked path it needs one copy of the size, not two.
	if _, free, err := h.storage.GetDiskUsage(context.Background(), ""); err == nil && totalSize > free {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusInsufficientStorage, gin.H{
			"error":    fmt.Sprintf("not enough free disk space: this upload needs %d bytes free", totalSize),
			"disk":     true,
			"free":     free,
			"required": totalSize,
		})
		return
	}

	checksum := strings.ToLower(strings.TrimSpace(metadata["checksum"]))

	upload := &model.TusUpload{
		UserID:     userID,
		ParentID:   parentID,
		Filename:   filename,
		TargetPath: targetPath,
		TotalSize:  totalSize,
		Checksum:   checksum,
		Status:     "uploading",
		ExpiresAt:  time.Now().Add(h.tusTTL()),
	}
	if err := h.db.Create(upload).Error; err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upload session"})
		return
	}

	c.Header("Tus-Resumable", tusVersion)
	c.Header("Location", "/api/files/tus/"+upload.ID.String())
	c.Header("Upload-Offset", "0")
	c.Status(http.StatusCreated)
}

// TusHead answers how much of an upload the server already has. This is the
// request that makes resume possible after a refresh, a dropped connection or a
// server restart.
func (h *FileHandler) TusHead(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	upload, ok := h.loadTusUpload(c, userID)
	if !ok {
		return
	}

	offset, err := h.tusOffset(upload)
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upload state"})
		return
	}

	c.Header("Tus-Resumable", tusVersion)
	c.Header("Upload-Offset", strconv.FormatInt(offset, 10))
	c.Header("Upload-Length", strconv.FormatInt(upload.TotalSize, 10))
	c.Header("Cache-Control", "no-store")
	// Not part of tus. Our client uses it to tell "resume this" apart from
	// "this id has already produced its file, start a new upload" — without it,
	// re-selecting the same file after a completed upload would return the old
	// result instead of uploading again.
	c.Header("X-Yuarcloud-Status", upload.Status)
	c.Status(http.StatusOK)
}

// TusPatch appends bytes at the offset the client claims, then finalises the
// upload when the last byte has arrived.
func (h *FileHandler) TusPatch(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	liftTransferDeadlines(c)

	upload, ok := h.loadTusUpload(c, userID)
	if !ok {
		return
	}

	unlock := h.tusLock(upload.ID.String())
	defer unlock()

	// Re-read under the lock. Another PATCH may have advanced the offset between
	// loadTusUpload and here, and acting on the stale copy would append at a
	// position the file no longer has.
	if err := h.db.First(upload, "id = ? AND user_id = ?", upload.ID, userID).Error; err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}

	switch upload.Status {
	case "completed":
		// Idempotent: a retried final PATCH should not look like a failure.
		c.Header("Tus-Resumable", tusVersion)
		c.Header("Upload-Offset", strconv.FormatInt(upload.TotalSize, 10))
		c.Status(http.StatusNoContent)
		return
	case "finalizing":
		c.Header("Tus-Resumable", tusVersion)
		c.Header("Upload-Offset", strconv.FormatInt(upload.TotalSize, 10))
		c.Status(http.StatusNoContent)
		return
	case "failed":
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusGone, gin.H{"error": upload.ErrorMessage})
		return
	}

	if time.Now().After(upload.ExpiresAt) {
		h.storage.TusRemove(upload.ID.String())
		h.markTusFailed(upload, "upload session expired")
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusGone, gin.H{"error": "upload session expired"})
		return
	}

	if ct := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type"))); !strings.HasPrefix(ct, "application/offset+octet-stream") {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/offset+octet-stream"})
		return
	}

	current, err := h.storage.TusSize(upload.ID.String())
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upload state"})
		return
	}
	if current > upload.TotalSize {
		current = upload.TotalSize
	}

	clientOffset, err := strconv.ParseInt(strings.TrimSpace(c.GetHeader("Upload-Offset")), 10, 64)
	if err != nil || clientOffset < 0 {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Upload-Offset"})
		return
	}
	if clientOffset != current {
		// The protocol's answer to a stale offset: tell the client where the
		// server actually is, and let it retry from there.
		c.Header("Tus-Resumable", tusVersion)
		c.Header("Upload-Offset", strconv.FormatInt(current, 10))
		c.JSON(http.StatusConflict, gin.H{"error": "Upload-Offset does not match"})
		return
	}

	// Already complete on disk (a previous PATCH won the race, or a restart
	// landed between the append and the response). Finish it rather than
	// accepting a zero-length append.
	if current == upload.TotalSize {
		h.completeTus(c, upload, userID)
		return
	}

	newOffset, err := h.storage.TusAppend(c.Request.Context(), upload.ID.String(), current, upload.TotalSize, c.Request.Body)
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		switch {
		case errors.Is(err, storage.ErrTusOffsetMismatch):
			c.Header("Upload-Offset", strconv.FormatInt(newOffset, 10))
			c.JSON(http.StatusConflict, gin.H{"error": "Upload-Offset does not match"})
		case errors.Is(err, storage.ErrTusTooLarge):
			c.Header("Upload-Offset", strconv.FormatInt(newOffset, 10))
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "upload exceeds the declared length"})
		case middleware.BodyTooLarge(err):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "chunk exceeds the maximum upload size"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write upload data"})
		}
		return
	}

	if err := h.db.Model(&model.TusUpload{}).Where("id = ?", upload.ID).Updates(map[string]interface{}{
		"offset":     newOffset,
		"expires_at": time.Now().Add(h.tusTTL()),
		"updated_at": time.Now(),
	}).Error; err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record upload progress"})
		return
	}
	upload.Offset = newOffset

	c.Header("Upload-Offset", strconv.FormatInt(newOffset, 10))
	c.Header("Tus-Resumable", tusVersion)

	if newOffset < upload.TotalSize {
		c.Status(http.StatusNoContent)
		return
	}

	// The bytes are all here. Large files finalise out of band for the same
	// reason the chunked path does: a 50 GB rename is instant, but a checksum
	// pass or a cross-volume copy is not, and the response should not wait on
	// either past the proxy's deadline.
	if upload.TotalSize >= chunkedAsyncAssemblyThreshold {
		if h.claimTus(upload) {
			snapshot := *upload
			ip := c.ClientIP()
			userAgent := c.GetHeader("User-Agent")
			go h.finalizeTusAsync(snapshot, ip, userAgent)
		}
		c.Status(http.StatusNoContent)
		return
	}

	h.completeTus(c, upload, userID)
}

// TusStatus is an extension, not part of tus: after the last PATCH the client
// needs the created file record, and tus' 204 carries no body. The chunked
// uploader already polls a status endpoint for the same reason.
func (h *FileHandler) TusStatus(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	upload, ok := h.loadTusUpload(c, userID)
	if !ok {
		return
	}

	// An upload can reach its declared length without becoming a file: a crash
	// during finalisation, or a client that reconnects to a session the startup
	// reconciliation reset to uploading. This endpoint is what the client polls
	// at that point, so it is the place to finish the job rather than leaving
	// the row in a state no request would ever move past.
	//
	// The lock is the same one PATCH and DELETE hold. Recovery used to claim and
	// finalise without it, so a cancel that had already re-read the row could
	// delete it while this path renamed the bytes and charged the quota —
	// leaving a file no row describes.
	unlock := h.tusLock(upload.ID.String())
	if err := h.db.First(upload, "id = ? AND user_id = ?", upload.ID, userID).Error; err != nil {
		unlock()
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	completed := h.completeStuckTus(upload)
	unlock()
	if completed {
		if err := h.db.First(upload, "id = ?", upload.ID).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upload state"})
			return
		}
	}

	resp := gin.H{
		"id":         upload.ID,
		"filename":   upload.Filename,
		"status":     upload.Status,
		"offset":     upload.Offset,
		"total_size": upload.TotalSize,
		"progress":   tusProgress(upload.Offset, upload.TotalSize),
		"created_at": upload.CreatedAt,
		"expires_at": upload.ExpiresAt,
	}
	if upload.ErrorMessage != "" {
		resp["error"] = upload.ErrorMessage
	}
	if upload.Status == "completed" && upload.FileID != nil {
		var file model.File
		if err := scoped(reqScope(c), h.db).
			First(&file, "id = ? AND user_id = ?", *upload.FileID, userID).Error; err == nil {
			resp["file"] = toResponse(reqScope(c), &file)
		}
	}
	c.JSON(http.StatusOK, resp)
}

// TusDelete terminates an upload. A completed upload has no partial data left;
// removing its bookkeeping row is still correct and makes the id unusable.
func (h *FileHandler) TusDelete(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	upload, ok := h.loadTusUpload(c, userID)
	if !ok {
		return
	}

	unlock := h.tusLock(upload.ID.String())
	defer unlock()

	// Re-read under the lock: a final PATCH may have claimed the upload between
	// loadTusUpload and here. Deleting the row during finalization used to
	// answer 204 while the goroutine went on to create the file and charge the
	// quota, so "cancelled" left both the bytes and the charge behind.
	if err := h.db.First(upload, "id = ? AND user_id = ?", upload.ID, userID).Error; err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return
	}
	if upload.Status == "finalizing" {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusConflict, gin.H{"error": "upload is being finalised; try again shortly"})
		return
	}

	_ = h.storage.TusRemove(upload.ID.String())
	h.db.Delete(&model.TusUpload{}, "id = ?", upload.ID)

	c.Header("Tus-Resumable", tusVersion)
	c.Status(http.StatusNoContent)
}

// StartTusSweeper reclaims abandoned tus uploads on the same interval the
// chunked sessions use. The stored cleanup setting can change while the process
// runs, so the interval is read after every tick rather than baked into a
// ticker at construction.
func (h *FileHandler) StartTusSweeper() {
	go func() {
		for {
			interval := h.storage.GetLimits().CleanupInterval()
			if interval < 30*time.Second {
				interval = 30 * time.Second
			}
			timer := time.NewTimer(interval)
			<-timer.C
			h.sweepExpiredTus()
		}
	}()
}

// ReconcileTus deletes partial data files that no database row refers to, and
// recovers rows a crash left mid-finalisation.
//
// A crash between claimTus (uploading → finalizing) and the completed update
// leaves a row no goroutine owns: PATCH refuses to touch it and completion can
// never be claimed. Resetting it to uploading puts it back under the same path
// a live upload takes, and TusStatus finishes it on the next poll. Anything
// that stays abandoned is reclaimed by the expiry sweep like any other upload.
func (h *FileHandler) ReconcileTus() {
	h.db.Model(&model.TusUpload{}).
		Where("status = ?", "finalizing").
		Update("status", "uploading")

	var ids []uuid.UUID
	if err := h.db.Model(&model.TusUpload{}).Pluck("id", &ids).Error; err != nil {
		return
	}
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		keep[id.String()] = struct{}{}
	}
	h.storage.SweepTusOrphans(keep)
}

func (h *FileHandler) sweepExpiredTus() {
	now := time.Now()

	var expired []model.TusUpload
	h.db.Where("status IN ? AND expires_at < ?", []string{"uploading", "failed"}, now).Find(&expired)
	for _, candidate := range expired {
		// The list above was read before this loop, and PATCH extends
		// ExpiresAt only after its append finishes. Sweeping on the stale copy
		// deleted a session that had just accepted data: the PATCH answered 204
		// with an advanced offset, then the next HEAD was 404. Taking the same
		// per-upload lock PATCH holds, and re-reading under it, turns that into
		// a no-op when the upload is still alive.
		unlock := h.tusLock(candidate.ID.String())
		var fresh model.TusUpload
		if err := h.db.First(&fresh, "id = ?", candidate.ID).Error; err != nil {
			unlock()
			continue
		}
		if !fresh.ExpiresAt.Before(now) || (fresh.Status != "uploading" && fresh.Status != "failed") {
			unlock()
			continue
		}
		_ = h.storage.TusRemove(fresh.ID.String())
		h.db.Delete(&model.TusUpload{}, "id = ?", fresh.ID)
		unlock()
	}

	// Completed rows are a short-lived lookup for the client. The file they
	// created lives in the files table and is not affected.
	h.db.Where("status = ? AND updated_at < ?", "completed", now.Add(-tusStatusRetention)).
		Delete(&model.TusUpload{})
}

// completeTus claims and finalises an upload inline.
func (h *FileHandler) completeTus(c *gin.Context, upload *model.TusUpload, userID uuid.UUID) {
	if !h.claimTus(upload) {
		// Someone else is already finalising it. The client polls status.
		c.Header("Tus-Resumable", tusVersion)
		c.Header("Upload-Offset", strconv.FormatInt(upload.TotalSize, 10))
		c.Status(http.StatusNoContent)
		return
	}

	file, err := h.finalizeTusClaimed(upload)
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(tusErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	logAudit(h.db, c, userID, "upload_tus", "file", &file.ID,
		fmt.Sprintf("Uploaded (tus): %s", upload.Filename))

	c.Header("Tus-Resumable", tusVersion)
	c.Header("Upload-Offset", strconv.FormatInt(upload.TotalSize, 10))
	c.Status(http.StatusNoContent)
}

// completeStuckTus finalises an upload whose bytes are all present but whose
// row never reached "completed". It reports whether it did.
//
// The offset guard keeps this off the hot path of a normal upload: while bytes
// are still arriving the row is below its total, and only the last PATCH — which
// normally finalises inline — ever sets it equal. A stat then confirms the file
// really is complete before claiming, because the row alone could be ahead of
// the disk after a failed append.
func (h *FileHandler) completeStuckTus(upload *model.TusUpload) bool {
	if upload.Status != "uploading" || upload.Offset < upload.TotalSize {
		return false
	}
	size, err := h.storage.TusSize(upload.ID.String())
	if err != nil || size < upload.TotalSize {
		return false
	}
	if !h.claimTus(upload) {
		// Someone else is finalising; the next poll will see the result.
		return false
	}
	if _, err := h.finalizeTusClaimed(upload); err != nil {
		return false
	}
	return true
}

// claimTus moves uploading → finalizing exactly once.
//
// Two PATCHes can both see offset == length (a retried final chunk, or two
// workers racing on a resumed upload). The conditional update is the lock: the
// loser matches no row and must not rename the same data file a second time.
func (h *FileHandler) claimTus(upload *model.TusUpload) bool {
	res := h.db.Model(&model.TusUpload{}).
		Where("id = ? AND status = ?", upload.ID, "uploading").
		Updates(map[string]interface{}{"status": "finalizing", "updated_at": time.Now()})
	return res.Error == nil && res.RowsAffected == 1
}

// finalizeTusClaimed assumes the caller holds the uploading → finalizing claim.
func (h *FileHandler) finalizeTusClaimed(upload *model.TusUpload) (*model.File, error) {
	// Two sessions can be created for the same filename before either finishes;
	// each has its own id, so claimTus does not serialize them. The destination
	// check and the rename were therefore a race: both saw "free", both renamed,
	// and the last one silently replaced the first one's bytes while both rows
	// claimed to describe their own upload. Serializing on the target path makes
	// the loser see the winner's file and fail honestly.
	unlock := h.tusLock("path:" + upload.TargetPath)
	defer unlock()

	fail := func(err error) (*model.File, error) {
		h.markTusFailed(upload, err.Error())
		_ = h.storage.TusRemove(upload.ID.String())
		return nil, err
	}

	// Re-check the destination. The check at creation was a courtesy, not a
	// lock: another upload can take the name while this one is in flight.
	if h.pathTakenAnywhere(upload.TargetPath) {
		return fail(fmt.Errorf("%w: an item with that name already exists here", storage.ErrTusTargetExists))
	}
	if exists, err := h.storage.Exists(context.Background(), upload.TargetPath); err == nil && exists {
		return fail(fmt.Errorf("%w: something already exists at that location", storage.ErrTusTargetExists))
	}

	size, err := h.storage.TusFinalize(context.Background(), upload.ID.String(), upload.TargetPath, upload.Checksum)
	if err != nil {
		return fail(err)
	}

	file := &model.File{
		UserID:      upload.UserID,
		ParentID:    upload.ParentID,
		Name:        upload.Filename,
		Path:        upload.TargetPath,
		Size:        size,
		MimeType:    mime.TypeByExtension(filepath.Ext(upload.Filename)),
		IsDir:       false,
		Checksum:    upload.Checksum,
		StorageType: "local",
		StoragePath: upload.TargetPath,
	}
	if file.MimeType == "" {
		file.MimeType = "application/octet-stream"
	}

	if err := h.db.Create(file).Error; err != nil {
		// Only remove bytes with no row behind them: a concurrent winner may
		// have registered this path, and its file must survive our failure.
		var rows int64
		h.db.Model(&model.File{}).Where("path = ?", upload.TargetPath).Count(&rows)
		if rows == 0 {
			h.storage.Remove(context.Background(), upload.TargetPath)
		}
		return fail(fmt.Errorf("failed to create file record"))
	}

	h.db.Model(&model.User{}).
		Where("id = ?", upload.UserID).
		UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", size))

	h.db.Model(&model.TusUpload{}).Where("id = ?", upload.ID).Updates(map[string]interface{}{
		"status":        "completed",
		"file_id":       file.ID,
		"offset":        size,
		"error_message": "",
		"updated_at":    time.Now(),
	})

	return file, nil
}

func (h *FileHandler) finalizeTusAsync(upload model.TusUpload, ip, userAgent string) {
	// A panic in a bare goroutine escapes gin's Recovery and would leave the
	// row in "finalizing" with no worker behind it. Record the failure instead,
	// so the client polling status gets a sentence rather than a timeout.
	defer func() {
		if r := recover(); r != nil {
			h.markTusFailed(&upload, fmt.Sprintf("finalisation failed unexpectedly: %v", r))
			_ = h.storage.TusRemove(upload.ID.String())
		}
	}()

	file, err := h.finalizeTusClaimed(&upload)
	if err != nil {
		return
	}
	logAuditClient(h.db, upload.UserID, "upload_tus", "file", &file.ID,
		fmt.Sprintf("Uploaded (tus): %s", upload.Filename), ip, userAgent)
}

func (h *FileHandler) markTusFailed(upload *model.TusUpload, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	h.db.Model(&model.TusUpload{}).Where("id = ?", upload.ID).Updates(map[string]interface{}{
		"status":        "failed",
		"error_message": message,
		"updated_at":    time.Now(),
	})
}

// loadTusUpload resolves the id from the route and enforces ownership.
func (h *FileHandler) loadTusUpload(c *gin.Context, userID uuid.UUID) (*model.TusUpload, bool) {
	id, err := uuid.Parse(c.Param("uploadId"))
	if err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid upload id"})
		return nil, false
	}

	var upload model.TusUpload
	if err := h.db.First(&upload, "id = ? AND user_id = ?", id, userID).Error; err != nil {
		c.Header("Tus-Resumable", tusVersion)
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return nil, false
	}
	return &upload, true
}

// tusOffset is the number of bytes the client may trust. The data file is the
// truth: the row's offset can lag by one PATCH after a crash, and reporting the
// stale value would make the client resend bytes it already sent.
func (h *FileHandler) tusOffset(upload *model.TusUpload) (int64, error) {
	if upload.Status == "completed" || upload.Status == "finalizing" {
		return upload.TotalSize, nil
	}
	size, err := h.storage.TusSize(upload.ID.String())
	if err != nil {
		return 0, err
	}
	if size > upload.TotalSize {
		size = upload.TotalSize
	}
	if size > upload.Offset {
		// Heal the row so a later request does not have to reconcile again.
		h.db.Model(&model.TusUpload{}).Where("id = ?", upload.ID).UpdateColumn("offset", size)
	}
	return size, nil
}

func (h *FileHandler) tusTTL() time.Duration {
	ttl := h.storage.GetLimits().CleanupInterval()
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	return ttl
}

func (h *FileHandler) tusLock(id string) func() {
	return h.tusLocks.Lock(id)
}

// pathTakenAnywhere is the unscoped form used during finalisation, where there
// is no request to read a scope from. It is stricter than necessary — a row in
// another user's home does not normally collide — but the files table's unique
// path index is global, so the insert would fail anyway.
func (h *FileHandler) pathTakenAnywhere(path string) bool {
	var count int64
	h.db.Model(&model.File{}).
		Where("path = ? AND is_trashed = ?", path, false).
		Count(&count)
	return count > 0
}

func tusProgress(offset, total int64) float64 {
	if total <= 0 {
		return 0
	}
	progress := float64(offset) / float64(total) * 100
	if progress > 100 {
		progress = 100
	}
	return progress
}

func tusErrorStatus(err error) int {
	switch {
	case errors.Is(err, storage.ErrTusChecksumMismatch):
		return http.StatusBadRequest
	case errors.Is(err, storage.ErrTusTargetExists):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// parseTusMetadata decodes Upload-Metadata's `key base64value` comma-separated
// form. An entry with no value is legal (a flag), so it is kept as an empty
// string rather than rejected.
func parseTusMetadata(raw string) (map[string]string, error) {
	metadata := make(map[string]string)
	if strings.TrimSpace(raw) == "" {
		return metadata, nil
	}

	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, " ")
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("invalid Upload-Metadata")
		}
		if value == "" {
			metadata[key] = ""
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid Upload-Metadata value for %q", key)
		}
		metadata[key] = string(decoded)
	}
	return metadata, nil
}
