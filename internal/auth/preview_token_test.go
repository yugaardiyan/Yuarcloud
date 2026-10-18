package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func newTestService() *AuthService {
	return NewAuthService("test-secret", time.Hour, 24*time.Hour, 4, "yuarcloud")
}

func decodeClaims(t *testing.T, s *AuthService, token string) *Claims {
	t.Helper()
	claims, err := s.parse(token)
	if err != nil {
		t.Fatalf("token failed to parse: %v", err)
	}
	return claims
}

// The whole point of a preview token is that it is NOT a session token. If
// one can be replayed against the API, the URL it travels in stops being a
// low-value leak and becomes a full account compromise.
func TestPreviewTokenIsNotAnAPIToken(t *testing.T) {
	s := newTestService()
	user := uuid.New()
	file := uuid.New()

	pt, err := s.GeneratePreviewToken(user, "alice", "user", file)
	if err != nil {
		t.Fatalf("GeneratePreviewToken: %v", err)
	}

	if _, err := s.ValidateToken(pt); err != ErrWrongTokenPurpose {
		t.Fatalf("preview token accepted as an API token (err = %v)", err)
	}
}

// ...and a preview token must be pinned to the file it was minted for, or a
// leaked URL could simply be edited to point at something else.
func TestPreviewTokenIsBoundToOneFile(t *testing.T) {
	s := newTestService()
	user := uuid.New()
	file := uuid.New()
	other := uuid.New()

	pt, err := s.GeneratePreviewToken(user, "alice", "user", file)
	if err != nil {
		t.Fatalf("GeneratePreviewToken: %v", err)
	}

	if _, err := s.ValidatePreviewToken(pt, file); err != nil {
		t.Fatalf("token rejected for its own file: %v", err)
	}
	if _, err := s.ValidatePreviewToken(pt, other); err != ErrWrongTokenPurpose {
		t.Fatalf("token accepted for a different file (err = %v)", err)
	}
}

// The reverse direction: an ordinary session token must not unlock a preview
// URL, otherwise ?token= would have silently become a second access path.
func TestAccessTokenIsNotAPreviewToken(t *testing.T) {
	s := newTestService()
	user := uuid.New()

	pair, err := s.GenerateTokenPair(user, "alice", "user", 0)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}

	if _, err := s.ValidatePreviewToken(pair.AccessToken, uuid.New()); err != ErrWrongTokenPurpose {
		t.Fatalf("access token accepted as a preview token (err = %v)", err)
	}
	if _, err := s.ValidateToken(pair.AccessToken); err != nil {
		t.Fatalf("access token rejected by ValidateToken: %v", err)
	}
}

func TestPreviewTokenExpiry(t *testing.T) {
	// A service whose preview TTL has already elapsed.
	s := NewAuthService("test-secret", time.Hour, 24*time.Hour, 4, "yuarcloud")
	s.jwtSecret = []byte("test-secret")

	now := time.Now()
	claims := &Claims{
		UserID:  uuid.New(),
		Purpose: PurposePreview,
		FileID:  uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Hour)),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Hour)),
			Issuer:    "yuarcloud",
		},
	}

	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := s.ValidatePreviewToken(expired, claims.FileID); err != ErrExpiredToken {
		t.Fatalf("expired preview token accepted (err = %v)", err)
	}
}

func TestIssuedTokensCarryTheirPurpose(t *testing.T) {
	s := newTestService()
	user := uuid.New()

	pair, err := s.GenerateTokenPair(user, "alice", "user", 0)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}

	if got := decodeClaims(t, s, pair.AccessToken).Purpose; got != PurposeAccess {
		t.Errorf("access token purpose = %q, want %q", got, PurposeAccess)
	}
	if got := decodeClaims(t, s, pair.RefreshToken).Purpose; got != PurposeRefresh {
		t.Errorf("refresh token purpose = %q, want %q", got, PurposeRefresh)
	}
}
