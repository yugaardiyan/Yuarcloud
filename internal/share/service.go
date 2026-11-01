package share

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var (
	ErrShareNotFound      = errors.New("share link not found")
	ErrShareExpired       = errors.New("share link expired")
	ErrShareInactive      = errors.New("share link inactive")
	ErrShareMaxViews      = errors.New("share link max views reached")
	ErrInvalidPassword    = errors.New("invalid password")
	ErrShareAlreadyExists = errors.New("a share link already exists for this file")
	ErrPasswordRequired   = errors.New("this server requires a password on share links")
)

type ShareService struct {
	db              *gorm.DB
	baseURL         string
	defaultExpiry   int
	maxExpiry       int
	requirePassword bool
	maxViews        int
}

func NewShareService(db *gorm.DB, baseURL string, defaultExpiryDays, maxExpiryDays int, requirePassword bool, maxViews int) *ShareService {
	return &ShareService{
		db:              db,
		baseURL:         baseURL,
		defaultExpiry:   defaultExpiryDays,
		maxExpiry:       maxExpiryDays,
		requirePassword: requirePassword,
		maxViews:        maxViews,
	}
}

// generateToken builds a short, human-friendly share code: six random
// alphanumeric characters (e.g. aZ3kPq). Every position is drawn from the
// same pool, so digits and letters turn up wherever they land. It retries
// until the code is unused so two links never collide.
func (s *ShareService) generateToken() (string, error) {
	const alnum = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

	for attempt := 0; attempt < 50; attempt++ {
		buf := make([]byte, 0, shareTokenLength)

		for i := 0; i < shareTokenLength; i++ {
			idx, err := randIndex(len(alnum))
			if err != nil {
				return "", err
			}
			buf = append(buf, alnum[idx])
		}

		token := string(buf)
		exists, err := s.tokenExists(token)
		if err != nil {
			return "", err
		}
		if !exists {
			return token, nil
		}
		// Extremely rare collision — try again.
	}
	return "", errors.New("failed to generate a unique share token")
}

// randIndex returns an unbiased random index in [0, n).
func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

