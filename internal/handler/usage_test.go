package handler

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

func usageTestHandler(t *testing.T) (*FileHandler, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.AuditLog{}, &model.UploadSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewFileHandler(db, nil, nil), db
}

// used_bytes is the number every quota decision is made against, so it has to
// come back down. These pin the two ways it used to ratchet upward instead.
func seedSizedFile(t *testing.T, h *FileHandler, userID uuid.UUID, path string, size int64) model.File {
	t.Helper()
	f := model.File{
		ID: uuid.New(), UserID: userID, Name: path, Path: path,
		Size: size, MimeType: "application/octet-stream", StorageType: "local", StoragePath: path,
	}
	if err := h.db.Create(&f).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	return f
}

func usedOf(t *testing.T, h *FileHandler, userID uuid.UUID) int64 {
	t.Helper()
	var got int64
	h.db.Model(&model.User{}).Where("id = ?", userID).Select("used_bytes").Scan(&got)
	return got
}

// The measurement has to happen before the soft delete. GORM hides stamped
// rows from every query on the model, so measuring afterwards returns zero and
// nothing is ever subtracted.
func TestSubtreeSizeExcludesSoftDeletedRows(t *testing.T) {
	h, db := usageTestHandler(t)
	user := quotaUser(t, db, 0, 1000)
	f := seedSizedFile(t, h, user, "doomed.bin", 500)

	if got := h.subtreeSize(user, f.Path); got != 500 {
		t.Fatalf("subtreeSize before delete = %d, want 500", got)
	}

	if err := db.Delete(&f).Error; err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if got := h.subtreeSize(user, f.Path); got != 0 {
		t.Fatalf("subtreeSize after soft delete = %d, want 0 — this is why the caller must measure first", got)
	}
}

// Reproduces the drift: charge for a file, then release it the way the
// permanent-delete path does. Before the fix this left used_bytes permanently
// 500 too high.
func TestPermanentDeleteReleasesTheBytes(t *testing.T) {
	h, db := usageTestHandler(t)
	user := quotaUser(t, db, 0, 1000)

	f := seedSizedFile(t, h, user, "gone.bin", 500)
	h.adjustUsage(user, f.Size)

	if got := usedOf(t, h, user); got != 500 {
		t.Fatalf("after charge used_bytes = %d, want 500", got)
	}

	freed := h.subtreeSize(f.UserID, f.Path)
	h.db.Where("path = ? OR path LIKE ?", f.Path, f.Path+"/%").Delete(&model.File{})
	if err := db.Delete(&f).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	h.adjustUsage(f.UserID, -freed)

	if got := usedOf(t, h, user); got != 0 {
		t.Fatalf("after permanent delete used_bytes = %d, want 0", got)
	}
}

// An admin may delete another account's file. The bytes must come off the
// owner's counter; charging the caller left the owner inflated and quietly
// corrupted the admin's number in the other direction.
func TestPermanentDeleteChargesTheOwnerNotTheCaller(t *testing.T) {
	h, db := usageTestHandler(t)
	owner := quotaUser(t, db, 0, 1000)
	admin := quotaUser(t, db, 0, 1000)

	f := seedSizedFile(t, h, owner, "theirs.bin", 700)
	h.adjustUsage(owner, f.Size)

	// Deleting as `admin`: the same two calls, charged to the owner.
	freed := h.subtreeSize(f.UserID, f.Path)
	if err := db.Delete(&f).Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	h.adjustUsage(f.UserID, -freed)

	if got := usedOf(t, h, owner); got != 0 {
		t.Fatalf("owner used_bytes = %d, want 0", got)
	}
	if got := usedOf(t, h, admin); got != 0 {
		t.Fatalf("admin used_bytes = %d, want 0 — the caller was charged for someone else's file", got)
	}
}

// The counter must never go negative, even if a delete is replayed.
func TestAdjustUsageClampsAtZero(t *testing.T) {
	h, db := usageTestHandler(t)
	user := quotaUser(t, db, 0, 1000)

	h.adjustUsage(user, -9999)
	if got := usedOf(t, h, user); got != 0 {
		t.Fatalf("used_bytes = %d, want 0", got)
	}
	h.adjustUsage(user, 250)
	if got := usedOf(t, h, user); got != 250 {
		t.Fatalf("used_bytes = %d, want 250", got)
	}
}
