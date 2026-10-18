package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/admin"
	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"github.com/yuarcloud/yuarcloud/internal/share"
	"github.com/yuarcloud/yuarcloud/internal/storage"
	"gorm.io/gorm"
)

// maxAllowedChunkSize keeps a single upload request under the 100 MB ceiling
// imposed by Cloudflare Tunnel's free tier.
const maxAllowedChunkSize int64 = 100 * 1024 * 1024

type AdminHandler struct {
	db           *gorm.DB
	adminService *admin.AdminService
	authService  *auth.AuthService
	shareService *share.ShareService
	// storage is here so the chunked-upload settings can be written straight into
	// the live limits; without it the panel could only write rows nobody read.
	storage storage.StorageBackend
}

func NewAdminHandler(db *gorm.DB, adminService *admin.AdminService, authService *auth.AuthService, shareService *share.ShareService, store storage.StorageBackend) *AdminHandler {
	return &AdminHandler{
		db:           db,
		adminService: adminService,
		authService:  authService,
		shareService: shareService,
		storage:      store,
	}
}

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

	ctx := c.Request.Context()
	user, err := h.adminService.GetUser(ctx, uuid.MustParse(userID))
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

	var req struct {
		QuotaBytes int64 `json:"quota_bytes" binding:"required,gte=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	if err := h.adminService.UpdateUserQuota(ctx, uuid.MustParse(userID), req.QuotaBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update quota"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_quota", "user", uuidPtr(userID), fmt.Sprintf("Quota set to %d bytes", req.QuotaBytes))

	c.JSON(http.StatusOK, gin.H{"message": "quota updated"})
}

func (h *AdminHandler) UpdateUserRole(c *gin.Context) {
	userID := c.Param("id")

	var req struct {
		Role string `json:"role" binding:"required,oneof=admin user"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()
	if err := h.adminService.UpdateUserRole(ctx, uuid.MustParse(userID), req.Role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_role", "user", uuidPtr(userID), fmt.Sprintf("Role changed to %s", req.Role))

	c.JSON(http.StatusOK, gin.H{"message": "role updated"})
}

func (h *AdminHandler) GetStats(c *gin.Context) {
	ctx := c.Request.Context()

	userStats, err := h.adminService.GetStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get stats"})
		return
	}

	storageStats, err := h.adminService.GetStorageStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get storage stats"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"users":   userStats,
		"storage": storageStats,
	})
}