func (s *ShareService) tokenExists(token string) (bool, error) {
	var count int64
	if err := s.db.Model(&model.ShareLink{}).Where("token = ?", token).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateShare is the single-item entry point. It is the same logic as
// CreateShareFiles with a one-element set, so single and multi links cannot
// diverge on expiry, password policy or the one-share-per-item rule.
func (s *ShareService) CreateShare(ctx context.Context, userID, fileID uuid.UUID, password string, expiryDays, maxViews int, permission string) (*model.ShareLink, error) {
	return s.CreateShareFiles(ctx, userID, []uuid.UUID{fileID}, password, expiryDays, maxViews, permission)
}

// CreateShareFiles creates one link covering every id in fileIDs. A single id
// keeps the historical shape (FileID only); two or more also write a row per
// item, ordered the way the caller sent them, which is what the public page
// lists.
func (s *ShareService) CreateShareFiles(ctx context.Context, userID uuid.UUID, fileIDs []uuid.UUID, password string, expiryDays, maxViews int, permission string) (*model.ShareLink, error) {
	ids := make([]uuid.UUID, 0, len(fileIDs))
	seen := make(map[uuid.UUID]struct{}, len(fileIDs))
	for _, id := range fileIDs {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, ErrShareNotFound
	}

	// A file can only carry one share link at a time so the popup never
	// accumulates a stack of links. That rule now has to look inside bundles
	// too: sharing B as part of [A B] must count as B already being shared.
	// A lapsed link is invisible in every listing, so it is replaced rather
	// than refused — otherwise the member would have nothing they could delete.
	for _, id := range ids {
		var existing model.ShareLink
		err := s.db.WithContext(ctx).
			Where("file_id = ? OR id IN (?)", id,
				s.db.Model(&model.ShareLinkItem{}).Select("share_link_id").Where("file_id = ?", id)).
			First(&existing).Error
		switch {
		case err == nil:
			if existing.ExpiresAt == nil || time.Now().Before(*existing.ExpiresAt) {
				return nil, ErrShareAlreadyExists
			}
			if delErr := s.db.WithContext(ctx).Delete(&existing).Error; delErr != nil {
				return nil, delErr
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			// Nothing to replace.
		default:
			return nil, err
		}
	}

	if expiryDays < 0 {
		// Sentinel for "no expiry": leave ExpiresAt nil so the link
		// lives until the member deletes the file or the share itself.
	} else if expiryDays == 0 {
		expiryDays = s.defaultExpiry
	}
	if expiryDays > s.maxExpiry {
		expiryDays = s.maxExpiry
	}
	if maxViews <= 0 && s.maxViews > 0 {
		maxViews = s.maxViews
	}
	if permission != PermissionWrite {
		permission = PermissionRead
	}

	// The policy is a floor, not a hint. The branch below used to be entered
	// whenever requirePassword was set, but hashPassword("") returns an empty
	// string, and ValidateShare only checks a password when PasswordHash is
	// non-empty — so a client that simply omitted the field produced an
	// unprotected link while the operator believed the policy was in force.
	if password == "" && s.requirePassword {
		return nil, ErrPasswordRequired
	}

	var passwordHash string
	if password != "" {
		hash, err := hashPassword(password)
		if err != nil {
			return nil, err
		}
		passwordHash = hash
	}

	token, err := s.generateToken()
	if err != nil {
		return nil, err
	}

	var expiresAt *time.Time
	if expiryDays > 0 {
		t := time.Now().AddDate(0, 0, expiryDays)
		expiresAt = &t
	}

	share := &model.ShareLink{
		UserID:       userID,
		FileID:       ids[0],
		Token:        token,
		PasswordHash: passwordHash,
		ExpiresAt:    expiresAt,
		MaxViews:     maxViews,
		IsActive:     true,
		Permission:   permission,
	}

	// One transaction: a link whose item rows half-failed would open a page
	// that lists a different set than the owner chose.
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(share).Error; err != nil {
			return err
		}
		if len(ids) < 2 {
			return nil
		}
		for i, id := range ids {
			item := &model.ShareLinkItem{
				ShareLinkID: share.ID,
				FileID:      id,
				Position:    i,
			}
			if err := tx.Create(item).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return share, nil
}

func (s *ShareService) GetShareByToken(ctx context.Context, token string) (*model.ShareLink, error) {
	var share model.ShareLink
	if err := s.db.WithContext(ctx).
		Preload("User").
		Preload("File").
		Preload("Items", func(db *gorm.DB) *gorm.DB {
			return db.Order("position ASC")
		}).
		Preload("Items.File").
		Where("token = ? AND is_active = ?", token, true).
		First(&share).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrShareNotFound
		}
		return nil, err
	}
	return &share, nil
}

func (s *ShareService) ValidateShare(ctx context.Context, token, password string) (*model.ShareLink, error) {
	share, err := s.GetShareByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	if share.ExpiresAt != nil && time.Now().After(*share.ExpiresAt) {
		return nil, ErrShareExpired
	}

	if share.MaxViews > 0 && share.ViewCount >= share.MaxViews {
		return nil, ErrShareMaxViews
	}

	if share.PasswordHash != "" {
		if !checkPassword(password, share.PasswordHash) {
			return nil, ErrInvalidPassword
		}
	}

	// The target has to still be a live file. Nothing checked this, so a link
	// created before a file was trashed went on serving it — the payload now
	// lives under .yuarcloud-trash/, and every public download, preview and
	// browse endpoint followed it there. Trashing is supposed to take the item
	// out of circulation; a share link quietly undid that, for anyone holding
	// the token, with no expiry and no view cap involved.
	//
	// A multi-item link is judged by its set: the first item may have been
	// trashed while the rest are still shared, and the page can simply leave
	// that one out. Only a set with nothing live left is gone.
	if len(share.Items) == 0 {
		if share.File.ID == uuid.Nil || share.File.IsTrashed {
			return nil, ErrShareNotFound
		}
	} else if len(LiveShareFiles(share)) == 0 {
		return nil, ErrShareNotFound
	}

	// And so does the account behind it. Preload silently yields a zero-valued
	// User for a soft-deleted owner, so deletion revoked nothing; the account
	// is gone and its links are still public. The zero id is the deleted case,
	// not a reason to skip the check.
	if share.User.ID == uuid.Nil || !share.User.IsActive {
		return nil, ErrShareNotFound
	}

	return share, nil
}

// IncrementViewCount consumes one view, and refuses when the cap is spent.
//
// The cap is enforced in the UPDATE rather than in a check that runs before it.
// They used to be separate statements: N concurrent requests all read
// view_count = max-1, all passed, and all incremented, so a link with
// max_views: 1 could be pulled as many times as the caller cared to fan out.
// Folding the predicate into the write makes the database the arbiter — the
// loser of the race matches no row and is told the limit is reached.
func (s *ShareService) IncrementViewCount(ctx context.Context, shareID uuid.UUID) error {
	res := s.db.WithContext(ctx).
		Model(&model.ShareLink{}).
		Where("id = ? AND (max_views <= 0 OR view_count < max_views)", shareID).
		UpdateColumn("view_count", gorm.Expr("view_count + 1"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrShareMaxViews
	}
	return nil
}

// Link lifecycle states. The rules live here, next to ValidateShare, so the
// UI can never drift from what the server actually enforces.
const (
	StatusActive      = "active"
	StatusInactive    = "inactive"
	StatusExpired     = "expired"
	StatusLimitNeeded = "limit_reached"
)

// Share permissions. A "read" link only lets visitors fetch the item;
// a "write" link on a folder also lets them upload into it.
const (
	PermissionRead  = "read"
	PermissionWrite = "write"
)

// Share token shape: six random alphanumeric characters, e.g. aZ3kPq.
// Tokens used to carry a "yc" prefix and then a forced leading digit, but that
// put the product name in twice — once in the path (/yc/…) and once in the code
// itself — and constrained a position nothing needed constrained. Lookup is an
// exact match on the stored string; no code parses the shape.
const (
	shareTokenLength = 6
)

// LiveShareFiles is the item set a public request may serve: the rows of a
// multi-item link, in the owner's order, with anything trashed or vanished
// left out. A single-item link has no rows and falls back to its File.
func LiveShareFiles(s *model.ShareLink) []model.File {
	if len(s.Items) == 0 {
		if s.File.ID == uuid.Nil || s.File.IsTrashed {
			return nil
		}
		return []model.File{s.File}
	}
	files := make([]model.File, 0, len(s.Items))
	for _, item := range s.Items {
		if item.File.ID == uuid.Nil || item.File.IsTrashed {
			continue
		}
		files = append(files, item.File)
	}
	return files
}

// Status reports whether a link can still be opened, using the same checks as
// ValidateShare.
func Status(s *model.ShareLink) string {
	switch {
	case !s.IsActive:
		return StatusInactive
	case s.ExpiresAt != nil && time.Now().After(*s.ExpiresAt):
		return StatusExpired
	case s.MaxViews > 0 && s.ViewCount >= s.MaxViews:
		return StatusLimitNeeded
	default:
		return StatusActive
	}
}

// liveShares is the one definition of a link worth listing. A link past its
// date cannot be opened — ValidateShare refuses it — so keeping it in the list
// only offers the owner a URL to copy that is guaranteed not to work.
func liveShares(db *gorm.DB) *gorm.DB {
	return db.Where("expires_at IS NULL OR expires_at > ?", time.Now())
}

func (s *ShareService) GetUserShares(ctx context.Context, userID uuid.UUID, page, pageSize int, fileID *uuid.UUID) ([]*model.ShareLink, int64, error) {
	var shares []*model.ShareLink
	var total int64

	scoped := liveShares(s.db.WithContext(ctx).Model(&model.ShareLink{})).Where("user_id = ?", userID)
	if fileID != nil {
		scoped = scoped.Where("file_id = ?", *fileID)
	}

	// Count on its own statement — Count() cannot share a statement with Preload.
	if err := scoped.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	query := liveShares(s.db.WithContext(ctx).Model(&model.ShareLink{})).Where("user_id = ?", userID)
	if fileID != nil {
		query = query.Where("file_id = ?", *fileID)
	}

	offset := (page - 1) * pageSize
	if err := query.
		Preload("File").
		Preload("Items", func(db *gorm.DB) *gorm.DB {
			return db.Order("position ASC")
		}).
		Preload("Items.File").
		Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&shares).Error; err != nil {
		return nil, 0, err
	}

	return shares, total, nil
}

// GetAllShares lists every share link on the instance, newest first, with the
// shared file and its owner preloaded. Admin-only — GetUserShares is the
// scoped counterpart a regular user sees.
func (s *ShareService) GetAllShares(ctx context.Context, page, pageSize int) ([]*model.ShareLink, int64, error) {
	var shares []*model.ShareLink
	var total int64

	// Count on its own statement — Count() cannot share one with Preload.
	if err := liveShares(s.db.WithContext(ctx).Model(&model.ShareLink{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	if err := liveShares(s.db.WithContext(ctx).Model(&model.ShareLink{})).
		Preload("File").
		Preload("Items", func(db *gorm.DB) *gorm.DB {
			return db.Order("position ASC")
		}).
		Preload("Items.File").
		// Select the three owner columns inside a closure. Passing bare strings
		// as trailing args to Preload does not restrict the columns — they are
		// folded into the query's conditions, which then match no row, and the
		// owner comes back blank with no error to show for it.
		Preload("User", func(db *gorm.DB) *gorm.DB {
			return db.Select("id", "username", "display_name")
		}).
		Order("created_at DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&shares).Error; err != nil {
		return nil, 0, err
	}

	return shares, total, nil
}

// DeleteShareByID removes a share link regardless of who owns it. Admin-only
// — DeleteShare is the owner-scoped counterpart, and it is what the per-user
// endpoint uses so a user can never reach someone else's link.
func (s *ShareService) DeleteShareByID(ctx context.Context, shareID uuid.UUID) error {
	return s.db.WithContext(ctx).
		Where("id = ?", shareID).
		Delete(&model.ShareLink{}).Error
}

func (s *ShareService) DeleteShare(ctx context.Context, userID, shareID uuid.UUID) error {
	return s.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", shareID, userID).
		Delete(&model.ShareLink{}).Error
}

// URLPath prefixes every public share link. Links used to live under
// /s/<token>; the route is now /yc/<token> to match the product name. The
// frontend still redirects /s/ for links handed out before the move.
const URLPath = "/yc/"

// ShareURL builds the public share URL for a token. When no explicit base
// URL is configured, it is derived from the incoming request, so the link is
// always a complete URL that recipients can actually open.
func (s *ShareService) ShareURL(token string, r *http.Request) string {
	if s.baseURL != "" {
		return strings.TrimSuffix(s.baseURL, "/") + URLPath + token
	}
	return baseURLFromRequest(r) + URLPath + token
}

// baseURLFromRequest derives the external base URL (scheme + host) from an
// incoming request, honoring a reverse proxy's X-Forwarded-Proto header.
func baseURLFromRequest(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host
}

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", nil
	}
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func checkPassword(password, hash string) bool {
	if hash == "" {
		return true
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
