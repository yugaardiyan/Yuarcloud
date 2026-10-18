package admin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

type AdminService struct {
	db *gorm.DB
}

func NewAdminService(db *gorm.DB) *AdminService {
	return &AdminService{db: db}
}

func (s *AdminService) GetUsers(ctx context.Context, page, pageSize int, search, role string) ([]*model.UserResponse, int64, error) {
	var users []*model.User
	var total int64

	query := s.db.WithContext(ctx).Model(&model.User{})

	if search != "" {
		// LOWER(x) LIKE LOWER(?) rather than ILIKE. SQLite has no ILIKE
		// operator at all — GORM does not rewrite it for that dialect either —
		// so every user search on the shipped database was a parse error, and
		// the admin panel's search box returned 500 on the default install.
		// Folding the lower() into the columns keeps the match case-insensitive
		// on both engines, which is what ILIKE meant here.
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where(
			"LOWER(username) LIKE ? OR LOWER(email) LIKE ? OR LOWER(display_name) LIKE ?",
			pattern, pattern, pattern,
		)
	}
	if role != "" {
		query = query.Where("role = ?", role)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Order("created_at DESC").Find(&users).Error; err != nil {
		return nil, 0, err
	}

	responses := make([]*model.UserResponse, len(users))
	for i, u := range users {
		responses[i] = &model.UserResponse{
			ID:          u.ID,
			Username:    u.Username,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			AvatarURL:   u.AvatarURL,
			Role:        u.Role,
			QuotaBytes:  u.QuotaBytes,
			UsedBytes:   u.UsedBytes,
			HomePath:    u.HomePath,
			IsActive:    u.IsActive,
			// The admin list is where an operator finds out who is still on the
			// published credential, which is the only way they can tell a
			// locked-out account from a working one.
			MustRotatePassword: u.MustRotatePass,
			CreatedAt:          u.CreatedAt,
		}
	}

	return responses, total, nil
}

func (s *AdminService) GetUser(ctx context.Context, userID uuid.UUID) (*model.UserResponse, error) {
	var user model.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", userID).Error; err != nil {
		return nil, err
	}

	return &model.UserResponse{
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
	}, nil
}

func (s *AdminService) CreateUser(ctx context.Context, username, email, passwordHash, displayName, role string, quotaBytes int64, homePath string) (*model.User, error) {
	user := &model.User{
		Username:     username,
		Email:        email,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
		Role:         role,
		QuotaBytes:   quotaBytes,
		HomePath:     homePath,
		IsActive:     true,
	}

	if err := s.db.WithContext(ctx).Create(user).Error; err != nil {
		return nil, err
	}

	return user, nil
}

func (s *AdminService) UpdateUser(ctx context.Context, userID uuid.UUID, updates map[string]interface{}) error {
	updates["updated_at"] = time.Now()
	return s.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Updates(updates).Error
}

func (s *AdminService) DeleteUser(ctx context.Context, userID uuid.UUID) error {
	return s.db.WithContext(ctx).Delete(&model.User{}, "id = ?", userID).Error
}

func (s *AdminService) UpdateUserQuota(ctx context.Context, userID uuid.UUID, quotaBytes int64) error {
	return s.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Update("quota_bytes", quotaBytes).Error
}

func (s *AdminService) UpdateUserRole(ctx context.Context, userID uuid.UUID, role string) error {
	return s.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", userID).Update("role", role).Error
}

func (s *AdminService) GetStats(ctx context.Context) (*model.UserStats, error) {
	var stats model.UserStats

	s.db.WithContext(ctx).Model(&model.User{}).Count(&stats.TotalUsers)
	s.db.WithContext(ctx).Model(&model.User{}).Where("is_active = ?", true).Count(&stats.ActiveUsers)

	s.db.WithContext(ctx).Model(&model.ShareLink{}).Where("is_active = ?", true).Count(&stats.TotalShares)

	return &stats, nil
}

func (s *AdminService) GetStorageStats(ctx context.Context) (*model.StorageStats, error) {
	var stats model.StorageStats

	s.db.WithContext(ctx).Model(&model.File{}).Where("is_dir = ?", false).Count(&stats.TotalFiles)
	s.db.WithContext(ctx).Model(&model.File{}).Where("is_dir = ?", true).Count(&stats.TotalFolders)

	s.db.WithContext(ctx).Model(&model.File{}).Where("is_dir = ?", false).Select("COALESCE(SUM(size), 0)").Scan(&stats.TotalSize)

	// Only active users contribute to the allowance on offer.
	s.db.WithContext(ctx).Model(&model.User{}).Where("is_active = ?", true).Select("COALESCE(SUM(quota_bytes), 0)").Scan(&stats.TotalQuota)

	return &stats, nil
}

// GetSetting reads a single persisted setting, returning "" when absent.
func (s *AdminService) GetSetting(ctx context.Context, key string) (string, error) {
	var setting model.Setting
	if err := s.db.WithContext(ctx).First(&setting, "key = ?", key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return setting.Value, nil
}

func (s *AdminService) UpdateSetting(ctx context.Context, key, value, description string, isPublic bool) error {
	setting := model.Setting{
		Key:         key,
		Value:       value,
		Description: description,
		IsPublic:    isPublic,
	}
	return s.db.WithContext(ctx).Save(&setting).Error
}

func (s *AdminService) GetAuditLogs(ctx context.Context, page, pageSize int, action, userID string) ([]*model.AuditLog, int64, error) {
	var logs []*model.AuditLog
	var total int64

	query := s.db.WithContext(ctx).Model(&model.AuditLog{})

	if action != "" {
		query = query.Where("action = ?", action)
	}
	if userID != "" {
		query = query.Where("user_id = ?", userID)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Order("created_at DESC").Find(&logs).Error; err != nil {
		return nil, 0, err
	}

	return logs, total, nil
}
