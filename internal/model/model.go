package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// generateUUID assigns a v4 UUID when the primary key is still the nil value.
// SQLite has no auto-increment or column default for uuid columns, so without
// this every insert would store 00000000-0000-0000-0000-000000000000.
//
// Kept as a plain function (not an embedded struct) because embedding a struct
// with an unexported type breaks GORM's foreign-key relation resolution.
func generateUUID(id *uuid.UUID) {
	if *id == uuid.Nil {
		*id = uuid.New()
	}
}

type User struct {
	ID uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	// Uniqueness is enforced over *live* accounts only. A plain unique index
	// would keep a soft-deleted row's username reserved forever, so the name
	// stayed unusable while the Users table — which filters deleted rows out —
	// showed no trace of who was holding it. Deleting an account now hands the
	// name back, which is what deleting an account is supposed to mean.
	Username     string `gorm:"uniqueIndex:idx_users_username,where:deleted_at IS NULL;size:50;not null" json:"username"`
	Email        string `gorm:"uniqueIndex:idx_users_email,where:deleted_at IS NULL;size:100" json:"email"`
	PasswordHash string `gorm:"size:255;not null" json:"-"`
	DisplayName  string `gorm:"size:100" json:"display_name"`
	AvatarURL    string `gorm:"size:500" json:"avatar_url"`
	Role         string `gorm:"size:20;default:'user'" json:"role"`     // admin, user
	QuotaBytes   int64  `gorm:"default:10737418240" json:"quota_bytes"` // 10GB default
	UsedBytes    int64  `gorm:"default:0" json:"used_bytes"`
	// HomePath confines the user to a subtree of the storage root, the way
	// FileBrowser scopes a user to a home directory. Empty means the storage
	// root, i.e. no confinement. Two users sharing a HomePath see the same
	// files; a user given their own folder gets a private one.
	HomePath string `gorm:"size:1000;default:''" json:"home_path"`
	IsActive bool   `gorm:"default:true" json:"is_active"`
	// TokenVersion is embedded in every issued token and bumped whenever the
	// account's authority changes — password, role, deactivation. A token
	// carrying an older version is refused.
	//
	// Without it, changing a password was not revocation. Every outstanding
	// access token kept working for its full 24h, every refresh token for 168h,
	// and because refresh mints a new pair from the old one, a stolen refresh
	// token granted perpetual access — which is exactly the session an incident
	// responder needs to be able to cut off.
	TokenVersion int `gorm:"default:0" json:"-"`
	// MustRotatePass is set on the account seeded from the shipped default
	// credential and cleared only by a real password change. While it is set the
	// account can do nothing but change its password — the server still runs,
	// because refusing to boot would strand an operator who cannot reach the
	// panel in the first place.
	MustRotatePass bool           `gorm:"column:must_rotate_pass;default:false" json:"must_rotate_password"`
	LastLoginAt    *time.Time     `json:"last_login_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
}

func (u *User) BeforeCreate(*gorm.DB) error {
	generateUUID(&u.ID)
	return nil
}

func (User) TableName() string {
	return "users"
}

type File struct {
	ID           uuid.UUID      `gorm:"type:uuid;primary_key" json:"id"`
	UserID       uuid.UUID      `gorm:"type:uuid;index;not null" json:"user_id"`
	ParentID     *uuid.UUID     `gorm:"type:uuid;index" json:"parent_id"`
	Name         string         `gorm:"size:255;not null" json:"name"`
	Path         string         `gorm:"size:1000;not null;index" json:"path"`
	Size         int64          `gorm:"default:0" json:"size"`
	MimeType     string         `gorm:"size:100" json:"mime_type"`
	IsDir        bool           `gorm:"default:false;index" json:"is_dir"`
	Checksum     string         `gorm:"size:64" json:"checksum"` // SHA256
	StorageType  string         `gorm:"size:20;default:'local'" json:"storage_type"`
	StoragePath  string         `gorm:"size:500" json:"storage_path"`
	IsTrashed    bool           `gorm:"default:false;index" json:"is_trashed"`
	TrashedAt    *time.Time     `json:"trashed_at"`
	OriginalPath string         `gorm:"size:1000" json:"-"` // set while in trash, enables restore
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`

	// Relations
	User User `gorm:"foreignKey:UserID" json:"-"`
}

