package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yuarcloud/yuarcloud/internal/admin"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
	"gorm.io/gorm"
)

// Instance-level overview: the handler itself, and the two things an operator
// looks at to answer "is this instance healthy" — usage across every account,
// and what has happened on it.

type AdminHandler struct {
	db           *gorm.DB
	adminService *admin.AdminService
	authService  *auth.AuthService
	shareService *share.ShareService
	// storage is here so the chunked-upload settings can be written straight into
	// the live limits; without it the panel could only write rows nobody read.
	storage storage.StorageBackend
}

func NewAdminHandler(db *gorm.DB, adminService *admin.AdminService, authService *auth.AuthService, shareService *share.ShareService, store storage.StorageBackend) *AdminHandler {
	return &AdminHandler{
		db:           db,
		adminService: adminService,
		authService:  authService,
		shareService: shareService,
		storage:      store,
	}
}

func (h *AdminHandler) GetStats(c *gin.Context) {
	ctx := c.Request.Context()

	userStats, err := h.adminService.GetStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get stats"})
		return
	}

	storageStats, err := h.adminService.GetStorageStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get storage stats"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"users":   userStats,
		"storage": storageStats,
	})
}

func (h *AdminHandler) GetAuditLogs(c *gin.Context) {
	page := 1
	pageSize := 50
	action := c.Query("action")
	userID := c.Query("user_id")

	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	ctx := c.Request.Context()
	logs, total, err := h.adminService.GetAuditLogs(ctx, page, pageSize, action, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get audit logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":     logs,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}
