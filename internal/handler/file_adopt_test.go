package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func adoptionTestHarness(t *testing.T) (*FileHandler, *gorm.DB, storage.StorageBackend, string, uuid.UUID) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	root := t.TempDir()
	store, err := storage.NewLocalStorage(&config.Config{
		Storage: config.StorageConfig{
			Type:  "local",
			Local: config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{
				TempDir: filepath.Join(root, ".yuarcloud-chunks"),
			},
		},
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	userID := quotaUser(t, db, 0, 0)
	return NewFileHandler(db, store, nil), db, store, root, userID
}

func writeTestFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func findAdopted(t *testing.T, db *gorm.DB, p string) model.File {
	t.Helper()
	var f model.File
	if err := db.Where("path = ? AND is_trashed = ?", p, false).First(&f).Error; err != nil {
		t.Fatalf("row for %q: %v", p, err)
	}
	return f
}

func TestAdoptDirectoryEntriesCreatesUnchargedRowsAndSkipsSymlinks(t *testing.T) {
	h, db, store, root, userID := adoptionTestHarness(t)
	_ = h

	writeTestFile(t, root, "readme.txt", "hello")
	writeTestFile(t, root, "Docs/a.txt", "nested")
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.txt", "outside")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	entries, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if err := adoptDirectoryEntries(db, userID, "", nil, entries); err != nil {
		t.Fatalf("adopt root: %v", err)
	}

	readme := findAdopted(t, db, "readme.txt")
	if readme.Size != 5 || readme.IsDir || readme.CountsAgainstQuota() {
		t.Fatalf("readme row = %+v, want size 5, file, uncharged", readme)
	}
	docs := findAdopted(t, db, "Docs")
	if !docs.IsDir || docs.Size != 0 || docs.CountsAgainstQuota() {
		t.Fatalf("Docs row = %+v, want empty uncharged directory", docs)
	}
	if docs.ParentID != nil {
		t.Fatalf("Docs parent_id = %v, want nil at the root", docs.ParentID)
	}

	for _, p := range []string{"link.txt", "escape", "escape/secret.txt"} {
		var n int64
		db.Model(&model.File{}).Where("path = ?", p).Count(&n)
		if n != 0 {
			t.Fatalf("adoption created a row for symlink path %q", p)
		}
	}

	// Adopting the same directory again must be a no-op, not a duplicate-row
	// error from the partial unique index.
	if err := adoptDirectoryEntries(db, userID, "", nil, entries); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	var count int64
	db.Model(&model.File{}).Where("path IN ?", []string{"readme.txt", "Docs"}).Count(&count)
	if count != 2 {
		t.Fatalf("rows after second adoption = %d, want 2", count)
	}

	// Children are linked to the folder row the first listing created.
	childEntries, err := store.List(context.Background(), "Docs")
	if err != nil {
		t.Fatalf("list Docs: %v", err)
	}
	if err := adoptDirectoryEntries(db, userID, "Docs", &docs, childEntries); err != nil {
		t.Fatalf("adopt Docs: %v", err)
	}
	child := findAdopted(t, db, "Docs/a.txt")
	if child.ParentID == nil || *child.ParentID != docs.ID {
		t.Fatalf("Docs/a.txt parent = %v, want %s", child.ParentID, docs.ID)
	}
	if child.CountsAgainstQuota() {
		t.Fatal("nested adopted file was charged to quota")
	}

	// The disk remains the source of truth for an adopted file. Editing it
	// outside the app must show up on the next listing without touching quota.
	writeTestFile(t, root, "readme.txt", "hello again")
	entries, err = store.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list root after edit: %v", err)
	}
	if err := adoptDirectoryEntries(db, userID, "", nil, entries); err != nil {
		t.Fatalf("reconcile root: %v", err)
	}
	readme = findAdopted(t, db, "readme.txt")
	if readme.Size != 11 {
		t.Fatalf("readme size after external edit = %d, want 11", readme.Size)
	}
	if readme.CountsAgainstQuota() {
		t.Fatal("metadata refresh charged an adopted file")
	}
}

func TestAdoptPathRejectsSymlinkEscapeAndBuildsParentChain(t *testing.T) {
	_, db, store, root, userID := adoptionTestHarness(t)

	writeTestFile(t, root, "Docs/Sub/deep.txt", "deep")
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.txt", "outside")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	got, err := adoptPathFrom(db, store, userID, "", nil, "Docs/Sub/deep.txt")
	if err != nil {
		t.Fatalf("adopt path: %v", err)
	}
	if got == nil || got.Path != "Docs/Sub/deep.txt" || got.IsDir {
		t.Fatalf("adopted target = %+v", got)
	}
	docs := findAdopted(t, db, "Docs")
	sub := findAdopted(t, db, "Docs/Sub")
	if sub.ParentID == nil || *sub.ParentID != docs.ID {
		t.Fatalf("Docs/Sub parent = %v, want %s", sub.ParentID, docs.ID)
	}
	deep := findAdopted(t, db, "Docs/Sub/deep.txt")
	if deep.ParentID == nil || *deep.ParentID != sub.ID {
		t.Fatalf("deep parent = %v, want %s", deep.ParentID, sub.ID)
	}

	if _, err := adoptPathFrom(db, store, userID, "", nil, "escape/secret.txt"); err != storage.ErrNotFound {
		t.Fatalf("adopting through a symlink error = %v, want ErrNotFound", err)
	}
}

