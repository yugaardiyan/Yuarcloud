package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

func shareAdoptionHarness(t *testing.T) (*gin.Engine, *gorm.DB, storage.StorageBackend, string, model.User, model.File) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.ShareLink{}); err != nil {
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

	owner := model.User{
		ID: uuid.New(), Username: "owner", Email: "owner@t.local",
		PasswordHash: "x", Role: "user", IsActive: true,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	shared := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "Shared", Path: "Shared",
		IsDir: true, MimeType: "inode/directory", StorageType: "local", StoragePath: "Shared",
	}
	deep := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "Deep", Path: "Deep",
		IsDir: true, MimeType: "inode/directory", StorageType: "local", StoragePath: "Deep",
	}
	for _, rootRow := range []*model.File{&shared, &deep} {
		if err := db.Create(rootRow).Error; err != nil {
			t.Fatalf("seed root %s: %v", rootRow.Name, err)
		}
	}
	for _, link := range []model.ShareLink{
		{ID: uuid.New(), UserID: owner.ID, FileID: shared.ID, Token: "tok", IsActive: true, Permission: "read"},
		{ID: uuid.New(), UserID: owner.ID, FileID: deep.ID, Token: "deep", IsActive: true, Permission: "read"},
	} {
		if err := db.Create(&link).Error; err != nil {
			t.Fatalf("seed link %s: %v", link.Token, err)
		}
	}

	writeTestFile(t, root, "Shared/a.txt", "a")
	writeTestFile(t, root, "Shared/Sub/b.txt", "b")
	writeTestFile(t, root, "Deep/A/B.txt", "deep")
	outside := t.TempDir()
	writeTestFile(t, outside, "secret.txt", "outside")
	if err := os.Symlink(outside, filepath.Join(root, "Shared", "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	svc := share.NewShareService(db, "", 0, 0, false, 0)
	h := NewShareHandler(db, svc, store)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/shares/:token/children", h.ListSharedFolder)
	router.GET("/api/shares/:token/download", h.DownloadSharedFile)

	return router, db, store, root, owner, shared
}

func getShareChildren(t *testing.T, router *gin.Engine, token, query string) (int, []model.FileResponse) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/shares/"+token+"/children"+query, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}

	var body struct {
		Items []model.FileResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return w.Code, body.Items
}

func TestSharedFolderAdoptsExistingChildren(t *testing.T) {
	router, db, _, _, owner, shared := shareAdoptionHarness(t)

	code, items := getShareChildren(t, router, "tok", "")
	if code != http.StatusOK {
		t.Fatalf("root listing = %d", code)
	}
	got := names(items)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "Sub" || got[1] != "a.txt" {
		t.Fatalf("root listing = %v, want [Sub a.txt]", got)
	}

	for _, p := range []string{"Shared/a.txt", "Shared/Sub"} {
		var row model.File
		if err := db.First(&row, "path = ?", p).Error; err != nil {
			t.Fatalf("row %s: %v", p, err)
		}
		if row.UserID != owner.ID {
			t.Fatalf("%s owner = %s, want share owner %s", p, row.UserID, owner.ID)
		}
		if row.CountsAgainstQuota() {
			t.Fatalf("%s adopted through a share was charged to quota", p)
		}
	}
	var adoptedRoot model.File
	db.First(&adoptedRoot, "path = ?", "Shared/Sub")
	if adoptedRoot.ParentID == nil || *adoptedRoot.ParentID != shared.ID {
		t.Fatalf("Shared/Sub parent = %v, want %s", adoptedRoot.ParentID, shared.ID)
	}

	// A symlink must not become a browsable child or a way out of the root.
	for _, item := range items {
		if item.Name == "escape" {
			t.Fatalf("symlink was listed as a shared folder child: %v", names(items))
		}
	}
}

func TestSharedFolderDeepLinkAdoptsChain(t *testing.T) {
	router, _, _, _, _, _ := shareAdoptionHarness(t)

	code, items := getShareChildren(t, router, "deep", "?path=Deep%2FA")
	if code != http.StatusOK {
		t.Fatalf("deep listing = %d", code)
	}
	if got := names(items); len(got) != 1 || got[0] != "B.txt" {
		t.Fatalf("deep listing = %v, want [B.txt]", got)
	}
}

func TestFolderZipWalksDiskIncludingUnadoptedFiles(t *testing.T) {
	_, _, store, root, _, _ := shareAdoptionHarness(t)
	writeTestFile(t, root, "Shared/Sub/extra.txt", "extra")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeFolderZip(context.Background(), store, zw, "Shared")
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	var got []string
	for _, f := range zr.File {
		got = append(got, f.Name)
	}
	sort.Strings(got)
	want := []string{"Sub/b.txt", "Sub/extra.txt", "a.txt"}
	if len(got) != len(want) {
		t.Fatalf("zip entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zip entries = %v, want %v", got, want)
		}
	}

	// The helper reads through the archive; make sure the content is intact.
	for _, f := range zr.File {
		if f.Name != "Sub/extra.txt" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry: %v", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(data) != "extra" {
			t.Fatalf("zip entry content = %q, err %v", data, err)
		}
		return
	}
	t.Fatal("Sub/extra.txt missing from zip")
}
