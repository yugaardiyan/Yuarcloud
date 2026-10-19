package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"gorm.io/gorm"
)

// Account administration: listing, creation, and every edit that changes what an
// account can do — its home directory, its quota, its role, its password.
//
// Grouped because these are the endpoints that decide who may reach the rest of
// the system, and the guards between them (never close the last door into the
// panel, never hand out a home directory that does not exist) only make sense
// read together with the operations they protect.

func (h *AdminHandler) GetUsers(c *gin.Context) {
	page := 1
	pageSize := 20
	search := c.Query("search")
	role := c.Query("role")

	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	ctx := c.Request.Context()
	users, total, err := h.adminService.GetUsers(ctx, page, pageSize, search, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get users"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"users":    users,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}

func (h *AdminHandler) GetUser(c *gin.Context) {
	userID := c.Param("id")
	parsed, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	ctx := c.Request.Context()
	user, err := h.adminService.GetUser(ctx, parsed)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	c.JSON(http.StatusOK, user)
}

func (h *AdminHandler) CreateUser(c *gin.Context) {
	var req struct {
		Username    string `json:"username" binding:"required,min=3,max=50"`
		Email       string `json:"email" binding:"required,email"`
		Password    string `json:"password" binding:"required,min=8"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		QuotaBytes  int64  `json:"quota_bytes"`
		// HomePath pins the new account to a subtree. Empty means the storage
		// root. Pointing several accounts at the same folder is how a shared
		// folder is set up; pointing one at a folder of its own makes it
		// private to them.
		HomePath string `json:"home_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Role == "" {
		req.Role = "user"
	}
	if req.QuotaBytes == 0 {
		req.QuotaBytes = 10737418240 // 10GB default
	}

	homePath, ok := h.resolveHomePath(c, req.HomePath)
	if !ok {
		return
	}

	// A taken username or email is the caller's mistake, not an outage, and the
	// unique index used to answer it as a 500 — which reached the admin panel as
	// a bare "failed to create user" with no idea which field was the problem.
	// Check before writing so the message can name it.
	ctx := c.Request.Context()
	if h.usernameTaken(ctx, req.Username) {
		c.JSON(http.StatusConflict, gin.H{"error": "That username is already taken."})
		return
	}
	if h.emailTaken(ctx, req.Email) {
		c.JSON(http.StatusConflict, gin.H{"error": "That email address is already in use."})
		return
	}

	passwordHash, err := h.authService.HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
		return
	}

	user, err := h.adminService.CreateUser(ctx, req.Username, req.Email, passwordHash, req.DisplayName, req.Role, req.QuotaBytes, homePath)
	if err != nil {
		// Lost the race against a concurrent create between the check above and
		// the insert. The index is still the authority; say which field it hit.
		if h.usernameTaken(ctx, req.Username) {
			c.JSON(http.StatusConflict, gin.H{"error": "That username is already taken."})
			return
		}
		if h.emailTaken(ctx, req.Email) {
			c.JSON(http.StatusConflict, gin.H{"error": "That email address is already in use."})
			return
		}
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user"})
		return
	}

	// Log audit
	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_create_user", "user", &user.ID, "Created user: "+req.Username)

	c.JSON(http.StatusCreated, model.UserResponse{
		ID:                 user.ID,
		Username:           user.Username,
		Email:              user.Email,
		DisplayName:        user.DisplayName,
		Role:               user.Role,
		QuotaBytes:         user.QuotaBytes,
		HomePath:           user.HomePath,
		IsActive:           user.IsActive,
		MustRotatePassword: user.MustRotatePass,
		CreatedAt:          user.CreatedAt,
	})
}

// resolveHomePath validates a requested home directory and returns the cleaned
// absolute form. It must already exist as a folder row, otherwise the account
// would start out pointing at nothing and every request would 404.
func (h *AdminHandler) resolveHomePath(c *gin.Context, raw string) (string, bool) {
	cleaned := scope.Clean(raw)
	if cleaned == "" {
		return "", true // storage root
	}

	// The trash root is a real folder row, so it passes the lookup below. But
	// browsing it is refused (ListFiles, CreateFolder, RenameFile all block it),
	// and lookups by id carry no is_trashed filter — so pinning a home there
	// would hand the account a way to reach every trashed file in the instance
	// through download and preview. Refuse it as a home, and anything under it.
	if cleaned == trashRoot || strings.HasPrefix(cleaned, trashRoot+"/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the trash cannot be used as a home folder"})
		return "", false
	}

	var folder model.File
	if err := h.db.First(&folder, "path = ? AND is_dir = ? AND is_trashed = ?", cleaned, true, false).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "home folder not found"})
		return "", false
	}
	return cleaned, true
}

// otherAdminRemains reports whether some account other than `except` is still
// an active admin. Used to refuse demoting or deleting the last way into the
// panel: nothing in the API can restore it afterwards, because SeedAdmin only
// runs at boot and returns early while any admin row exists.
func (h *AdminHandler) otherAdminRemains(ctx context.Context, except uuid.UUID) bool {
	var n int64
	h.db.WithContext(ctx).Model(&model.User{}).
		Where("role = ? AND is_active = ? AND id <> ?", "admin", true, except).
		Count(&n)
	return n > 0
}

// usernameTaken and emailTaken report whether a value collides with a *live*
// account. Soft-deleted rows are excluded on purpose: the unique indexes they are
// checked against are partial for the same reason, so counting the deleted rows
// here would report a conflict the database is willing to accept — and the
// Users table shows only live accounts, so the admin would have no way to see
// what was holding the name.
func (h *AdminHandler) usernameTaken(ctx context.Context, username string) bool {
	var n int64
	h.db.WithContext(ctx).Model(&model.User{}).
		Where("username = ?", username).Count(&n)
	return n > 0
}

