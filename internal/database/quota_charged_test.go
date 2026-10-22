package database

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

func TestMigrateQuotaChargedBackfillsOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "quota.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.Setting{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	user := model.User{
		ID: uuid.New(), Username: "u", Email: "u@t.local",
		PasswordHash: "x", IsActive: true,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	old := model.File{
		ID: uuid.New(), UserID: user.ID, Name: "old.bin", Path: "old.bin", Size: 10,
		StorageType: "local",
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatalf("seed old: %v", err)
	}
	// Simulate the column being added with a false/NULL default: every row
	// that existed before the feature was charged, regardless of what the
	// schema migration chose.
	if err := db.Model(&model.File{}).Where("id = ?", old.ID).Update("quota_charged", false).Error; err != nil {
		t.Fatalf("set old uncharged: %v", err)
	}

	if err := migrateQuotaCharged(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var got model.File
	if err := db.First(&got, "id = ?", old.ID).Error; err != nil {
		t.Fatalf("read old: %v", err)
	}
	if !got.CountsAgainstQuota() {
		t.Fatal("pre-existing row was not marked as charged")
	}

	// Rows adopted after the migration must stay uncharged when the migration
	// runs on the next boot.
	uncharged := false
	adopted := model.File{
		ID: uuid.New(), UserID: user.ID, Name: "adopted.bin", Path: "adopted.bin",
		Size: 20, QuotaCharged: &uncharged, StorageType: "local",
	}
	if err := db.Create(&adopted).Error; err != nil {
		t.Fatalf("seed adopted: %v", err)
	}
	if err := migrateQuotaCharged(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var gotAdopted model.File
	if err := db.First(&gotAdopted, "id = ?", adopted.ID).Error; err != nil {
		t.Fatalf("read adopted: %v", err)
	}
	if gotAdopted.CountsAgainstQuota() {
		t.Fatal("second migration charged a row adopted after the first run")
	}
}
