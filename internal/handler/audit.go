package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

// logAudit appends one row to the audit trail: who did what, to which row,
// from where.
//
// It was four copies of this literal, one per handler, differing only in the
// receiver name. That is four places to forget when a column is added, and the
// copy that gets missed is the one nobody notices is missing.
//
// The write error is dropped on purpose. An audit row is a record of something
// that already happened, not a precondition for it: failing the request because
// the record could not be written turns a completed action into a 500 the
// client will retry, which is a worse outcome than a gap in the trail.
func logAudit(db *gorm.DB, c *gin.Context, userID uuid.UUID, action, resource string, resourceID *uuid.UUID, details string) {
	_ = db.Create(&model.AuditLog{
		UserID:     &userID,
		Action:     action,
		Resource:   resource,
		ResourceID: resourceID,
		Details:    details,
		IPAddress:  c.ClientIP(),
		UserAgent:  c.GetHeader("User-Agent"),
	}).Error
}
