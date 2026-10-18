package database

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"golang.org/x/crypto/bcrypt"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

// sqliteDSN returns the DSN for a SQLite file with the pragmas that have to be
// in force on *every* connection, not just the one that happened to open first.
//
// foreign_keys is the important one. `PRAGMA foreign_keys = ON` issued through
// the pool only configures whichever connection ran it — the pool is allowed 25
// connections and grows under load, and every one grown later starts with
// foreign keys OFF. Measured on this codebase, 3 of 20 connections held at once
// had enforcement disabled. Nothing stops those from inserting rows pointing at
// users or files that do not exist, which is how orphans accumulate in a table
// that was supposed to prevent them. Putting it in the DSN makes the driver run
// it as each connection is opened.
//
// busy_timeout is for the same reason: SQLite serialises writers, and without a
// wait a second concurrent write gets SQLITE_BUSY immediately instead of
// retrying. The app serves requests in parallel, so that is the normal case,
// not an edge one.
func sqliteDSN(path string) string {
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	return path + sep + "_foreign_keys=on&_busy_timeout=10000"
}

func Init(cfg *config.Config) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch cfg.Database.Type {
	case "postgres":
		dialector = postgres.Open(cfg.GetDSN())
	default:
		// Ensure SQLite directory exists
		dbDir := filepath.Dir(cfg.Database.SQLite.Path)
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
		dialector = sqlite.Open(sqliteDSN(cfg.Database.SQLite.Path))
	}

	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	}

	if cfg.Logging.Level == "debug" {
		gormConfig.Logger = logger.Default.LogMode(logger.Info)
	}

	db, err := gorm.Open(dialector, gormConfig)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Auto migrate
	if err := autoMigrate(db); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}

	// Configure connection pool
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)

	return db, nil
}

func autoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.User{},
		&model.File{},
		&model.ShareLink{},
		&model.Setting{},
		&model.UploadSession{},
		&model.AuditLog{},
	); err != nil {
		return err
	}
	if err := migratePartialUserIndexes(db); err != nil {
		return err
	}

	return migrateFilePathUniqueness(db)
}