func (f *File) BeforeCreate(*gorm.DB) error {
	generateUUID(&f.ID)

	return nil
}

func (File) TableName() string {
	return "files"
}

type ShareLink struct {
	ID           uuid.UUID  `gorm:"type:uuid;primary_key" json:"id"`
	UserID       uuid.UUID  `gorm:"type:uuid;index;not null" json:"user_id"`
	FileID       uuid.UUID  `gorm:"type:uuid;index;not null" json:"file_id"`
	Token        string     `gorm:"uniqueIndex;size:64;not null" json:"token"`
	PasswordHash string     `gorm:"size:255" json:"-"`
	ExpiresAt    *time.Time `gorm:"index" json:"expires_at"`
	MaxViews     int        `gorm:"default:0" json:"max_views"`
	ViewCount    int        `gorm:"default:0" json:"view_count"`
	IsActive     bool       `gorm:"default:true;index" json:"is_active"`
	// Permission is "read" (default) or "write". A "write" share on a
	// folder lets visitors upload into it without an account.
	Permission string         `gorm:"size:20;default:'read'" json:"permission"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`

	// Relations
	User User `gorm:"foreignKey:UserID" json:"-"`
	File File `gorm:"foreignKey:FileID" json:"-"`
}

func (s *ShareLink) BeforeCreate(*gorm.DB) error {
	generateUUID(&s.ID)
	return nil
}

func (ShareLink) TableName() string {
	return "share_links"
}

type Setting struct {
	Key         string    `gorm:"primaryKey;size:100" json:"key"`
	Value       string    `gorm:"type:text" json:"value"`
	Description string    `gorm:"size:500" json:"description"`
	IsPublic    bool      `gorm:"default:false" json:"is_public"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (Setting) TableName() string {
	return "settings"
}

type UploadSession struct {
	ID             uuid.UUID      `gorm:"type:uuid;primary_key" json:"id"`
	UserID         uuid.UUID      `gorm:"type:uuid;index;not null" json:"user_id"`
	FileID         *uuid.UUID     `gorm:"type:uuid;index" json:"file_id"`
	ParentID       *uuid.UUID     `gorm:"type:uuid;index" json:"parent_id"`
	Filename       string         `gorm:"size:255;not null" json:"filename"`
	TargetPath     string         `gorm:"size:1000;not null" json:"target_path"`
	TotalSize      int64          `gorm:"not null" json:"total_size"`
	ChunkSize      int64          `gorm:"not null" json:"chunk_size"`
	TotalChunks    int            `gorm:"not null" json:"total_chunks"`
	UploadedChunks int            `gorm:"default:0" json:"uploaded_chunks"`
	Status         string         `gorm:"size:20;default:'pending'" json:"status"` // pending, uploading, completed, failed, expired
	TempDir        string         `gorm:"size:500" json:"temp_dir"`
	Checksum       string         `gorm:"size:64" json:"checksum"`
	ExpiresAt      time.Time      `gorm:"index" json:"expires_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
}

func (u *UploadSession) BeforeCreate(*gorm.DB) error {
	generateUUID(&u.ID)
	return nil
}

func (UploadSession) TableName() string {
	return "upload_sessions"
}

