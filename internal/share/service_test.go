package share

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

// An expired link must not be listed. The settings page only looked right
// because the query happened to filter; the share dialog lists the same
// endpoint, and a row for a link nobody can open is worse than no row at all.
// These pin the filter so neither surface can regress into showing dead URLs.
var shareDBCounter atomic.Int64

func freshShareDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:share_%s_%d?mode=memory&cache=shared", t.Name(), shareDBCounter.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.ShareLink{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedShare(t *testing.T, db *gorm.DB, owner uuid.UUID, fileID uuid.UUID, token string, expires *time.Time) {
	t.Helper()
	link := model.ShareLink{
		ID: uuid.New(), UserID: owner, FileID: fileID, Token: token,
		ExpiresAt: expires, IsActive: true, Permission: PermissionRead,
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("seed share %s: %v", token, err)
	}
}

func seedOwner(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := model.User{
		ID: uuid.New(), Username: "s" + uuid.New().String()[:8],
		Email: uuid.New().String() + "@test", PasswordHash: "x", IsActive: true,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	file := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "PTK intan", Path: "PTK intan",
		Size: 0, IsDir: true,
	}
	if err := db.Create(&file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}
	return owner.ID, file.ID
}

func TestExpiredLinkIsNotListed(t *testing.T) {
	db := freshShareDB(t)
	owner, fileID := seedOwner(t, db)
	svc := NewShareService(db, "", 7, 365, false, 0)

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)

	seedShare(t, db, owner, fileID, "dead01", &past)   // expired by date
	seedShare(t, db, owner, fileID, "live01", &future) // still has time
	seedShare(t, db, owner, fileID, "forever01", nil)  // no expiry at all

	for _, name := range []string{"per-file (share dialog)", "user-wide (settings)"} {
		t.Run(name, func(t *testing.T) {
			var filter *uuid.UUID
			if name == "per-file (share dialog)" {
				filter = &fileID
			}
			shares, total, err := svc.GetUserShares(context.Background(), owner, 1, 20, filter)
			if err != nil {
				t.Fatalf("GetUserShares: %v", err)
			}
			tokens := make(map[string]bool, len(shares))
			for _, s := range shares {
				tokens[s.Token] = true
			}
			if tokens["dead01"] {
				t.Errorf("expired link was listed: %v", tokens)
			}
			if !tokens["live01"] {
				t.Errorf("link with remaining time is missing: %v", tokens)
			}
			if !tokens["forever01"] {
				t.Errorf("no-expiry link is missing: %v", tokens)
			}
			if total != int64(len(shares)) {
				t.Errorf("total = %d, listed = %d: the count must agree with the list", total, len(shares))
			}
			if total != 2 {
				t.Errorf("total = %d, want 2 (expired links must not be counted)", total)
			}
		})
	}
}

// The dialogs only reload when something asks them to. This checks the rule the
// client mirrors: the moment a date is behind us, the link is gone, with no
// delete needed and no grace period.
func TestStatusFlipsAtTheExpirySecond(t *testing.T) {
	at := time.Now().Add(2 * time.Second)
	link := model.ShareLink{IsActive: true, ExpiresAt: &at, Permission: PermissionRead}

	if got := Status(&link); got != StatusActive {
		t.Fatalf("status before the deadline = %q, want %q", got, StatusActive)
	}

	past := time.Now().Add(-time.Second)
	link.ExpiresAt = &past
	if got := Status(&link); got != StatusExpired {
		t.Fatalf("status after the deadline = %q, want %q", got, StatusExpired)
	}

	// No date is a deliberate choice, not an omission: it must never expire.
	link.ExpiresAt = nil
	if got := Status(&link); got != StatusActive {
		t.Fatalf("status with no expiry = %q, want %q", got, StatusActive)
	}
}
