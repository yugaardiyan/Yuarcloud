package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

func authTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func authSvc(t *testing.T) *auth.AuthService {
	t.Helper()
	return auth.NewAuthService("test-secret-that-is-long-enough-32", time.Hour, 24*time.Hour, 4, "yuarcloud")
}

func seedAccount(t *testing.T, db *gorm.DB, username, role string, active bool) model.User {
	t.Helper()
	u := model.User{
		ID: uuid.New(), Username: username, Email: username + "@t.local",
		PasswordHash: "$2a$10$abcdefghijklmnopqrstuv", Role: role, IsActive: active,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	return u
}

func decodePair(t *testing.T, body []byte) *auth.Claims {
	t.Helper()
	var out struct {
		Tokens auth.TokenPair `json:"tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	svc := authSvc(t)
	claims, err := svc.ValidateToken(out.Tokens.AccessToken)
	if err != nil {
		t.Fatalf("access token does not validate: %v", err)
	}
	return claims
}

// The finding this exists for: RefreshAccessToken used to mint a new pair from
// the *presented token's* claims, never consulting the users table — so a
// demoted admin refreshed their way to fresh admin tokens indefinitely.
//
// Note what is and is not refused. A demoted account is still a live account,
// so refreshing succeeds; what must not happen is the admin claim coming back.
func TestRefreshDoesNotReplayADemotedAdminClaim(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)

	victim := seedAccount(t, db, "victim", "admin", true)
	pair, err := svc.GenerateTokenPair(victim.ID, victim.Username, "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if claims := decodePair(t, mustJSON(t, pair)); claims.Role != "admin" {
		t.Fatalf("baseline token role = %q, want admin", claims.Role)
	}

	// Demoted while holding a live refresh token.
	db.Model(&model.User{}).Where("id = ?", victim.ID).Update("role", "user")

	next, err := svc.RefreshAccessTokenWithDB(db, pair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := decodePair(t, mustJSON(t, next)).Role; got == "admin" {
		t.Fatal("a demoted admin refreshed into a fresh admin token; the old claim was replayed")
	}
}

func TestRefreshRefusesADeletedAccount(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)

	victim := seedAccount(t, db, "gone", "admin", true)
	pair, _ := svc.GenerateTokenPair(victim.ID, victim.Username, "admin", 0)

	db.Delete(&model.User{}, "id = ?", victim.ID)

	if _, err := svc.RefreshAccessTokenWithDB(db, pair.RefreshToken); err == nil {
		t.Fatal("a deleted account refreshed a token successfully")
	}
}

func TestRefreshRefusesADeactivatedAccount(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)

	u := seedAccount(t, db, "sleepy", "user", true)
	pair, _ := svc.GenerateTokenPair(u.ID, u.Username, "user", 0)

	db.Model(&model.User{}).Where("id = ?", u.ID).Update("is_active", false)

	if _, err := svc.RefreshAccessTokenWithDB(db, pair.RefreshToken); err == nil {
		t.Fatal("a deactivated account refreshed a token successfully")
	}
}

// A refresh must also pick up the *current* role rather than replaying the old
// claim, so a promotion takes effect without a re-login.
func TestRefreshTakesTheRoleFromTheRow(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)

	u := seedAccount(t, db, "promoted", "user", true)
	pair, _ := svc.GenerateTokenPair(u.ID, u.Username, "user", 0)

	db.Model(&model.User{}).Where("id = ?", u.ID).Update("role", "admin")

	next, err := svc.RefreshAccessTokenWithDB(db, pair.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := decodePair(t, mustJSON(t, next)).Role; got != "admin" {
		t.Fatalf("refreshed role = %q, want admin — the claim was replayed instead of re-read", got)
	}
}

// An access token must still not be exchangeable for a refresh pair.
func TestRefreshStillRejectsAnAccessToken(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)
	u := seedAccount(t, db, "swap", "user", true)

	pair, _ := svc.GenerateTokenPair(u.ID, u.Username, "user", 0)
	if _, err := svc.RefreshAccessTokenWithDB(db, pair.AccessToken); err == nil {
		t.Fatal("an access token was exchanged for a refresh pair")
	}
}

// A disabled account must not get a scope, so its existing tokens stop working.
func TestScopeMiddlewareRejectsADeactivatedAccount(t *testing.T) {
	db := authTestDB(t)
	svc := authSvc(t)
	u := seedAccount(t, db, "off", "user", true)

	pair, _ := svc.GenerateTokenPair(u.ID, u.Username, "user", 0)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(svc), middleware.ScopeMiddleware(db))
	r.GET("/api/files", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	call := func() int {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/files", bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := call(); code != http.StatusOK {
		t.Fatalf("active account: %d, want 200", code)
	}

	db.Model(&model.User{}).Where("id = ?", u.ID).Update("is_active", false)
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("deactivated account: %d, want 401", code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tokens": v})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