func (h *AdminHandler) emailTaken(ctx context.Context, email string) bool {
	if email == "" {
		return false
	}
	var n int64
	h.db.WithContext(ctx).Model(&model.User{}).
		Where("email = ?", email).Count(&n)
	return n > 0
}

func (h *AdminHandler) UpdateUser(c *gin.Context) {
	userID := c.Param("id")

	// An allowlist, not a denylist. The denylist this replaces removed id,
	// created_at, used_bytes and is_active, and omitted two fields that mattered:
	// deleted_at, so `{"deleted_at": null}` cleared the soft-delete stamp and
	// brought a deleted account back with its original password hash and admin
	// role — invisible in the user list, because every listing filters deleted
	// rows out. And role, which let a patch set any string at all, bypassing the
	// oneof=admin user validation the dedicated route performs.
	allowed := map[string]bool{
		"username": true, "email": true, "display_name": true,
		"avatar_url": true, "home_path": true, "role": true,
		"password_hash": true,
	}
	var raw map[string]interface{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		if !allowed[k] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "field cannot be updated here: " + k})
			return
		}
		req[k] = v
	}
	if len(req) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	// A home directory change goes through the same validation as creation,
	// so it can never be pointed at a path that is not a real folder.
	if raw, present := req["home_path"]; present {
		asString, isString := raw.(string)
		if !isString {
			c.JSON(http.StatusBadRequest, gin.H{"error": "home_path must be a string"})
			return
		}
		homePath, ok := h.resolveHomePath(c, asString)
		if !ok {
			return
		}
		req["home_path"] = homePath
	}

	// A password in the patch means "reset it". The client omits the field when
	// the admin is only changing something else, which leaves the hash alone.
	if raw, present := req["password"]; present {
		plain, isString := raw.(string)
		if !isString {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password must be a string"})
			return
		}
		delete(req, "password")
		if plain != "" {
			if len(plain) < 8 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 8 characters"})
				return
			}
			hash, err := h.authService.HashPassword(plain)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
				return
			}
			req["password_hash"] = hash
		}
	}

	// A role change is an authority change, so every token already issued to this
	// account has to stop working — including the one making this request, which
	// is why the version is bumped rather than merely checked.
	if _, changingRole := req["role"]; changingRole {
		role, _ := req["role"].(string)
		if role != "admin" && role != "user" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role must be admin or user"})
			return
		}
		req["token_version"] = gorm.Expr("token_version + 1")
	}
	if _, resettingPassword := req["password_hash"]; resettingPassword {
		req["token_version"] = gorm.Expr("token_version + 1")
	}

	target, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	// Never let the last way into the panel be closed by accident. A self
	// demotion also leaves the caller holding a token the version check is about
	// to reject, so it would look like it worked for a request and then stop.
	if _, demoting := req["role"]; demoting {
		if role, _ := req["role"].(string); role != "admin" && !h.otherAdminRemains(c.Request.Context(), target) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "this is the last admin account; promote another user first"})
			return
		}
	}

	ctx := c.Request.Context()
	if err := h.adminService.UpdateUser(ctx, target, req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update user"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_user", "user", uuidPtr(userID), "Updated user")

	c.JSON(http.StatusOK, gin.H{"message": "user updated"})
}

func (h *AdminHandler) DeleteUser(c *gin.Context) {
	userID := c.Param("id")

	target, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ctx := c.Request.Context()

	// Revoke first, and check the result. Deleting the row alone left every
	// share link the account had handed out live and serving their files —
	// GetShareByToken never joined the user, so a soft-deleted owner did not
	// stop it. Deleting an account has to take its public surface with it.
	if n := h.db.WithContext(ctx).Model(&model.ShareLink{}).
		Where("user_id = ?", target).Delete(&model.ShareLink{}).RowsAffected; n > 0 {
		logAudit(h.db, c, target, "share_revoke_all", "user", &target, "Revoked share links on account deletion")
	}
	// Closing the last door to the panel is not recoverable through the API:
	// SeedAdmin runs at boot and returns early while any admin row exists.
	var role string
	if err := h.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", target).Select("role").Scan(&role).Error; err == nil && role == "admin" {
		if !h.otherAdminRemains(ctx, target) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "this is the last admin account; promote another user first"})
			return
		}
	}

	if err := h.adminService.DeleteUser(ctx, target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete user"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_delete_user", "user", &target, "Deleted user")

	c.JSON(http.StatusOK, gin.H{"message": "user deleted"})
}

func (h *AdminHandler) UpdateUserQuota(c *gin.Context) {
	userID := c.Param("id")
	parsed, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var req struct {
		QuotaBytes int64 `json:"quota_bytes" binding:"required,gte=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	if err := h.adminService.UpdateUserQuota(ctx, parsed, req.QuotaBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update quota"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_quota", "user", &parsed, fmt.Sprintf("Quota set to %d bytes", req.QuotaBytes))

	c.JSON(http.StatusOK, gin.H{"message": "quota updated"})
}

func (h *AdminHandler) UpdateUserRole(c *gin.Context) {
	userID := c.Param("id")
	parsed, err := uuid.Parse(userID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}

	var req struct {
		Role string `json:"role" binding:"required,oneof=admin user"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	if err := h.adminService.UpdateUserRole(ctx, parsed, req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_role", "user", &parsed, fmt.Sprintf("Role changed to %s", req.Role))

	c.JSON(http.StatusOK, gin.H{"message": "role updated"})
}
