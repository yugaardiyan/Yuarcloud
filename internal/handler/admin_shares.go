package handler

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/share"
)

// Share links seen from the admin side. An admin has no owner here — these are
// every link on the instance, which is the point: a link whose owner has been
// deleted is still live until somebody with this access revokes it.

// GetShares lists every share link on the instance.
//
// This used to sit here with a note that it "replaces the audit-log view in the
// admin UI: server-side events are read from the container log". That was false,
// and it was load-bearing — it is the stated reason the audit table was removed
// from the panel.
//
// Server-side events are not read from the container log. They are rows: every
// logAudit call writes to the audit_logs table, and GetAuditLogs serves them
// back over GET /api/admin/audit-logs. Measured on a throwaway instance: seven
// rows written, all seven returned by the endpoint, all seven still there after
// a restart, and zero of them in the application log — the only audit-shaped
// lines there are gin's access log recording requests to the endpoint itself.
// The container log carries the request; the table carries the event.
//
// The endpoint works. What is missing is a view for it. Restoring that is a
// frontend change and nothing here needs to move to make it possible.
func (h *AdminHandler) GetShares(c *gin.Context) {
	page := 1
	pageSize := 50

	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	ctx := c.Request.Context()
	shares, total, err := h.shareService.GetAllShares(ctx, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get share links"})
		return
	}

	type entry struct {
		ID         uuid.UUID  `json:"id"`
		Token      string     `json:"token"`
		URL        string     `json:"url"`
		Path       string     `json:"path"`
		Owner      string     `json:"owner"`
		FileName   string     `json:"file_name"`
		FileID     uuid.UUID  `json:"file_id"`
		Active     bool       `json:"is_active"`
		HasPass    bool       `json:"has_password"`
		Permission string     `json:"permission"`
		Views      int        `json:"views"`
		MaxViews   int        `json:"max_views"`
		Expires    *time.Time `json:"expires_at"`
		CreatedAt  time.Time  `json:"created_at"`
	}

	out := make([]entry, 0, len(shares))
	for _, s := range shares {
		// User is a value field, not a pointer, so a row whose owner has been
		// deleted preloads as the zero value rather than nil.
		owner := s.User.DisplayName
		if owner == "" {
			owner = s.User.Username
		}
		url := h.shareService.ShareURL(s.Token, c.Request)
		out = append(out, entry{
			ID:    s.ID,
			Token: s.Token,
			// ShareURL rather than a token-only builder: with no base_url
			// configured the latter returns a bare "/yc/<token>", which is no
			// use in a copy button.
			URL: url,
			// Path is what the table shows. The absolute URL is long enough to
			// push an eighth column past the edge of a laptop screen, and the
			// host is already visible in the address bar.
			Path:       share.URLPath + s.Token,
			Owner:      owner,
			FileName:   s.File.Name,
			FileID:     s.FileID,
			Active:     s.IsActive,
			HasPass:    s.PasswordHash != "",
			Permission: s.Permission,
			Views:      s.ViewCount,
			MaxViews:   s.MaxViews,
			Expires:    s.ExpiresAt,
			CreatedAt:  s.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"shares":    out,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// DeleteShare revokes a share link from the admin view. Unlike the per-user
// endpoint this is not scoped to an owner — an admin is expected to be able
// to take down any link on the instance.
func (h *AdminHandler) DeleteShare(c *gin.Context) {
	shareID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid share id"})
		return
	}

	if err := h.shareService.DeleteShareByID(c.Request.Context(), shareID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete share link"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "share link deleted"})
}