// migrateFilePathUniqueness makes one live row per path, and enforces it.
//
// Nothing stopped two rows from claiming the same path. Uploading a file whose
// name was already taken overwrote the bytes on disk but inserted a second row,
// so the same file was counted twice in used_bytes and twice in every file
// count — and a later delete released only one copy, leaving the other
// stranded. On a real database this had produced 9 surplus rows and inflated
// the account's storage counter by tens of megabytes.
//
// The newest row wins: it is the one whose size matches the bytes actually on
// disk, since that was written by the upload that created it.
//
// Partial, matching the unique indexes on users: soft-deleted rows keep their
// old path and must not block a new file that legitimately takes it back.
func migrateFilePathUniqueness(db *gorm.DB) error {
	var done int64
	db.Raw("SELECT count(*) FROM settings WHERE key = ?", "migration.file_path_unique.v1").Scan(&done)
	if done > 0 {
		return nil
	}

	// ROW_NUMBER rather than MAX(updated_at): two rows written in the same
	// instant would tie, and the comparison would then keep both of them —
	// which is the exact condition this migration exists to remove.
	if err := db.Exec(
		`DELETE FROM files WHERE deleted_at IS NULL AND id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY path ORDER BY updated_at DESC, id DESC
				) AS rn
				FROM files WHERE deleted_at IS NULL
			) WHERE rn > 1
		)`,
	).Error; err != nil {
		return fmt.Errorf("dedupe file paths: %w", err)
	}

	if err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_path_unique ON files(path) WHERE deleted_at IS NULL`,
	).Error; err != nil {
		return fmt.Errorf("index unique file paths: %w", err)
	}

	return db.Exec(
		"INSERT OR REPLACE INTO settings (key, value, description, created_at, updated_at) VALUES (?, ?, ?, datetime('now'), datetime('now'))",
		"migration.file_path_unique.v1",
		"deduped",
		"Surplus rows sharing a path removed; unique index on files(path) for live rows",
	).Error
}

// migratePartialUserIndexes rewrites the username and email indexes to partial
// ones covering live rows only.
//
// AutoMigrate never drops an index it did not create, so changing the model's
// index tags is not enough on an existing database: the original full-width
// unique indexes survive and keep soft-deleted accounts holding their names
// hostage. Recreating them has to be stated explicitly.
//
// Safe to run repeatedly — it checks the current definition first, so a database
// that is already correct is left alone rather than dropped and rebuilt on every
// boot.
func migratePartialUserIndexes(db *gorm.DB) error {
	for _, spec := range []struct{ name, table, column, ddl string }{
		{"idx_users_username", "users", "username",
			"CREATE UNIQUE INDEX idx_users_username ON users(username) WHERE deleted_at IS NULL"},
		{"idx_users_email", "users", "email",
			"CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE deleted_at IS NULL"},
	} {
		var sql string
		err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", spec.name).
			Scan(&sql).Error
		if err != nil {
			return fmt.Errorf("inspect index %s: %w", spec.name, err)
		}

		// Absent, or already partial, and there is nothing to do.
		if sql == "" || strings.Contains(strings.ToUpper(sql), "WHERE DELETED_AT IS NULL") {
			continue
		}

		// A full-width unique index could already be contradicted by a rename
		// back onto a soft-deleted name. Refuse rather than leave the database
		// half-migrated and without the uniqueness it used to have.
		var dupes int64
		if err := db.Raw(
			"SELECT count(*) FROM (SELECT " + spec.column + " FROM " + spec.table + " GROUP BY " + spec.column + " HAVING count(*) > 1)",
		).Scan(&dupes).Error; err != nil {
			return fmt.Errorf("check duplicates for %s: %w", spec.name, err)
		}
		if dupes > 0 {
			return fmt.Errorf(
				"cannot make %s partial: %d duplicated %s value(s) exist across deleted and live rows",
				spec.name, dupes, spec.column,
			)
		}

		if err := db.Exec("DROP INDEX " + spec.name).Error; err != nil {
			return fmt.Errorf("drop index %s: %w", spec.name, err)
		}
		if err := db.Exec(spec.ddl).Error; err != nil {
			return fmt.Errorf("create partial index %s: %w", spec.name, err)
		}
	}
	return nil
}

// SeedAdmin creates the first administrator, and nothing else ever.
//
// `mustRotate` marks the account created from the shipped default credential.
// The panel refuses to do anything else while the flag is set, and the flag
// only clears when the password is changed through the normal endpoint. A
// self-hosted server reachable on a network with `admin`/`admin` still set was
// fully open, and unlike a weak user password this one is public: it is what
// the repository, the image and the example config all ship.
//
// It is not a startup error. Refusing to boot would strand an operator who
// cannot reach the panel to change the password — the exact situation the flag
// exists for — so the server runs, reachable, and refuses everything but the
// password change.
// SeedAdmin creates the first administrator, and nothing else ever.
//
// It reports whether it actually created one. The caller logs from that rather
// than from "is the configured password the default": on any installation that
// has been running for more than one boot, seeding returns early and a warning
// printed from the config value alone would claim the panel is open on an
// instance where the admin changed the password months ago.
//
// `mustRotate` marks the account created from the shipped default credential.
// The panel refuses to do anything else while the flag is set, and the flag
// only clears when the password is changed through the normal endpoint. A
// self-hosted server reachable on a network with `admin`/`admin` still set was
// fully open, and unlike a weak user password this one is public: it is what
// the repository, the image and the example config all ship.
//
// It is not a startup error. Refusing to boot would strand an operator who
// cannot reach the panel to change the password — the exact situation the flag
// exists for — so the server runs, reachable, and refuses everything but the
// password change.
func SeedAdmin(db *gorm.DB, username, passwordHash string, mustRotate bool) (bool, error) {
	var count int64
	db.Model(&model.User{}).Where("role = ?", "admin").Count(&count)
	if count > 0 {
		return false, nil // Admin already exists
	}

	admin := &model.User{
		Username:       username,
		PasswordHash:   passwordHash,
		DisplayName:    "Administrator",
		Role:           "admin",
		QuotaBytes:     107374182400, // 100GB for admin
		IsActive:       true,
		MustRotatePass: mustRotate,
	}

	return true, db.Create(admin).Error
}

// FlagAccountsOnDefaultPassword marks every administrator still using the
// shipped `admin`/`admin` credential, and returns how many it marked.
//
// Seeding alone only ever covers a brand-new database, which is the *least*
// exposed case: an installation that has been running for a year is the one
// where nobody was prompted, and it is the one most likely to still have the
// published password. Comparing hashes at boot catches exactly that — one
// bcrypt verification per admin, once, at startup.
//
// Rows already flagged are left alone so a boot does not churn them, and an
// account whose password differs is never touched.
func FlagAccountsOnDefaultPassword(db *gorm.DB, defaultPassword string) (int64, error) {
	if defaultPassword == "" {
		return 0, nil
	}

	var admins []model.User
	if err := db.Where("role = ? AND is_active = ?", "admin", true).Find(&admins).Error; err != nil {
		return 0, err
	}

	var flagged int64
	for i := range admins {
		if admins[i].MustRotatePass {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(admins[i].PasswordHash), []byte(defaultPassword)) != nil {
			continue
		}
		if err := db.Model(&model.User{}).
			Where("id = ? AND must_rotate_pass = ?", admins[i].ID, false).
			Update("must_rotate_pass", true).Error; err != nil {
			continue
		}
		flagged++
	}

	return flagged, nil
}
