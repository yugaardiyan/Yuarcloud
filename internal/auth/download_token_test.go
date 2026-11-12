package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// A download token rides in a URL, so it must be useless as an API credential.
// Without the purpose check a leaked download link would be a full session.
func TestDownloadTokenIsNotAnAPIToken(t *testing.T) {
	s := newTestService()
	user := uuid.New()

	token, err := s.GenerateDownloadToken(user, "alice", "user", 0, uuid.New(), nil)
	if err != nil {
		t.Fatalf("GenerateDownloadToken: %v", err)
	}

	if _, err := s.ValidateToken(token); !errors.Is(err, ErrWrongTokenPurpose) {
		t.Fatalf("download token accepted as an API token (err = %v)", err)
	}
}

// The file id in the path is not a permission: the token has to name the same
// item, or a leaked URL could be edited to fetch something else.
func TestDownloadTokenIsBoundToOneItem(t *testing.T) {
	s := newTestService()
	user := uuid.New()
	file := uuid.New()
	other := uuid.New()

	token, err := s.GenerateDownloadToken(user, "alice", "user", 0, file, nil)
	if err != nil {
		t.Fatalf("GenerateDownloadToken: %v", err)
	}

	if _, err := s.ValidateDownloadToken(token, &file); err != nil {
		t.Fatalf("token rejected for its own item: %v", err)
	}
	if _, err := s.ValidateDownloadToken(token, &other); !errors.Is(err, ErrWrongTokenPurpose) {
		t.Fatalf("token accepted for a different item (err = %v)", err)
	}
}

// The batch token has no path id to bind to; the set it carries is its whole
// authority, and it must not be usable as a single-item token either.
func TestBatchDownloadTokenCarriesItsSet(t *testing.T) {
	s := newTestService()
	user := uuid.New()
	ids := []uuid.UUID{uuid.New(), uuid.New()}

	token, err := s.GenerateDownloadToken(user, "alice", "user", 0, uuid.Nil, ids)
	if err != nil {
		t.Fatalf("GenerateDownloadToken: %v", err)
	}

	claims, err := s.ValidateDownloadToken(token, nil)
	if err != nil {
		t.Fatalf("batch token rejected: %v", err)
	}
	if len(claims.FileIDs) != len(ids) {
		t.Fatalf("batch token carries %d ids, want %d", len(claims.FileIDs), len(ids))
	}
	for i := range ids {
		if claims.FileIDs[i] != ids[i] {
			t.Errorf("id %d = %s, want %s", i, claims.FileIDs[i], ids[i])
		}
	}

	// A batch token names no single item, so the single-item routes refuse it.
	single := uuid.Nil
	if _, err := s.ValidateDownloadToken(token, &single); !errors.Is(err, ErrWrongTokenPurpose) {
		t.Fatalf("batch token accepted for a single item (err = %v)", err)
	}
}

// The reverse direction: an access token must not unlock a download URL, or
// ?token= would have become a second way to spend the session credential.
func TestAccessTokenIsNotADownloadToken(t *testing.T) {
	s := newTestService()
	pair, err := s.GenerateTokenPair(uuid.New(), "alice", "user", 0)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}

	file := uuid.New()
	if _, err := s.ValidateDownloadToken(pair.AccessToken, &file); !errors.Is(err, ErrWrongTokenPurpose) {
		t.Fatalf("access token accepted as a download token (err = %v)", err)
	}
}

func TestDownloadTokenExpiry(t *testing.T) {
	s := newTestService()

	now := time.Now()
	claims := &Claims{
		UserID:  uuid.New(),
		Purpose: PurposeDownload,
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

	file := claims.FileID
	if _, err := s.ValidateDownloadToken(expired, &file); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expired download token accepted (err = %v)", err)
	}
}

// A password change bumps token_version. ScopeMiddleware compares it on every
// request, so a download token minted before the change must stop working —
// that is what makes the bump a real revocation rather than a future-login one.
func TestDownloadTokenCarriesTokenVersion(t *testing.T) {
	s := newTestService()

	token, err := s.GenerateDownloadToken(uuid.New(), "alice", "user", 7, uuid.New(), nil)
	if err != nil {
		t.Fatalf("GenerateDownloadToken: %v", err)
	}

	if got := decodeClaims(t, s, token).TokenVersion; got != 7 {
		t.Fatalf("token version = %d, want 7", got)
	}
}
