package database

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"gorm.io/gorm"
)

// WAL is a performance setting, not a correctness one — except for the part
// where it changes what a backup has to include. These pin both halves.

func walDB(t *testing.T, dir string) *gorm.DB {
	t.Helper()
	cfg := &config.Config{
		Database: config.DatabaseConfig{
			Type:   "sqlite",
			SQLite: config.SQLiteConfig{Path: filepath.Join(dir, "y.db")},
		},
	}
	db, err := Init(cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return db
}

func TestInitEnablesWAL(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)

	var mode string
	if err := db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

// Foreign keys are per-connection in SQLite, so enabling WAL — which changes
// how connections are opened and reused — is exactly the kind of change that
// quietly drops a pragma set somewhere else.
func TestWALDoesNotCostForeignKeys(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)

	var fk int
	if err := db.Raw("PRAGMA foreign_keys").Scan(&fk).Error; err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

// The reason Shutdown exists. A committed transaction can still live in the
// -wal file; a backup copying y.db on its own would restore to a state older
// than the last write, and would open without complaining about it.
func TestShutdownCheckpointsSoTheDatabaseFileStandsAlone(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)

	const marker = "wal-shutdown-marker"
	db.Exec("INSERT INTO settings (key, value, created_at, updated_at) VALUES (?,?,datetime('now'),datetime('now'))",
		"wal.test.key", marker)

	walPath := filepath.Join(dir, "y.db-wal")
	if _, err := os.Stat(walPath); err != nil {
		t.Skipf("no -wal file on this platform (%v); nothing to assert", err)
	}

	Shutdown(db)

	// Closing the last connection removes the -wal and -shm files outright, so
	// the assertion that matters is the weak one: nothing is left pending in the
	// log. Either outcome means the same thing to anyone copying y.db.
	if info, err := os.Stat(walPath); err == nil && info.Size() != 0 {
		t.Errorf("-wal is %d bytes after shutdown — the row would not be in y.db alone", info.Size())
	} else if err != nil && !os.IsNotExist(err) {
		t.Errorf("stat -wal after shutdown: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsBytes(raw, []byte(marker)) {
		t.Error("the value is not present in y.db after a clean shutdown")
	}
}

// Shutdown runs while the process is stopping. A checkpoint that fails there is
// not worth turning into a failed shutdown, and must not stop the pool close.
func TestShutdownSurvivesAnAlreadyClosedPool(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	Shutdown(db) // a second close: an error logged, not a panic
}

func TestShutdownOnNilHandleDoesNotPanic(t *testing.T) {
	Shutdown(nil)
}

// Data written under WAL has to still be there, in full, after the checkpoint.
// A TRUNCATE that dropped the last transactions would pass the "file is empty"
// assertion above while losing data.
func TestCheckpointPreservesEverythingWritten(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)

	for i := 0; i < 40; i++ {
		db.Exec("INSERT INTO settings (key, value, created_at, updated_at) VALUES (?,?,datetime('now'),datetime('now'))",
			"k", "v")
	}
	var before int64
	if err := db.Raw("SELECT count(*) FROM settings").Scan(&before).Error; err != nil {
		t.Fatalf("count before: %v", err)
	}

	Shutdown(db)

	// Reopen the file from scratch. If the checkpoint dropped anything, the
	// count comes back short — which is exactly what a restored backup would see.
	reopened := walDB(t, dir)
	defer Shutdown(reopened)

	var after int64
	if err := reopened.Raw("SELECT count(*) FROM settings").Scan(&after).Error; err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != before {
		t.Errorf("rows after reopen = %d, want %d", after, before)
	}
}

func containsBytes(hay, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
