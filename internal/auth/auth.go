package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

var (
	ErrInvalidToken      = errors.New("invalid token")
	ErrExpiredToken      = errors.New("token expired")
	ErrUserInactive      = errors.New("user inactive")
	ErrWrongTokenPurpose = errors.New("token is not valid for this use")
)

// Token purposes. A JWT signed with the server's secret is not automatically
// good for everything — the purpose decides where a token may be spent, which
// is the only thing keeping a preview token from authorising an API call.
const (
	PurposeAccess  = "access"
	PurposeRefresh = "refresh"
	PurposePreview = "preview"
)

// PreviewTokenTTL is deliberately short. A preview token ends up in a URL,
// which means access logs and browser history, so its usefulness after leaking
// is kept to the length of one viewing session.
const PreviewTokenTTL = 5 * time.Minute

type Claims struct {
	UserID   uuid.UUID `json:"user_id"`
	Username string    `json:"username"`
	Role     string    `json:"role"`
	// Purpose scopes the token. Empty means a token issued before this field
	// existed, which predates preview tokens and is treated as an access token.
	Purpose string `json:"purpose,omitempty"`
	// FileID is set on preview tokens only, and pins them to a single file.
	FileID uuid.UUID `json:"file_id,omitempty"`
	// TokenVersion mirrors the user row. Checked on every request that resolves a
	// user, so bumping the row revokes everything already issued.
	TokenVersion int `json:"tv,omitempty"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type AuthService struct {
	jwtSecret  []byte
	tokenTTL   time.Duration
	refreshTTL time.Duration
	bcryptCost int
	issuer     string
}

func NewAuthService(secret string, tokenTTL, refreshTTL time.Duration, bcryptCost int, issuer string) *AuthService {
	return &AuthService{
		jwtSecret:  []byte(secret),
		tokenTTL:   tokenTTL,
		refreshTTL: refreshTTL,
		bcryptCost: bcryptCost,
		issuer:     issuer,
	}
}

func (s *AuthService) HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	return string(bytes), err
}

func (s *AuthService) CheckPassword(password, hash string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// DummyPasswordCheck spends the same bcrypt time as CheckPassword and always
// fails. The login handler calls it when no account matched, so a request
// against a username that does not exist costs what a request against one that
// does costs. Without it the response time alone separates the two, which is
// enough to enumerate every username on the instance with no error message
// involved.
func (s *AuthService) DummyPasswordCheck(password string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// A valid bcrypt digest of a value nobody knows, at the configured cost. It
// only has to be well-formed and expensive.
var dummyHash = []byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.7DKQ7CxSbLuKQpFWTVtL0zwsO6Fb.i5")

func (s *AuthService) GenerateTokenPair(userID uuid.UUID, username, role string, tokenVersion int) (*TokenPair, error) {
	now := time.Now()
	accessExpires := now.Add(s.tokenTTL)
	refreshExpires := now.Add(s.refreshTTL)

	accessClaims := &Claims{
		UserID:       userID,
		Username:     username,
		Role:         role,
		Purpose:      PurposeAccess,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(accessExpires),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			Subject:   userID.String(),
			ID:        uuid.New().String(),
		},
	}

	refreshClaims := &Claims{
		UserID:       userID,
		Username:     username,
		Role:         role,
		Purpose:      PurposeRefresh,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(refreshExpires),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			Subject:   userID.String(),
			ID:        uuid.New().String(),
		},
	}

	accessToken := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessTokenString, err := accessToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}

	refreshToken := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshTokenString, err := refreshToken.SignedString(s.jwtSecret)
	if err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:  accessTokenString,
		RefreshToken: refreshTokenString,
		ExpiresIn:    int64(s.tokenTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

func (s *AuthService) ValidateToken(tokenString string) (*Claims, error) {
	claims, err := s.parse(tokenString)
	if err != nil {
		return nil, err
	}

	// Only an access token authorises an API call. Both of the other purposes
	// used to slip through, because the check only rejected preview:
	//
	//   - a refresh token (168h) used as an access token, which quietly doubled
	//     the life of any credential
	//   - and, via RefreshAccessToken below, an access token (24h) exchanged for
	//     a fresh 168h refresh token, repeatable forever — so shortening the
	//     access TTL bought nothing at all
	//
	// An empty purpose means a token issued before the field existed; those are
	// treated as access tokens, and expire on their own schedule anyway.
	if claims.Purpose != "" && claims.Purpose != PurposeAccess {
		return nil, ErrWrongTokenPurpose
	}

	return claims, nil
}

// ValidatePreviewToken accepts a token only if it is a preview token minted
// for exactly fileID. Both checks matter: the purpose keeps it away from the
// API, and the file binding stops one leaked URL from being edited to read a
// different file.
func (s *AuthService) ValidatePreviewToken(tokenString string, fileID uuid.UUID) (*Claims, error) {
	claims, err := s.parse(tokenString)
	if err != nil {
		return nil, err
	}

	if claims.Purpose != PurposePreview {
		return nil, ErrWrongTokenPurpose
	}
	if claims.FileID != fileID {
		return nil, ErrWrongTokenPurpose
	}

	return claims, nil
}

// GeneratePreviewToken mints a short-lived, single-file token for the viewer.
func (s *AuthService) GeneratePreviewToken(userID uuid.UUID, username, role string, fileID uuid.UUID) (string, error) {
	now := time.Now()
	claims := &Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		Purpose:  PurposePreview,
		FileID:   fileID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(PreviewTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			Subject:   userID.String(),
			ID:        uuid.New().String(),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
	if err != nil {
		return "", err
	}
	return signed, nil
}

func (s *AuthService) parse(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return s.jwtSecret, nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, ErrInvalidToken
}

// RefreshToken re-reads the account before minting anything.
//
// It used to call GenerateTokenPair with the *old token's* claims, which meant
// the refresh path never consulted the users table at all: an admin who had
// been demoted, or deleted, kept minting fresh admin tokens from the refresh
// token they were already holding, and since each refresh issued a new 168h
// one, that was a sliding window with no end. It also meant a deleted account
// could keep a session alive indefinitely.
//
// db is optional in the signature but not in practice — main.go always supplies
// it, and a nil db would restore the old behaviour, so callers must not pass
// nil for a server that serves real users.
func (s *AuthService) RefreshAccessTokenWithDB(db *gorm.DB, refreshTokenString string) (*TokenPair, error) {
	claims, err := s.parse(refreshTokenString)
	if err != nil {
		return nil, err
	}

	// Refresh or bust. Without this, an access token — the short-lived one —
	// could be exchanged for a brand new 168h refresh token, and repeating that
	// turned a 24h credential into an unlimited one. Empty is the pre-purpose
	// legacy shape and is accepted; anything else must say "refresh".
	if claims.Purpose != "" && claims.Purpose != PurposeRefresh {
		return nil, ErrWrongTokenPurpose
	}

	if db == nil {
		return nil, ErrInvalidToken
	}

	var user model.User
	if err := db.First(&user, "id = ? AND is_active = ?", claims.UserID, true).Error; err != nil {
		// Deleted, gone, or deactivated: there is nobody to issue credentials for.
		return nil, ErrUserInactive
	}

	// A refresh token minted before the account's last authority change is dead.
	// Without this, changing a password or being demoted did nothing to a
	// session the holder already had, and refresh would keep renewing it — the
	// one action an incident responder needs to be able to take.
	if claims.TokenVersion != user.TokenVersion {
		return nil, ErrInvalidToken
	}

	// Authority comes from the row, never from the token being presented.
	return s.GenerateTokenPair(user.ID, user.Username, user.Role, user.TokenVersion)
}

func (s *AuthService) ExtractTokenFromHeader(authHeader string) (string, error) {
	const prefix = "Bearer "
	if len(authHeader) < len(prefix) || authHeader[:len(prefix)] != prefix {
		return "", ErrInvalidToken
	}
	return authHeader[len(prefix):], nil
}
