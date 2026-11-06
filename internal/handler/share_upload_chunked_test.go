package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// shareChunkedHarness mirrors the public share routes in cmd/server/main.go,
// with a write link ("write") and a read-only one ("read") on the same folder.
func shareChunkedHarness(t *testing.T) (*gin.Engine, *gorm.DB, storage.StorageBackend, model.User, model.File, model.File) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.File{}, &model.ShareLink{}, &model.ShareLinkItem{},
		&model.UploadSession{},
	); err != nil {
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
	other := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "Other", Path: "Other",
		IsDir: true, MimeType: "inode/directory", StorageType: "local", StoragePath: "Other",
	}
	for _, dir := range []*model.File{&shared, &other} {
		if err := db.Create(dir).Error; err != nil {
			t.Fatalf("seed dir %s: %v", dir.Name, err)
		}
	}

	for _, link := range []model.ShareLink{
		{ID: uuid.New(), UserID: owner.ID, FileID: shared.ID, Token: "write", IsActive: true, Permission: "write"},
		{ID: uuid.New(), UserID: owner.ID, FileID: shared.ID, Token: "read", IsActive: true, Permission: "read"},
	} {
		if err := db.Create(&link).Error; err != nil {
			t.Fatalf("seed link %s: %v", link.Token, err)
		}
	}

	svc := share.NewShareService(db, "", 0, 0, false, 0)
	h := NewShareHandler(db, svc, store)
	h.SetFileHandler(NewFileHandler(db, store, nil))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/shares/:token/upload-limits", h.ShareUploadLimits)
	router.POST("/api/shares/:token/upload/init", h.InitShareUpload)
	router.POST("/api/shares/:token/upload/chunk/:uploadId/:chunkIndex", h.UploadShareChunk)
	router.POST("/api/shares/:token/upload/complete/:uploadId", h.CompleteShareUpload)
	router.GET("/api/shares/:token/upload/status/:uploadId", h.GetShareUploadStatus)
	router.DELETE("/api/shares/:id/upload/:uploadId", h.CancelShareUpload)

	return router, db, store, owner, shared, other
}

func postShareJSON(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(http.MethodPost, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func postShareChunk(t *testing.T, router *gin.Engine, token, uploadID string, index int, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("chunk", "blob")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/shares/%s/upload/chunk/%s/%d", token, uploadID, index), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestShareChunkedUploadWritesIntoSharedFolder(t *testing.T) {
	router, db, store, owner, shared, _ := shareChunkedHarness(t)

	content := []byte("0123456789") // 10 bytes, 3 chunks of 4/4/2
	init := postShareJSON(t, router, "/api/shares/write/upload/init", map[string]any{
		"filename":     "report.bin",
		"total_size":   len(content),
		"chunk_size":   4,
		"total_chunks": 3,
		"parent_id":    shared.ID.String(),
	})
	if init.Code != http.StatusOK {
		t.Fatalf("init: status %d, body %s", init.Code, init.Body.String())
	}

	var started struct {
		UploadID    string `json:"upload_id"`
		ChunkSize   int64  `json:"chunk_size"`
		TotalChunks int    `json:"total_chunks"`
	}
	if err := json.Unmarshal(init.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode init: %v", err)
	}
	if started.UploadID == "" || started.ChunkSize != 4 || started.TotalChunks != 3 {
		t.Fatalf("unexpected init response: %+v", started)
	}

	for i := 0; i < started.TotalChunks; i++ {
		start := int64(i) * started.ChunkSize
		end := start + started.ChunkSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		w := postShareChunk(t, router, "write", started.UploadID, i, content[start:end])
		if w.Code != http.StatusOK {
			t.Fatalf("chunk %d: status %d, body %s", i, w.Code, w.Body.String())
		}
	}

	done := postShareJSON(t, router, "/api/shares/write/upload/complete/"+started.UploadID, nil)
	if done.Code != http.StatusCreated {
		t.Fatalf("complete: status %d, body %s", done.Code, done.Body.String())
	}

	var file model.File
	if err := db.First(&file, "path = ?", "Shared/report.bin").Error; err != nil {
		t.Fatalf("file row missing: %v", err)
	}
	if file.UserID != owner.ID {
		t.Fatalf("upload charged to %s, want owner %s", file.UserID, owner.ID)
	}
	if file.ParentID == nil || *file.ParentID != shared.ID {
		t.Fatalf("parent id = %v, want %s", file.ParentID, shared.ID)
	}
	if file.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", file.Size, len(content))
	}

	reader, err := store.Read(context.Background(), "Shared/report.bin")
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read bytes: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("stored bytes = %q, want %q", got, content)
	}
}

func TestShareChunkedUploadRefusesOutsideScope(t *testing.T) {
	router, _, _, _, _, other := shareChunkedHarness(t)

	// A write link only grants its own folder. Naming a sibling folder as the
	// parent must not create anything there.
	w := postShareJSON(t, router, "/api/shares/write/upload/init", map[string]any{
		"filename":     "escape.bin",
		"total_size":   10,
		"chunk_size":   4,
		"total_chunks": 3,
		"parent_id":    other.ID.String(),
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestShareChunkedUploadRefusesReadOnlyLink(t *testing.T) {
	router, _, _, _, _, _ := shareChunkedHarness(t)

	w := postShareJSON(t, router, "/api/shares/read/upload/init", map[string]any{
		"filename":     "nope.bin",
		"total_size":   10,
		"chunk_size":   4,
		"total_chunks": 3,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", w.Code, w.Body.String())
	}
}