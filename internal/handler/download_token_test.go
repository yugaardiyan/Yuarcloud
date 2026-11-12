package handler

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

const downloadTestSecret = "download-test-secret-at-least-32-characters"

type downloadHarness struct {
	router *gin.Engine
	db     *gorm.DB
	svc    *auth.AuthService
	user   *model.User
	root   string
}

// newDownloadHarness wires the same two route groups main.go uses: the
// authenticated one that mints tokens and the token-capable one that serves
// bytes, so a test exercises the real middleware order.
func newDownloadHarness(t *testing.T) *downloadHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

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
			Type:    "local",
			Local:   config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	svc := auth.NewAuthService(downloadTestSecret, time.Hour, 24*time.Hour, 4, "yuarcloud-test")
	user := &model.User{
		Username:     "downloader",
		Email:        "downloader@t.local",
		PasswordHash: "$2a$04$abcdefghijklmnopqrstuv",
		Role:         "user",
		IsActive:     true,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	files := NewFileHandler(db, store, svc)

	r := gin.New()
	api := r.Group("/api")

	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(svc))
	protected.Use(middleware.ScopeMiddleware(db))
	protected.Use(middleware.PasswordRotationRequiredMiddleware(db, map[string]bool{}))
	{
		protected.POST("/files/:id/download-token", files.IssueDownloadToken)
		protected.POST("/files/download-token", files.IssueBatchDownloadToken)
		// Stand-in for any other authenticated route, to prove a download
		// token cannot be spent here.
		protected.GET("/files", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"files": []any{}}) })
	}

	downloads := api.Group("/files", middleware.AuthDownloadMiddleware(svc))
	downloads.Use(middleware.ScopeMiddleware(db))
	downloads.Use(middleware.PasswordRotationRequiredMiddleware(db, map[string]bool{}))
	{
		downloads.GET("/:id/download", files.DownloadFile)
		downloads.GET("/:id/download-zip", files.DownloadFolder)
		downloads.GET("/download-zip", files.DownloadFilesZipToken)
	}

	return &downloadHarness{router: r, db: db, svc: svc, user: user, root: root}
}

func (h *downloadHarness) seedFile(t *testing.T, name string, content []byte) *model.File {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, name), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	file := &model.File{
		UserID:   h.user.ID,
		Name:     name,
		Path:     name,
		Size:     int64(len(content)),
		MimeType: "text/plain",
	}
	if err := h.db.Create(file).Error; err != nil {
		t.Fatalf("create row %s: %v", name, err)
	}
	return file
}

func (h *downloadHarness) seedFolder(t *testing.T, name string) *model.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.root, name), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	folder := &model.File{UserID: h.user.ID, Name: name, Path: name, IsDir: true}
	if err := h.db.Create(folder).Error; err != nil {
		t.Fatalf("create folder %s: %v", name, err)
	}
	return folder
}

func (h *downloadHarness) bearer(t *testing.T) string {
	t.Helper()
	pair, err := h.svc.GenerateTokenPair(h.user.ID, h.user.Username, h.user.Role, h.user.TokenVersion)
	if err != nil {
		t.Fatalf("mint access token: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

func (h *downloadHarness) mint(t *testing.T, path string, body []byte) string {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	req.Header.Set("Authorization", h.bearer(t))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint %s -> %d: %s", path, rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mint response: %v (%s)", err, rec.Body.String())
	}
	if out.Token == "" {
		t.Fatalf("mint %s returned no token: %s", path, rec.Body.String())
	}
	return out.Token
}

func (h *downloadHarness) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func zipNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

// A URL a browser can follow needs to work with no Authorization header at
// all — that is the point of the token — and to serve exactly the file it was
// minted for.
func TestDownloadTokenServesItsFileWithoutAHeader(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "hello.txt", []byte("hello world"))

	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	rec := h.get(t, "/api/files/"+file.ID.String()+"/download?token="+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("download -> %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello world" {
		t.Fatalf("body = %q, want %q", got, "hello world")
	}
}

func TestDownloadTokenIsRefusedWithoutOrForAnotherFile(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "mine.txt", []byte("mine"))
	other := h.seedFile(t, "other.txt", []byte("other"))

	if rec := h.get(t, "/api/files/"+file.ID.String()+"/download"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("download without a token -> %d, want 401", rec.Code)
	}

	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)
	if rec := h.get(t, "/api/files/"+other.ID.String()+"/download?token="+token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token replayed against another file -> %d, want 401", rec.Code)
	}
}

