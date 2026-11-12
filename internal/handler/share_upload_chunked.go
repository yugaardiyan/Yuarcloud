package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"github.com/yuarcloud/yuarcloud/internal/share"
)

// Public chunked upload into a write-enabled shared folder.
//
// The authenticated chunked engine already does everything an upload needs:
// quota, size ceilings, name checks, resumable chunks and background assembly.
// Writing a second copy for visitors would drift from it the first time either
// side changed, so the handlers below validate the share and then hand the
// request to the same FileHandler methods after parking the link's owner and
// the shared folder on the request. Every scoped query inside those methods
// then sees exactly the folder the link grants and nothing above it.
//
// This is what makes an 80 MB upload through a Cloudflare tunnel work: the
// plain share endpoint is one multipart POST, so the whole file has to fit in
// one request — under the 100 MB body limit and inside the ~100 second proxy
// window. The chunked path sends it in pieces the same way the signed-in
// uploader does.

// SetFileHandler gives the public upload access to the chunked engine. The
// dependency is wired after construction because the FileHandler is built
// alongside this handler, and only the upload routes need it.
func (h *ShareHandler) SetFileHandler(files *FileHandler) {
	h.files = files
}

// shareToken reads the token from whichever wildcard name the route had to use.
//
// Everything here is spelled ":token", except the cancel route: gin keeps one
// radix tree per method, and DELETE already carries the authenticated
// "/api/shares/:id". Two different wildcard names at the same position are a
// startup panic, so cancel reuses ":id" and this helper accepts either.
func shareToken(c *gin.Context) string {
	if token := c.Param("token"); token != "" {
		return token
	}
	return c.Param("id")
}

// shareUploadContext validates the link and prepares the request for the
// chunked handlers. It reports false after writing the response when the link
// cannot upload.
func (h *ShareHandler) shareUploadContext(c *gin.Context) bool {
	if h.files == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upload is not available"})
		return false
	}

	link, err := h.shareService.ValidateShare(c.Request.Context(), shareToken(c), c.Query("password"))
	if err != nil {
		h.writeShareAccessError(c, err)
		return false
	}
	if link.Permission != share.PermissionWrite {
		c.JSON(http.StatusForbidden, gin.H{"error": "share link is read-only"})
		return false
	}
	if !link.File.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "share target is not a folder"})
		return false
	}

	// A visitor has no account, so both facts the upload engine needs come from
	// the link: the bytes belong to its owner, and the only tree the request can
	// name is the shared folder.
	c.Set(middleware.UserIDContextKey, link.UserID)
	c.Set(middleware.ScopeContextKey, scope.New(link.File.Path))
	return true
}

// shareUploadSession additionally confines an operation on an existing upload
// session to the shared folder.
//
// The engine looks a session up by its owner and id. A visitor holding the link
// is not that owner, so without this check the only thing standing between the
// link and every other upload the owner has in flight would be the secrecy of a
// UUID. The target path is the session's one durable fact, and the share scope
// is exactly the boundary the link grants.
func (h *ShareHandler) shareUploadSession(c *gin.Context) bool {
	if !h.shareUploadContext(c) {
		return false
	}

	var session model.UploadSession
	if err := h.db.First(&session, "id = ?", c.Param("uploadId")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return false
	}
	if !reqScope(c).Contains(session.TargetPath) {
		c.JSON(http.StatusNotFound, gin.H{"error": "upload session not found"})
		return false
	}
	return true
}

// ShareUploadLimits reports the chunk ceiling to a visitor, so the public
// uploader can size its requests the way the signed-in one does. It is
// configuration, not secrets, and the link has to be able to upload at all.
func (h *ShareHandler) ShareUploadLimits(c *gin.Context) {
	if !h.shareUploadContext(c) {
		return
	}
	h.files.UploadLimits(c)
}

// InitShareUpload starts a chunked session inside the shared folder.
func (h *ShareHandler) InitShareUpload(c *gin.Context) {
	if !h.shareUploadContext(c) {
		return
	}
	h.files.InitChunkedUpload(c)
}

// UploadShareChunk appends one chunk to a session inside the shared folder.
func (h *ShareHandler) UploadShareChunk(c *gin.Context) {
	if !h.shareUploadSession(c) {
		return
	}
	h.files.UploadChunk(c)
}

// CompleteShareUpload assembles the chunks into the final file.
func (h *ShareHandler) CompleteShareUpload(c *gin.Context) {
	if !h.shareUploadSession(c) {
		return
	}
	h.files.CompleteChunkedUpload(c)
}

// GetShareUploadStatus reports a session's progress, including the file record
// once a background assembly finishes.
func (h *ShareHandler) GetShareUploadStatus(c *gin.Context) {
	if !h.shareUploadSession(c) {
		return
	}
	h.files.GetUploadStatus(c)
}

// CancelShareUpload drops a session the visitor abandoned.
func (h *ShareHandler) CancelShareUpload(c *gin.Context) {
	if !h.shareUploadSession(c) {
		return
	}
	h.files.CancelUpload(c)
}