func TestListFilesAdoptsExistingDirectoryContents(t *testing.T) {
	h, db, _, root, userID := adoptionTestHarness(t)

	writeTestFile(t, root, "readme.txt", "hello")
	writeTestFile(t, root, "Docs/a.txt", "nested")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, userID)
		c.Set(middleware.RoleContextKey, "admin")
		c.Set(middleware.ScopeContextKey, scope.New(""))
	})
	router.GET("/files", h.ListFiles)

	doList := func(target string) (int, []model.FileResponse) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			return w.Code, nil
		}
		var body struct {
			Files []model.FileResponse `json:"files"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode listing: %v (%s)", err, w.Body.String())
		}
		return w.Code, body.Files
	}

	code, files := doList("/files")
	if code != http.StatusOK {
		t.Fatalf("root listing = %d", code)
	}
	if len(files) != 2 {
		t.Fatalf("root listing returned %d files, want 2: %+v", len(files), files)
	}
	byName := make(map[string]model.FileResponse, len(files))
	for _, f := range files {
		byName[f.Name] = f
	}
	docs, ok := byName["Docs"]
	if !ok || docs.ID == uuid.Nil || !docs.IsDir {
		t.Fatalf("Docs row was not adopted into the listing: %+v", byName)
	}
	if _, ok := byName["readme.txt"]; !ok {
		t.Fatalf("readme.txt was not adopted into the listing: %+v", byName)
	}

	// Once nested rows exist, the root query still has to return only the
	// top-level entries; a path-prefix query without the slash check would
	// load and then filter the whole tree on every root listing.
	code, files = doList("/files")
	if code != http.StatusOK || len(files) != 2 {
		t.Fatalf("root listing after child adoption = %d %+v, want 2 entries", code, files)
	}

	code, files = doList("/files?path=Docs&parent_id=" + docs.ID.String())
	if code != http.StatusOK {
		t.Fatalf("Docs listing = %d", code)
	}
	if len(files) != 1 || files[0].Name != "a.txt" {
		t.Fatalf("Docs listing = %+v, want [a.txt]", files)
	}
	child := findAdopted(t, db, "Docs/a.txt")
	if child.ParentID == nil || *child.ParentID != docs.ID {
		t.Fatalf("adopted child parent = %v, want %s", child.ParentID, docs.ID)
	}
}

func TestCopyAdoptsUnimportedDescendantsAndChargesThem(t *testing.T) {
	h, db, _, root, userID := adoptionTestHarness(t)

	writeTestFile(t, root, "Src/nested/secret.txt", "secret")
	if err := db.Create(&model.File{
		UserID: userID, Name: "Src", Path: "Src", IsDir: true,
		MimeType: "inode/directory", StorageType: "local", StoragePath: "Src",
	}).Error; err != nil {
		t.Fatalf("seed Src: %v", err)
	}
	var src model.File
	if err := db.First(&src, "path = ?", "Src").Error; err != nil {
		t.Fatalf("read Src: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/files/:id/copy", func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, userID)
		c.Set(middleware.RoleContextKey, "admin")
		c.Set(middleware.ScopeContextKey, scope.New(""))
		h.CopyFile(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/files/"+src.ID.String()+"/copy",
		bytes.NewBufferString(`{"target_path":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("copy = %d %s", w.Code, w.Body.String())
	}

	if data, err := os.ReadFile(filepath.Join(root, "Src (2)", "nested", "secret.txt")); err != nil || string(data) != "secret" {
		t.Fatalf("copied bytes = %q, err %v", data, err)
	}
	var copied model.File
	if err := db.First(&copied, "path = ?", "Src (2)/nested/secret.txt").Error; err != nil {
		t.Fatalf("copied row: %v", err)
	}
	if !copied.CountsAgainstQuota() {
		t.Fatal("copied unimported descendant was left uncharged")
	}
	var user model.User
	db.First(&user, "id = ?", userID)
	if user.UsedBytes != 6 {
		t.Fatalf("used_bytes after copy = %d, want 6", user.UsedBytes)
	}
}

func TestQuotaReleaseSkipsAdoptedBytes(t *testing.T) {
	_, db, _, _, userID := adoptionTestHarness(t)
	if err := db.Model(&model.User{}).Where("id = ?", userID).Update("used_bytes", 300).Error; err != nil {
		t.Fatalf("seed used bytes: %v", err)
	}

	uncharged := false
	adopted := model.File{
		ID: uuid.New(), UserID: userID, Name: "adopted.bin", Path: "adopted.bin",
		Size: 100, QuotaCharged: &uncharged, StorageType: "local",
	}
	if err := db.Create(&adopted).Error; err != nil {
		t.Fatalf("seed adopted: %v", err)
	}
	uploaded := model.File{
		ID: uuid.New(), UserID: userID, Name: "uploaded.bin", Path: "uploaded.bin",
		Size: 100, StorageType: "local",
	}
	if err := db.Create(&uploaded).Error; err != nil {
		t.Fatalf("seed uploaded: %v", err)
	}

	var stored model.File
	if err := db.First(&stored, "id = ?", uploaded.ID).Error; err != nil {
		t.Fatalf("read uploaded: %v", err)
	}
	if !stored.CountsAgainstQuota() {
		t.Fatal("a row created without an explicit QuotaCharged should default to charged")
	}

	h := NewFileHandler(db, nil, nil)
	h.releaseSubtreeUsage("adopted.bin", -1)
	var user model.User
	db.First(&user, "id = ?", userID)
	if user.UsedBytes != 300 {
		t.Fatalf("used_bytes after releasing adopted file = %d, want 300", user.UsedBytes)
	}

	h.releaseSubtreeUsage("uploaded.bin", -1)
	db.First(&user, "id = ?", userID)
	if user.UsedBytes != 200 {
		t.Fatalf("used_bytes after releasing uploaded file = %d, want 200", user.UsedBytes)
	}
}
