package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
)

// IssueDownloadToken mints the short-lived token a download URL carries.
//
// Downloads used to be fetched with the session access token in an
// Authorization header and turned into a blob before saving. That works for
// small files and gets in the way for large ones: the browser cannot show
// progress, cannot resume, and the whole file has to be buffered before the
// save dialog appears. Handing the browser a URL lets it stream the response
// straight to disk — but a link cannot send a header, so the URL needs a
// credential. This is that credential: five minutes, one item, and refused by
// every route that is not a download.
func (h *FileHandler) IssueDownloadToken(c *gin.Context) {
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	// Same scope and trash rules as the download itself. Minting a token for
	// something outside the caller's home would hand out access the download
	// route would then honour.
	file, ok := h.lookupFile(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	token, ok := h.mintDownloadToken(c, file.ID, nil)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_in": int64(auth.DownloadTokenTTL.Seconds()),
		"is_dir":     file.IsDir,
	})
}

// IssueBatchDownloadToken mints one token for a selection, so "download as
// zip" can also be a plain URL.
//
// The ids are resolved through the caller's scope first, so the token names
// exactly what the archive will contain. The zip route re-checks them anyway;
// this only keeps the token from claiming access it never had.
func (h *FileHandler) IssueBatchDownloadToken(c *gin.Context) {
	var body downloadZipBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids are required"})
		return
	}

	files, ok := h.resolveZipItems(c, body.IDs)
	if !ok {
		return
	}

	ids := make([]uuid.UUID, 0, len(files))
	for i := range files {
		ids = append(ids, files[i].ID)
	}

	token, ok := h.mintDownloadToken(c, uuid.Nil, ids)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_in": int64(auth.DownloadTokenTTL.Seconds()),
	})
}

func (h *FileHandler) mintDownloadToken(c *gin.Context, fileID uuid.UUID, fileIDs []uuid.UUID) (string, bool) {
	claims, ok := middleware.GetClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return "", false
	}
	role, _ := middleware.GetUserRole(c)

	token, err := h.authService.GenerateDownloadToken(
		claims.UserID, claims.Username, role, claims.TokenVersion, fileID, fileIDs,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue download token"})
		return "", false
	}
	return token, true
}
