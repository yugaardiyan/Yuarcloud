package handler

import (
	"archive/zip"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
	"gorm.io/gorm"
)

type ShareHandler struct {
	db           *gorm.DB
	shareService *share.ShareService
	storage      storage.StorageBackend
}

func NewShareHandler(db *gorm.DB, shareService *share.ShareService, store storage.StorageBackend) *ShareHandler {
	return &ShareHandler{
		db:           db,
		shareService: shareService,
		storage:      store,
	}
}

// Read through storage, not a copy taken at construction — see the note on
// FileHandler.maxFileSize.
func (h *ShareHandler) maxFileSize() int64 { return h.storage.GetLimits().MaxFileSize() }

func (h *ShareHandler) CreateShare(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var req struct {
		FileID     uuid.UUID `json:"file_id" binding:"required"`
		Password   string    `json:"password"`
		ExpiryDays int       `json:"expiry_days"`
		MaxViews   int       `json:"max_views"`
		Permission string    `json:"permission"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// is_trashed is part of the lookup. It was not: a folder moved to the trash
	// still had its row, so a share was created pointing at it, and the link
	// went on serving the folder from the trash to anyone holding it — public
	// access to bytes the owner believed they had put away. Restore put the
	// folder back and the link silently started serving the live tree too.
	var file model.File
	if err := h.db.First(&file, "id = ? AND user_id = ? AND is_trashed = ?", req.FileID, userID, false).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	// "Read & write" is a folder-only feature; reject the rest early so the
	// visitor never lands on a share that promises uploads into a file.
	if req.Permission == share.PermissionWrite && !file.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read & write sharing is only available for folders"})
		return
	}

	ctx := c.Request.Context()
	shareLink, err := h.shareService.CreateShare(ctx, userID, req.FileID, req.Password, req.ExpiryDays, req.MaxViews, req.Permission)
	if err != nil {
		if err == share.ErrShareAlreadyExists {
			c.JSON(http.StatusConflict, gin.H{"error": "a share link already exists for this file. Delete it first."})
			return
		}
		if err == share.ErrPasswordRequired {
			c.JSON(http.StatusBadRequest, gin.H{"error": "this server requires a password on every share link"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create share link"})
		return
	}

	url := h.shareService.ShareURL(shareLink.Token, c.Request)

	logAudit(h.db, c, userID, "create_share", "share", &shareLink.ID, "Created share link for: "+file.Name)

	c.JSON(http.StatusCreated, model.ShareLinkResponse{
		ID:          shareLink.ID,
		FileID:      shareLink.FileID,
		FileName:    file.Name,
		Token:       shareLink.Token,
		URL:         url,
		ExpiresAt:   shareLink.ExpiresAt,
		MaxViews:    shareLink.MaxViews,
		ViewCount:   shareLink.ViewCount,
		IsActive:    shareLink.IsActive,
		Permission:  shareLink.Permission,
		Status:      share.Status(shareLink),
		HasPassword: shareLink.PasswordHash != "",
		CreatedAt:   shareLink.CreatedAt,
	})
}

func (h *ShareHandler) GetShares(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	page := 1
	pageSize := 20
	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	var fileID *uuid.UUID
	if raw := c.Query("file_id"); raw != "" {
		parsed, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file_id"})
			return
		}
		fileID = &parsed
	}

	ctx := c.Request.Context()
	shares, total, err := h.shareService.GetUserShares(ctx, userID, page, pageSize, fileID)
	if err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get shares"})
		return
	}

	// An empty (non-nil) slice marshals to "[]". A nil slice would become
	// "null", which some clients then can't iterate over.
	response := make([]model.ShareLinkResponse, 0, len(shares))
	for _, s := range shares {
		response = append(response, model.ShareLinkResponse{
			ID:          s.ID,
			FileID:      s.FileID,
			FileName:    s.File.Name,
			Token:       s.Token,
			URL:         h.shareService.ShareURL(s.Token, c.Request),
			ExpiresAt:   s.ExpiresAt,
			MaxViews:    s.MaxViews,
			ViewCount:   s.ViewCount,
			IsActive:    s.IsActive,
			Permission:  s.Permission,
			Status:      share.Status(s),
			HasPassword: s.PasswordHash != "",
			CreatedAt:   s.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"shares":   response,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}

func (h *ShareHandler) DeleteShare(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	shareID := c.Param("id")
	parsed, err := uuid.Parse(shareID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid share id"})
		return
	}

	ctx := c.Request.Context()
	if err := h.shareService.DeleteShare(ctx, userID, parsed); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete share"})
		return
	}

	logAudit(h.db, c, userID, "delete_share", "share", &parsed, "Deleted share link")

	c.JSON(http.StatusOK, gin.H{"message": "share deleted"})
}

// writeShareAccessError maps a ValidateShare error to its HTTP response and
// writes it, so the public endpoints (access, download, upload) can stay in
// sync without drifting.
func (h *ShareHandler) writeShareAccessError(c *gin.Context, err error) {
	switch err {
	case share.ErrShareNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": "share link not found"})
	case share.ErrShareExpired:
		c.JSON(http.StatusGone, gin.H{"error": "share link expired"})
	case share.ErrShareInactive:
		c.JSON(http.StatusForbidden, gin.H{"error": "share link inactive"})
	case share.ErrShareMaxViews:
		c.JSON(http.StatusForbidden, gin.H{"error": "share link max views reached"})
	case share.ErrInvalidPassword:
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password", "requires_password": true})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to access share"})
	}
}

// Public share access (no auth required)
func (h *ShareHandler) AccessShare(c *gin.Context) {
	token := c.Param("token")
	password := c.Query("password")

	ctx := c.Request.Context()
	shareLink, err := h.shareService.ValidateShare(ctx, token, password)
	if err != nil {
		h.writeShareAccessError(c, err)
		return
	}

	shareLink.ViewCount++

	file := shareLink.File
	c.JSON(http.StatusOK, gin.H{
		"file": model.FileResponse{
			ID:        file.ID,
			Name:      file.Name,
			Path:      file.Path,
			Size:      file.Size,
			MimeType:  file.MimeType,
			IsDir:     file.IsDir,
			Checksum:  file.Checksum,
			CreatedAt: file.CreatedAt,
			UpdatedAt: file.UpdatedAt,
		},
		"share": model.ShareLinkResponse{
			ID:          shareLink.ID,
			FileID:      shareLink.FileID,
			FileName:    file.Name,
			Token:       shareLink.Token,
			URL:         h.shareService.ShareURL(shareLink.Token, c.Request),
			ExpiresAt:   shareLink.ExpiresAt,
			MaxViews:    shareLink.MaxViews,
			ViewCount:   shareLink.ViewCount,
			IsActive:    shareLink.IsActive,
			Permission:  shareLink.Permission,
			Status:      share.Status(shareLink),
			HasPassword: shareLink.PasswordHash != "",
			CreatedAt:   shareLink.CreatedAt,
		},
	})
}

func (h *ShareHandler) DownloadSharedFile(c *gin.Context) {
	token := c.Param("token")
	password := c.Query("password")

	ctx := c.Request.Context()
	shareLink, err := h.shareService.ValidateShare(ctx, token, password)
	if err != nil {
		h.writeShareAccessError(c, err)
		return
	}

	// A `path` inside the shared folder selects one of its children; without
	// one this is the shared item itself. Same scoping for files and folders,
	// so a browse row can download or preview in place.
	file, ok := h.shareTarget(c, &shareLink.File)
	if !ok {
		return
	}

	h.shareService.IncrementViewCount(ctx, shareLink.ID)

	if file.IsDir {
		h.streamFolderZip(c, *file)
		return
	}

	reader, err := h.storage.Read(ctx, file.Path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file is no longer available"})
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", contentDisposition("attachment", file.Name))
	c.Header("Content-Type", file.MimeType)
	c.Header("Content-Length", fmt.Sprintf("%d", file.Size))
	c.DataFromReader(http.StatusOK, file.Size, file.MimeType, reader, nil)
}

// PreviewSharedFile streams a shared file inline so a visitor can view it in
// the browser instead of saving it. Same body as the authenticated preview —
// inline disposition plus Range support — and, like that one, it does not bump
// the view counter: previewing a PDF or scrubbing a video would otherwise burn
// through a view-limited link in seconds.
func (h *ShareHandler) PreviewSharedFile(c *gin.Context) {
	token := c.Param("token")
	password := c.Query("password")

	ctx := c.Request.Context()
	shareLink, err := h.shareService.ValidateShare(ctx, token, password)
	if err != nil {
		h.writeShareAccessError(c, err)
		return
	}

	// Like the download endpoint, `path` can point at a row inside the shared
	// folder, which is what the browse list uses to preview in place.
	target, ok := h.shareTarget(c, &shareLink.File)
	if !ok {
		return
	}

	serveInline(c, h.storage, target)
}

// streamFolderZip streams a shared folder to the visitor as a ZIP archive.
//
// The disk is the source of truth, not the rows: a shared folder can contain
// descendants that no authenticated listing has adopted yet, and the visitor
// must still receive them. The link's path prefix already confines the walk to
// the owner's tree; writeFolderZip skips symlinks and unreadable entries.
func (h *ShareHandler) streamFolderZip(c *gin.Context, folder model.File) {
	c.Header("Content-Disposition", contentDisposition("attachment", folder.Name+".zip"))
	c.Header("Content-Type", "application/zip")

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()
	writeFolderZip(c.Request.Context(), h.storage, zw, folder.Path)
}

// ShareUpload lets a visitor push a file into a write-enabled shared folder
// without an account. The file lands inside the shared folder and is owned
// by the link's creator, exactly as if they had uploaded it themselves.
func (h *ShareHandler) ShareUpload(c *gin.Context) {
	token := c.Param("token")
	password := c.Query("password")

	ctx := c.Request.Context()
	shareLink, err := h.shareService.ValidateShare(ctx, token, password)
	if err != nil {
		h.writeShareAccessError(c, err)
		return
	}

	if shareLink.Permission != share.PermissionWrite {
		c.JSON(http.StatusForbidden, gin.H{"error": "share link is read-only"})
		return
	}
	if !shareLink.File.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "share target is not a folder"})
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeUploadReadError(c, err, "no file provided")
		return
	}

	ownerID := shareLink.UserID
	parentFolder := shareLink.File

	// The name goes through the same validation as an authenticated upload.
	// This endpoint is public — anyone holding the token reaches it with no
	// account — and it was the only upload path that used the raw multipart
	// filename. Go's multipart parser applies filepath.Base, so a forward slash
	// cannot escape, but "." and ".." and control characters could still reach
	// the files table and from there into Content-Disposition headers.
	safeName, ok := safeFilename(fileHeader.Filename)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}
	targetPath := strings.TrimPrefix(filepath.Join(parentFolder.Path, safeName), "/")

	if max := h.maxFileSize(); max > 0 && fileHeader.Size > max {
		// 413, not 400: the request is well-formed, the payload is simply
		// too big. 400 tells a client it sent something the server could not
		// parse, which is not true and sends the caller looking for a syntax
		// error that does not exist.
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file too large"})
		return
	}

	// A visitor holds no account, so the upload is charged to the link's owner —
	// and that owner's quota has to be consulted before the bytes land, not
	// after. Enforcing it only behind the login would leave the share link as a
	// way around the limit entirely, which is the whole point of a quota.
	// In-flight chunked sessions count too, for the same reason: without it the
	// owner can reserve the quota through the authenticated path and spend the
	// remainder here, and each check passes on its own.
	over, by := quotaExceeded(h.db, ownerID, fileHeader.Size, inflightUploadBytes(h.db, ownerID))
	if over {
		body := gin.H{"error": "storage quota exceeded", "quota": true}
		if by > 0 {
			body["short_by"] = by
		}
		c.JSON(http.StatusInsufficientStorage, body)
		return
	}

	// An occupied name is refused before a byte is written.
	//
	// This path writes with os.Create, which truncates, and the insert below
	// fails on the unique path index - so uploading a name that already existed
	// used to destroy the owner's bytes and then delete the file outright while
	// its row stayed behind. A public link could erase a file by re-uploading
	// its name. Both checks are needed: the row catches files the app knows
	// about, the disk catches leftovers and anything placed there by hand.
	var occupied int64
	if err := h.db.Model(&model.File{}).Where("path = ?", targetPath).Count(&occupied).Error; err == nil && occupied > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
		return
	}
	if exists, err := h.storage.Exists(ctx, targetPath); err == nil && exists {
		c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open uploaded file"})
		return
	}
	defer src.Close()

	if err := h.storage.Write(ctx, targetPath, src, fileHeader.Size); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save file"})
		return
	}

	mimeType := fileHeader.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = mime.TypeByExtension(filepath.Ext(safeName))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	file := &model.File{
		UserID:      ownerID,
		ParentID:    &parentFolder.ID,
		Name:        safeName,
		Path:        targetPath,
		Size:        fileHeader.Size,
		MimeType:    mimeType,
		IsDir:       false,
		StorageType: "local",
		StoragePath: targetPath,
	}
	if err := h.db.Create(file).Error; err != nil {
		// Only remove bytes this request owns. If a concurrent request managed
		// to register the same path first, its row is live and its bytes must
		// survive this request's failure.
		var rows int64
		h.db.Model(&model.File{}).Where("path = ?", targetPath).Count(&rows)
		if rows == 0 {
			h.storage.Remove(ctx, targetPath)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file record"})
		return
	}

	// Charge the upload to the link owner's quota.
	h.db.Model(&model.User{}).Where("id = ?", ownerID).UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", fileHeader.Size))

	logAudit(h.db, c, ownerID, "share_upload", "file", &file.ID, fmt.Sprintf("Uploaded via share link: %s", safeName))

	c.JSON(http.StatusCreated, model.ToFileResponse(file))
}
