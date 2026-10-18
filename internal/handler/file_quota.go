package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

// Per-user quota: what is used, what is in flight, and when a request has to
// be refused.
//
// Usage is measured from the subtree rather than kept as a running total. A
// folder's size changes when a file inside it moves, is trashed or is deleted,
// and every caller that had to remember to adjust a counter is a caller that
// eventually does not — the number drifts and only the user notices.

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
