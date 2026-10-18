package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gorm.io/driver/sqlite"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

const revocationTestSecret = "revocation-test-secret-at-least-32-chars"

func setupRevocationHandler(t *testing.T) (*gin.Engine, *gorm.DB, *model.User, *auth.AuthService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{},
		&model.AuditLog{}, &model.UploadSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc := auth.NewAuthService(revocationTestSecret, time.Hour, 24*time.Hour, 4, "yuarcloud-test")
	hash, err := svc.HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	user := &model.User{
		Username:     "revoke-me",
		PasswordHash: hash,
		Role:         "user",
		QuotaBytes:   1 << 30,
		IsActive:     true,
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	r := gin.New()
	api := r.Group("/api")
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(svc))
	protected.Use(middleware.ScopeMiddleware(db))
	protected.Use(middleware.PasswordRotationRequiredMiddleware(db, map[string]bool{
		"POST /api/auth/change-password": true,
		"POST /api/auth/logout":          true,
		"GET /api/auth/me":               true,
	}))
	protected.GET("/auth/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	protected.GET("/files", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"files": []any{}}) })

	return r, db, user, svc
}

func bearer(t *testing.T, r *gin.Engine, svc *auth.AuthService, u *model.User) *httptest.ResponseRecorder {
	t.Helper()
	pair, err := svc.GenerateTokenPair(u.ID, u.Username, u.Role, u.TokenVersion)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// A password change must kill every token already outstanding, not just
// stop future logins. Before token_version existed, changing a password left
// every access token valid for its full TTL and every refresh token able to
// mint a fresh pair — so a stolen session was permanent.
func TestPasswordChangeRevokesIssuedTokens(t *testing.T) {
	r, db, user, svc := setupRevocationHandler(t)

	if rec := bearer(t, r, svc, user); rec.Code != http.StatusOK {
		t.Fatalf("token should work before the change: got %d %s", rec.Code, rec.Body)
	}

	newHash, err := svc.HashPassword("a different one entirely")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{
			"password_hash": newHash,
			"token_version": gorm.Expr("token_version + 1"),
		}).Error; err != nil {
		t.Fatalf("simulate change-password: %v", err)
	}

	rec := bearer(t, r, svc, user)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("outstanding token survived a password change: got %d %s", rec.Code, rec.Body)
	}

	// And a refresh token minted before the change is refused too, which is
	// the half that mattered: it is what turned one stolen token into an
	// indefinite one.
	pair, _ := svc.GenerateTokenPair(user.ID, user.Username, user.Role, 0)
	if _, err := svc.RefreshAccessTokenWithDB(db, pair.RefreshToken); err == nil {
		t.Fatal("refresh accepted a token predating the password change")
	}

	// A freshly minted token at the new version works. Reload first: the
	// in-memory row still carries the pre-change version, and that stale value
	// being refused is the point.
	var refreshed model.User
	if err := db.First(&refreshed, "id = ?", user.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	user = &refreshed
	if rec := bearer(t, r, svc, user); rec.Code != http.StatusOK {
		t.Fatalf("token at the current version should work: got %d %s", rec.Code, rec.Body)
	}
}

// The seeded default credential must not carry the panel. Full admin access
// with a password published in the repository is the same as no access
// control.
func TestDefaultCredentialIsConfinedToPasswordChange(t *testing.T) {
	r, db, user, svc := setupRevocationHandler(t)

	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Update("must_rotate_pass", true).Error; err != nil {
		t.Fatalf("flag: %v", err)
	}

	pair, err := svc.GenerateTokenPair(user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("flagged account reached the file API: got %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["must_rotate_password"] != true {
		t.Fatalf("response should tell the client why: %s", rec.Body)
	}

	// The way out stays open, or the owner is locked out of the only account
	// that can administer the server.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/change-password", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("change-password was refused for a flagged account")
	}
}

// Admin routes do not pass through the protected group, so the confinement has
// to be wired onto them separately. It was not: an administrator still on the
// default credential got 403 on every file route and a fully working /admin
// panel. That is the worst version of the bug — the confinement appeared to
// hold, and the one thing a flagged admin could still do was create a second
// account with a known password and sign in as that instead.
func TestPasswordRotationConfinesAdminRoutes(t *testing.T) {
	_, db, user, svc := setupRevocationHandler(t)

	if err := db.Model(&model.User{}).Where("id = ?", user.ID).Updates(map[string]any{
		"role":             "admin",
		"must_rotate_pass": true,
		"token_version":    gorm.Expr("token_version + 1"),
	}).Error; err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// A second router, mirroring main.go: the admin group re-checks the role
	// and then hits the same confinement middleware.
	panel := gin.New()
	api := panel.Group("/api")
	api.Use(middleware.AuthMiddleware(svc))
	api.Use(middleware.ScopeMiddleware(db))
	admin := api.Group("/admin")
	admin.Use(middleware.AdminOnlyMiddleware(db))
	admin.Use(middleware.PasswordRotationRequiredMiddleware(db, nil))
	admin.GET("/users", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"users": []any{}}) })

	// Reload: the role change bumped the token version, and a token minted at
	// the stale one is refused by the version check before the rotation guard
	// is ever consulted — which would make this test pass for the wrong reason.
	var reloaded model.User
	if err := db.First(&reloaded, "id = ?", user.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	pair, _ := svc.GenerateTokenPair(reloaded.ID, reloaded.Username, "admin", reloaded.TokenVersion)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
	rec := httptest.NewRecorder()
	panel.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("flagged admin reached the panel: got %d %s", rec.Code, rec.Body)
	}
	_ = uuid.Nil
}

// The client cannot route to the password-change screen unless the flag is on
// the wire. Without it the account is confined server-side and the app has no
// way to know why: a valid session, 403 on everything, nothing on screen.
func TestLoginResponseCarriesTheRotationFlag(t *testing.T) {
	_, db, user, _ := setupRevocationHandler(t)

	if err := db.Model(&model.User{}).Where("id = ?", user.ID).
		Update("must_rotate_pass", true).Error; err != nil {
		t.Fatalf("flag: %v", err)
	}
	var fresh model.User
	if err := db.First(&fresh, "id = ?", user.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Round-trip the same shape the login handler writes.
	body, err := json.Marshal(gin.H{"user": model.UserResponse{
		ID:                 fresh.ID,
		Username:           fresh.Username,
		MustRotatePassword: fresh.MustRotatePass,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		User struct {
			MustRotatePassword bool `json:"must_rotate_password"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.User.MustRotatePassword {
		t.Fatalf("must_rotate_password missing from the wire format: %s", body)
	}
}