func (h *AdminHandler) GetAuditLogs(c *gin.Context) {
	page := 1
	pageSize := 50
	action := c.Query("action")
	userID := c.Query("user_id")

	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	ctx := c.Request.Context()
	logs, total, err := h.adminService.GetAuditLogs(ctx, page, pageSize, action, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get audit logs"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":     logs,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}

// GetShares lists every share link on the instance.
//
// This used to sit here with a note that it "replaces the audit-log view in the
// admin UI: server-side events are read from the container log". That was false,
// and it was load-bearing — it is the stated reason the audit table was removed
// from the panel.
//
// Server-side events are not read from the container log. They are rows: every
// logAudit call writes to the audit_logs table, and GetAuditLogs serves them
// back over GET /api/admin/audit-logs. Measured on a throwaway instance: seven
// rows written, all seven returned by the endpoint, all seven still there after
// a restart, and zero of them in the application log — the only audit-shaped
// lines there are gin's access log recording requests to the endpoint itself.
// The container log carries the request; the table carries the event.
//
// The endpoint works. What is missing is a view for it. Restoring that is a
// frontend change and nothing here needs to move to make it possible.
func (h *AdminHandler) GetShares(c *gin.Context) {
	page := 1
	pageSize := 50

	if p := c.Query("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := c.Query("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}

	ctx := c.Request.Context()
	shares, total, err := h.shareService.GetAllShares(ctx, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get share links"})
		return
	}

	type entry struct {
		ID         uuid.UUID  `json:"id"`
		Token      string     `json:"token"`
		URL        string     `json:"url"`
		Path       string     `json:"path"`
		Owner      string     `json:"owner"`
		FileName   string     `json:"file_name"`
		FileID     uuid.UUID  `json:"file_id"`
		Active     bool       `json:"is_active"`
		HasPass    bool       `json:"has_password"`
		Permission string     `json:"permission"`
		Views      int        `json:"views"`
		MaxViews   int        `json:"max_views"`
		Expires    *time.Time `json:"expires_at"`
		CreatedAt  time.Time  `json:"created_at"`
	}

	out := make([]entry, 0, len(shares))
	for _, s := range shares {
		// User is a value field, not a pointer, so a row whose owner has been
		// deleted preloads as the zero value rather than nil.
		owner := s.User.DisplayName
		if owner == "" {
			owner = s.User.Username
		}
		url := h.shareService.ShareURL(s.Token, c.Request)
		out = append(out, entry{
			ID:    s.ID,
			Token: s.Token,
			// ShareURL rather than a token-only builder: with no base_url
			// configured the latter returns a bare "/yc/<token>", which is no
			// use in a copy button.
			URL: url,
			// Path is what the table shows. The absolute URL is long enough to
			// push an eighth column past the edge of a laptop screen, and the
			// host is already visible in the address bar.
			Path:       share.URLPath + s.Token,
			Owner:      owner,
			FileName:   s.File.Name,
			FileID:     s.FileID,
			Active:     s.IsActive,
			HasPass:    s.PasswordHash != "",
			Permission: s.Permission,
			Views:      s.ViewCount,
			MaxViews:   s.MaxViews,
			Expires:    s.ExpiresAt,
			CreatedAt:  s.CreatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"shares":    out,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// DeleteShare revokes a share link from the admin view. Unlike the per-user
// endpoint this is not scoped to an owner — an admin is expected to be able
// to take down any link on the instance.
func (h *AdminHandler) DeleteShare(c *gin.Context) {
	shareID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid share id"})
		return
	}

	if err := h.shareService.DeleteShareByID(c.Request.Context(), shareID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete share link"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "share link deleted"})
}

func (h *AdminHandler) GetChunkedUploadConfig(c *gin.Context) {
	// Report the values the server is actually enforcing, not just what is in
	// the database. They normally agree — ApplyChunkedConfig writes through on
	// save and again at boot — but the live numbers are what an upload will be
	// judged against, so showing anything else risks the page disagreeing with
	// the behaviour the user is trying to configure.
	limits := h.storage.GetLimits()

	c.JSON(http.StatusOK, gin.H{
		"max_chunk_size":   limits.MaxChunkSize(),
		"max_file_size":    limits.MaxFileSize(),
		"temp_dir":         defaultTempDir,
		"cleanup_interval": formatDuration(limits.CleanupInterval()),
	})
}

// formatDuration writes a duration the way the config files and the admin form
// both spell it: "15m", "1h", "24h".
//
// time.Duration.String() writes the whole breakdown instead — six hours comes
// back as "6h0m0s" — so the value an admin saved as "6h" was reported as
// "6h0m0s" and matched no option on the form that had just sent it. Every load
// of that page showed the first entry in the dropdown instead, and saving from
// it overwrote whatever was really configured. Anything that does not land on a
// whole unit falls through to Go's own spelling, which is at least honest.
//
// The alternative was returning the string from the settings table verbatim.
// That would round-trip too, but it reports what was stored rather than what is
// being enforced, which is the opposite of what this endpoint is for: the live
// limit is the one an upload is actually judged against.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	// Seconds only while there are none of the larger units to keep. Past a
	// minute, "1m30s" reads better than the "90s" the same duration is, and
	// Go's own spelling is the more legible of the two.
	case d < time.Minute && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	default:
		return d.String()
	}
}

func (h *AdminHandler) UpdateChunkedUploadConfig(c *gin.Context) {
	var req struct {
		MaxChunkSize    int64  `json:"max_chunk_size"`
		MaxFileSize     int64  `json:"max_file_size"`
		CleanupInterval string `json:"cleanup_interval"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx := c.Request.Context()

	// Reject values the upload path cannot honour.
	if req.MaxChunkSize > maxAllowedChunkSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("max_chunk_size must be at most %d bytes", maxAllowedChunkSize)})
		return
	}
	if req.MaxFileSize > 0 && req.MaxChunkSize > 0 && req.MaxFileSize < req.MaxChunkSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_file_size must be larger than max_chunk_size"})
		return
	}

	updates := map[string]string{}
	if req.MaxChunkSize > 0 {
		updates[settingKeyMaxChunkSize] = strconv.FormatInt(req.MaxChunkSize, 10)
	}
	if req.MaxFileSize > 0 {
		updates[settingKeyMaxFileSize] = strconv.FormatInt(req.MaxFileSize, 10)
	}
	if req.CleanupInterval != "" {
		if _, err := time.ParseDuration(req.CleanupInterval); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cleanup_interval must be a duration such as 30m or 1h"})
			return
		}
		updates[settingKeyCleanupInterval] = req.CleanupInterval
	}

	for key, value := range updates {
		if err := h.adminService.UpdateSetting(ctx, key, value, "Chunked upload configuration", true); err != nil {
			c.Error(err) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save configuration"})
			return
		}
	}

	// Persisting is not the end of it — push the new values into the running
	// limits so they take effect without a restart.
	if err := h.ApplyChunkedConfig(ctx); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply configuration"})
		return
	}

	currentUserID, _ := middleware.GetUserID(c)
	logAudit(h.db, c, currentUserID, "admin_update_chunked_config", "setting", nil, "Updated chunked upload configuration")

	c.JSON(http.StatusOK, gin.H{"message": "chunked upload config updated"})
}

// Chunked-upload defaults. These mirror configs/config.yaml.example and are
// used until an admin overrides them from the admin panel.
const (
	settingKeyMaxChunkSize    = "storage.chunked.max_chunk_size"
	settingKeyMaxFileSize     = "storage.chunked.max_file_size"
	settingKeyCleanupInterval = "storage.chunked.cleanup_interval"

	defaultMaxChunkSize    int64 = 10 * 1024 * 1024
	defaultCleanupInterval       = "1h"
	defaultTempDir               = "./data/tmp/chunks"
)

// ApplyChunkedConfig pushes the persisted ceilings into the live limits the
// upload path reads on every request.
//
// This is the step that was missing. The handler used to save these to the
// database and stop there; nothing consumed them, so the numbers on the settings
// page were decorative until someone edited the YAML and restarted. Saving now
// has to reach into the running process or the page is lying about what it does.
func (h *AdminHandler) ApplyChunkedConfig(ctx context.Context) error {
	limits := h.storage.GetLimits()

	if v := h.settingInt64(ctx, settingKeyMaxChunkSize, defaultMaxChunkSize); v > 0 {
		limits.SetMaxChunkSize(v)
	}
	// Zero means "no ceiling" rather than "reject everything", so it is left to
	// whatever is already in place rather than forced to the default.
	if v := h.settingInt64(ctx, settingKeyMaxFileSize, 0); v > 0 {
		limits.SetMaxFileSize(v)
	}
	if raw := h.settingString(ctx, settingKeyCleanupInterval, defaultCleanupInterval); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			limits.SetCleanupInterval(d)
		}
	}
	return nil
}

// settingInt64 reads a numeric setting, falling back on parse failure.
func (h *AdminHandler) settingInt64(ctx context.Context, key string, fallback int64) int64 {
	raw, err := h.adminService.GetSetting(ctx, key)
	if err != nil || raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (h *AdminHandler) settingString(ctx context.Context, key, fallback string) string {
	raw, err := h.adminService.GetSetting(ctx, key)
	if err != nil || raw == "" {
		return fallback
	}
	return raw
}
