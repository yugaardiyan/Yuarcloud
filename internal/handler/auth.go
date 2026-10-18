package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

type AuthHandler struct {
	authService *auth.AuthService
	db          *gorm.DB
}

func NewAuthHandler(authService *auth.AuthService, db *gorm.DB) *AuthHandler {
	return &AuthHandler{
		authService: authService,
		db:          db,
	}
}

type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Every failure answers the same status and the same body, and the
	// password is compared before anything else is branched on.
	//
	// The disabled-account check used to run first and returned 403 "account
	// disabled", so any garbage password against any username was enough to
	// confirm the account exists. It also skipped bcrypt entirely while a real
	// username paid for it — cost 12 is a few hundred milliseconds, which is a
	// timing oracle separating "no such user" from "wrong password" without any
	// error message at all.
	var user model.User
	lookupErr := h.db.Where("username = ?", req.Username).First(&user).Error

	if lookupErr != nil {
		// Burn the same work a real user costs, so the two paths are
		// indistinguishable by how long they take.
		h.authService.DummyPasswordCheck(req.Password)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := h.authService.CheckPassword(req.Password, user.PasswordHash); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if !user.IsActive {
		// Right password, disabled account: the only case that may differ,
		// because at this point the caller has already proved they own it.
		c.JSON(http.StatusForbidden, gin.H{"error": "account disabled"})
		return
	}

	tokenPair, err := h.authService.GenerateTokenPair(user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate tokens"})
		return
	}

	// Update last login
	h.db.Model(&user).Update("last_login_at", time.Now())

	// Log audit
	logAudit(h.db, c, user.ID, "login", "user", &user.ID, "User logged in")

	c.JSON(http.StatusOK, gin.H{
		"user": model.UserResponse{
			ID:                 user.ID,
			Username:           user.Username,
			Email:              user.Email,
			DisplayName:        user.DisplayName,
			AvatarURL:          user.AvatarURL,
			Role:               user.Role,
			QuotaBytes:         user.QuotaBytes,
			UsedBytes:          user.UsedBytes,
			HomePath:           user.HomePath,
			IsActive:           user.IsActive,
			MustRotatePassword: user.MustRotatePass,
			CreatedAt:          user.CreatedAt,
		},
		"tokens": tokenPair,
	})
}

func (h *AuthHandler) Refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tokenPair, err := h.authService.RefreshAccessTokenWithDB(h.db, req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"tokens": tokenPair})
}

func (h *AuthHandler) Me(c *gin.Context) {
	userID, exists := middleware.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var user model.User
	if err := h.db.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user": model.UserResponse{
			ID:                 user.ID,
			Username:           user.Username,
			Email:              user.Email,
			DisplayName:        user.DisplayName,
			AvatarURL:          user.AvatarURL,
			Role:               user.Role,
			QuotaBytes:         user.QuotaBytes,
			UsedBytes:          user.UsedBytes,
			HomePath:           user.HomePath,
			IsActive:           user.IsActive,
			MustRotatePassword: user.MustRotatePass,
			CreatedAt:          user.CreatedAt,
		},
	})
}

func (h *AuthHandler) ChangePassword(c *gin.Context) {
	userID, exists := middleware.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req struct {
		CurrentPassword string `json:"current_password" binding:"required"`
		NewPassword     string `json:"new_password" binding:"required,min=8"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user model.User
	if err := h.db.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if err := h.authService.CheckPassword(req.CurrentPassword, user.PasswordHash); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password incorrect"})
		return
	}

	newHash, err := h.authService.HashPassword(req.NewPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
		return
	}

	// The token version bump is the point of this endpoint. Changing a password
	// used to rotate the hash and return 200 while every token already issued
	// carried on working — 24h of access, 168h of refresh, and a refresh that
	// mints a fresh pair, so indefinite. If someone else had the session, the
	// owner's remedy did nothing.
	if err := h.db.Model(&user).Updates(map[string]any{
		"password_hash": newHash,
		// gorm.Expr so the column is incremented in SQL rather than read,
		// written and incremented here, which would lose a concurrent bump.
		"token_version": gorm.Expr("token_version + 1"),
		// The way out of PasswordRotationRequiredMiddleware, and the only way
		// in. Cleared here rather than anywhere else so no other code path can
		// unflag an account without a real password change behind it.
		"must_rotate_pass": false,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
		return
	}

	// A fresh pair, at the new version.
	//
	// The bump above invalidated the very tokens this request arrived on, and
	// nothing could hand the caller a usable replacement: the access token was
	// dead, and refreshing it failed for the same reason. So the session the
	// user just proved they own ended at the moment they secured it — the app
	// 401'd, the interceptor cleared the tokens, and the browser landed back
	// on the login screen. Issuing the new pair here is what makes "your
	// password changed" not also mean "you have been signed out".
	user.TokenVersion++
	tokenPair, err := h.authService.GenerateTokenPair(user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		// The password really did change. Failing to mint a replacement is not a
		// reason to report the change as failed — the user would retry against
		// a credential that no longer works and conclude the opposite.
		c.JSON(http.StatusOK, gin.H{"message": "password updated", "tokens": nil})
		return
	}

	logAudit(h.db, c, userID, "password_change", "user", &userID, "Password changed")

	c.JSON(http.StatusOK, gin.H{"message": "password updated", "tokens": tokenPair})
}

func (h *AuthHandler) Logout(c *gin.Context) {
	// Client-side token removal, but we can log audit
	userID, _ := middleware.GetUserID(c)
	if userID != uuid.Nil {
		logAudit(h.db, c, userID, "logout", "user", &userID, "User logged out")
	}
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

// uuidPtr parses a UUID string and returns a pointer to it.
// Returns nil if the string is not a valid UUID.
func uuidPtr(s string) *uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}