// A token that rides in a URL must not be spendable as a session credential.
func TestDownloadTokenIsNotAcceptedAsABearerToken(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "hello.txt", []byte("hello"))

	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("download token accepted as a bearer token -> %d, want 401", rec.Code)
	}
}

func TestBatchDownloadTokenStreamsOneArchive(t *testing.T) {
	h := newDownloadHarness(t)
	one := h.seedFile(t, "one.txt", []byte("one"))
	two := h.seedFile(t, "two.txt", []byte("two"))

	body, err := json.Marshal(map[string]any{"ids": []uuid.UUID{one.ID, two.ID}})
	if err != nil {
		t.Fatal(err)
	}
	token := h.mint(t, "/api/files/download-token", body)

	rec := h.get(t, "/api/files/download-zip?token="+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch zip -> %d: %s", rec.Code, rec.Body.String())
	}
	names := zipNames(t, rec.Body.Bytes())
	for _, want := range []string{"one.txt", "two.txt"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("archive is missing %q (has %v)", want, names)
		}
	}
}

func TestBatchDownloadTokenRequiresAToken(t *testing.T) {
	h := newDownloadHarness(t)
	one := h.seedFile(t, "one.txt", []byte("one"))

	// The old POST route still exists in main; the GET route must not become
	// an unauthenticated archive of anything.
	if rec := h.get(t, "/api/files/download-zip"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("batch zip without a token -> %d, want 401", rec.Code)
	}

	// A single-item token names no set, and the batch route has nothing to
	// bind it to, so it must not turn into "the whole selection".
	token := h.mint(t, "/api/files/"+one.ID.String()+"/download-token", nil)
	if rec := h.get(t, "/api/files/download-zip?token="+token); rec.Code == http.StatusOK {
		t.Fatalf("single-item token produced a batch archive: %s", rec.Body.String())
	}
}

func TestFolderDownloadTokenStreamsTheFolder(t *testing.T) {
	h := newDownloadHarness(t)
	folder := h.seedFolder(t, "Docs")
	h.seedFile(t, "Docs/inside.txt", []byte("inside"))

	token := h.mint(t, "/api/files/"+folder.ID.String()+"/download-token", nil)

	rec := h.get(t, "/api/files/"+folder.ID.String()+"/download-zip?token="+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("folder zip -> %d: %s", rec.Code, rec.Body.String())
	}
	names := zipNames(t, rec.Body.Bytes())
	if len(names) != 1 || names[0] != "inside.txt" {
		t.Fatalf("archive entries = %v, want [inside.txt]", names)
	}
}

// Resume is the other half of a browser-handled download: without Range the
// download manager has to restart from zero every time the connection drops.
func TestDownloadTokenServesRanges(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "ten.txt", []byte("0123456789"))
	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/files/"+file.ID.String()+"/download?token="+token, nil)
	req.Header.Set("Range", "bytes=4-6")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range download -> %d, want 206 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "456" {
		t.Fatalf("body = %q, want %q", got, "456")
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 4-6/10" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
}

func TestDownloadTokenServesUnsatisfiableRange(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "ten.txt", []byte("0123456789"))
	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/files/"+file.ID.String()+"/download?token="+token, nil)
	req.Header.Set("Range", "bytes=90-100")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-range download -> %d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
		t.Errorf("Content-Range = %q", got)
	}
}

// Deleting a password or bumping the version must kill a URL already handed
// out, not only future logins.
func TestDownloadTokenDiesOnTokenVersionBump(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "hello.txt", []byte("hello"))
	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	if err := h.db.Model(&model.User{}).Where("id = ?", h.user.ID).
		Update("token_version", gorm.Expr("token_version + 1")).Error; err != nil {
		t.Fatalf("bump token version: %v", err)
	}

	if rec := h.get(t, "/api/files/"+file.ID.String()+"/download?token="+token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("download after token-version bump -> %d, want 401", rec.Code)
	}
}

// A link must not keep serving a file that was thrown away after the link was
// minted.
func TestDownloadTokenStopsAtTheTrash(t *testing.T) {
	h := newDownloadHarness(t)
	file := h.seedFile(t, "secret.txt", []byte("secret"))
	token := h.mint(t, "/api/files/"+file.ID.String()+"/download-token", nil)

	if err := h.db.Model(&model.File{}).Where("id = ?", file.ID).
		Update("is_trashed", true).Error; err != nil {
		t.Fatalf("trash: %v", err)
	}

	if rec := h.get(t, "/api/files/"+file.ID.String()+"/download?token="+token); rec.Code != http.StatusNotFound {
		t.Fatalf("download of a trashed file -> %d, want 404", rec.Code)
	}
}