type AuditLog struct {
	ID         uuid.UUID  `gorm:"type:uuid;primary_key" json:"id"`
	UserID     *uuid.UUID `gorm:"type:uuid;index" json:"user_id"`
	Action     string     `gorm:"size:50;not null;index" json:"action"`
	Resource   string     `gorm:"size:50" json:"resource"`
	ResourceID *uuid.UUID `gorm:"type:uuid;index" json:"resource_id"`
	Details    string     `gorm:"type:text" json:"details"`
	IPAddress  string     `gorm:"size:45" json:"ip_address"`
	UserAgent  string     `gorm:"size:500" json:"user_agent"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
}

func (a *AuditLog) BeforeCreate(*gorm.DB) error {
	generateUUID(&a.ID)
	return nil
}

func (AuditLog) TableName() string {
	return "audit_logs"
}

// DTOs for API
type UserResponse struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url"`
	Role        string    `json:"role"`
	QuotaBytes  int64     `json:"quota_bytes"`
	UsedBytes   int64     `json:"used_bytes"`
	// HomePath is the user's root directory; "" means the storage root.
	HomePath string `json:"home_path"`
	IsActive bool   `json:"is_active"`
	// MustRotatePassword has to be on the wire, not just in the database.
	//
	// Without it the client cannot tell an account confined to the password
	// change from an account that is broken: every route answers 403, the
	// session is valid, and nothing on screen explains why. The field is what
	// routes the app to the one page that can clear the condition.
	MustRotatePassword bool      `json:"must_rotate_password"`
	CreatedAt          time.Time `json:"created_at"`
}

type FileResponse struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Size      int64      `json:"size"`
	MimeType  string     `json:"mime_type"`
	IsDir     bool       `json:"is_dir"`
	Checksum  string     `json:"checksum"`
	ParentID  *uuid.UUID `json:"parent_id"`
	IsTrashed bool       `json:"is_trashed"`
	TrashedAt *time.Time `json:"trashed_at"`
	// OriginalPath is only exposed for trashed items, so the client can label
	// where a file will return to on restore.
	OriginalPath string    `json:"original_path,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// FolderNode is a lightweight entry for the move/copy destination picker.
type FolderNode struct {
	ID       uuid.UUID    `json:"id"`
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	ParentID *uuid.UUID   `json:"parent_id"`
	Children []FolderNode `json:"children,omitempty"`
}

// ToFileResponse centralises model -> DTO conversion so every endpoint emits
// the same shape.
func ToFileResponse(f *File) FileResponse {
	return FileResponse{
		ID:           f.ID,
		Name:         f.Name,
		Path:         f.Path,
		Size:         f.Size,
		MimeType:     f.MimeType,
		IsDir:        f.IsDir,
		Checksum:     f.Checksum,
		ParentID:     f.ParentID,
		IsTrashed:    f.IsTrashed,
		TrashedAt:    f.TrashedAt,
		OriginalPath: f.OriginalPath,
		CreatedAt:    f.CreatedAt,
		UpdatedAt:    f.UpdatedAt,
	}
}

type ShareLinkResponse struct {
	ID         uuid.UUID  `json:"id"`
	FileID     uuid.UUID  `json:"file_id"`
	FileName   string     `json:"file_name"`
	Token      string     `json:"token"`
	URL        string     `json:"url"`
	ExpiresAt  *time.Time `json:"expires_at"`
	MaxViews   int        `json:"max_views"`
	ViewCount  int        `json:"view_count"`
	IsActive   bool       `json:"is_active"`
	Permission string     `json:"permission"`
	// Status is one of share.StatusActive / StatusInactive / StatusExpired /
	// StatusLimitNeeded, so the UI does not re-derive the rules.
	Status      string    `json:"status"`
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
}

type StorageStats struct {
	TotalFiles   int64 `json:"total_files"`
	TotalFolders int64 `json:"total_folders"`
	TotalSize    int64 `json:"total_size"`
	// TotalQuota is the sum of every user's storage allowance, so the admin
	// view can show a meaningful used/total bar.
	TotalQuota int64 `json:"total_quota"`
}

type UserStats struct {
	TotalUsers  int64 `json:"total_users"`
	ActiveUsers int64 `json:"active_users"`
	TotalShares int64 `json:"total_shares"`
}
