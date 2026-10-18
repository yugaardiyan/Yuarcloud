package handler

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// collisionHarness wires a FileHandler onto a real temp directory, so "the file
// is still on disk" is a fact the test reads rather than a claim it makes.
// freshDSN hands out a database nobody else is using. A shared in-memory DSN
// is one database for the whole test binary, so a row written by an earlier
// test is still there for the next one — which turned a legitimate upload into
// a spurious 409 here.
var dbCounter atomic.Int64

func freshDSN(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("file:mem_%s_%d?mode=memory&cache=shared&_pragma=foreign_keys(1)",
		t.Name(), dbCounter.Add(1))
}

func collisionHarness(t *testing.T) (*FileHandler, *gorm.DB, string) {
	t.Helper()
	root := t.TempDir()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{},
		&model.AuditLog{}, &model.UploadSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store, err := storage.NewLocalStorage(&config.Config{
		Storage: config.StorageConfig{
			Type:    "local",
			Local:   config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	gin.SetMode(gin.TestMode)
	return NewFileHandler(db, store, nil), db, root
}

func uploadRequest(t *testing.T, filename string, content []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// Uploading a file whose name is already taken used to destroy the original.
// storage.Write is an os.Create — it truncates — and the unique index on
// files(path) then rejected the insert, at which point the rollback removed the
// path entirely. Net effect: a 500, and the user's data gone.
func TestUploadDoesNotOverwriteAnExistingFile(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	const original = "ORIGINAL CONTENT"
	onDisk := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(onDisk, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	row := model.File{
		ID: uuid.New(), UserID: user, Name: "notes.txt", Path: "notes.txt",
		Size: int64(len(original)), MimeType: "text/plain",
		StorageType: "local", StoragePath: "notes.txt",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = uploadRequest(t, "notes.txt", []byte("REPLACEMENT"))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadFile(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}

	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("the pre-existing file was deleted: %v", err)
	}
	if string(got) != original {
		t.Fatalf("the pre-existing file was overwritten with %q", got)
	}

	var rows int64
	db.Model(&model.File{}).Where("path = ?", "notes.txt").Count(&rows)
	if rows != 1 {
		t.Fatalf("%d rows for notes.txt, want 1", rows)
	}
}

// A fresh name still works — the guard must not be a blanket refusal.
func TestUploadStillAcceptsANewName(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = uploadRequest(t, "fresh.txt", []byte("hello"))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadFile(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}
}

// A trashed row must not block the name forever. Trashing rewrites the row's
// path into its own id-namespaced trash folder, so it no longer holds the live
// path at all — a user who trashes notes.txt and later creates notes.txt again
// must not be refused.
func TestUploadAcceptsAPathOnlyHeldByATrashedRow(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	trashedID := uuid.New()
	trashPath := ".yuarcloud-trash/" + trashedID.String() + "/notes.txt"
	stale := model.File{
		ID: trashedID, UserID: user, Name: "notes.txt",
		Path: trashPath, OriginalPath: "notes.txt", IsTrashed: true,
		Size: 5, StorageType: "local", StoragePath: trashPath,
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = uploadRequest(t, "notes.txt", []byte("hello"))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadFile(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a trashed row must not reserve the name; body %s", w.Code, w.Body.String())
	}
}

// The guard is scoped: a path outside the caller's home is reported as free, so
// the check can never be used to probe what exists beyond the home.
func TestPathTakenIsScopedToTheCaller(t *testing.T) {
	h, db, _ := collisionHarness(t)
	other := quotaUser(t, db, 0, 1000)

	stranger := model.File{
		ID: uuid.New(), UserID: other, Name: "secret.txt", Path: "private/secret.txt",
		Size: 10, StorageType: "local", StoragePath: "private/secret.txt",
	}
	if err := db.Create(&stranger).Error; err != nil {
		t.Fatal(err)
	}

	sc := scope.New("mine")
	if h.pathTaken(sc, "private/secret.txt") {
		t.Fatal("pathTaken reported a path outside the caller's home as occupied")
	}
}
