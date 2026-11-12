package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

const (
	UserContextKey   = "user"
	UserIDContextKey = "user_id"
	RoleContextKey   = "role"
)

func AuthMiddleware(authService *auth.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		tokenString, err := authService.ExtractTokenFromHeader(authHeader)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}

		claims, err := authService.ValidateToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}

		c.Set(UserContextKey, claims)
		c.Set(UserIDContextKey, claims.UserID)
		c.Set(RoleContextKey, claims.Role)
		c.Next()
	}
}

// AuthPreviewMiddleware guards the inline preview routes.
//
// A browser hands <img>, <video>, <audio> and <iframe> a bare URL and cannot
// attach an Authorization header, so something has to travel in the query
// string. What travels is NOT the session access token — that one is valid for
// every endpoint and has no business sitting in an access log. It is a
// dedicated preview token: five minutes old, pinned to one file, and rejected
// by ValidateToken everywhere else.
//
// A normal Authorization header still works here (the fetch-based previewers
// use it), and it is checked first so a header always wins over a URL token.
func AuthPreviewMiddleware(authService *auth.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if header := c.GetHeader("Authorization"); header != "" {
			tokenString, err := authService.ExtractTokenFromHeader(header)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			claims, err := authService.ValidateToken(tokenString)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			setClaims(c, claims)
			c.Next()
			return
		}

		fileID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
			return
		}

		claims, err := authService.ValidatePreviewToken(c.Query("token"), fileID)
		if err != nil {
			// Deliberately the same response as a missing token: saying
			// "that is a valid token for a different file" would confirm
			// the file exists to someone holding only a leaked URL.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "preview token required"})
			return
		}

		setClaims(c, claims)
		c.Next()
	}
}

// AuthDownloadMiddleware guards the file download routes.
//
// It is the same idea as AuthPreviewMiddleware: a browser following a download
// URL cannot send a header, so the URL carries a dedicated, five-minute token
// instead of the session credential. The token is refused by ValidateToken and
// by every route that is not a download, and here it is additionally bound to
// the item named in the path (nil on the batch route, where the token carries
// the id set itself).
//
// A normal Authorization header is checked first, so the existing API clients
// and tests keep working unchanged.
func AuthDownloadMiddleware(authService *auth.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if header := c.GetHeader("Authorization"); header != "" {
			tokenString, err := authService.ExtractTokenFromHeader(header)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			claims, err := authService.ValidateToken(tokenString)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
			setClaims(c, claims)
			c.Next()
			return
		}

		// No id in the path is the batch route: the set of items is a claim.
		var expected *uuid.UUID
		if raw := c.Param("id"); raw != "" {
			fileID, err := uuid.Parse(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
				return
			}
			expected = &fileID
		}

		claims, err := authService.ValidateDownloadToken(c.Query("token"), expected)
		if err != nil {
			// Same response as a missing token: naming the reason would tell
			// someone holding only a leaked URL that the file exists.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "download token required"})
			return
		}

		setClaims(c, claims)
		c.Next()
	}
}

func setClaims(c *gin.Context, claims *auth.Claims) {
	c.Set(UserContextKey, claims)
	c.Set(UserIDContextKey, claims.UserID)
	c.Set(RoleContextKey, claims.Role)
}

// AdminOnlyMiddleware authorises against the database, not against the token.
//
// The role used to be read from the JWT claim and nothing ever contradicted
// it: AuthMiddleware never touches the users table, and this middleware sat
// behind it with no scope middleware in front. Demoting an admin wrote the new
// role to the row and left the account exactly as powerful as before for the
// life of the token — and because RefreshAccessToken re-minted from the old
// refresh token's claims, refreshing extended that for the full refresh TTL.
// Deleting the account did nothing either. Revocation was a suggestion with a
// seven-day fuse.
//
// Re-reading the row costs one indexed primary-key lookup per admin request,
// which is the price of a role change taking effect when it is made.
func AdminOnlyMiddleware(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		var user model.User
		if err := db.Select("role", "token_version").First(&user, "id = ?", userID).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		// A token predating the account's last authority change is refused here
		// as well, so a demotion or a password change takes effect on every
		// route and not only on the ones that happen to re-read the row.
		if claims, ok := GetClaims(c); ok && claims.TokenVersion != user.TokenVersion {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired, please sign in again"})
			return
		}
		if user.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}

		// Keep the context honest for anything downstream that reads it.
		c.Set(RoleContextKey, user.Role)
		c.Next()
	}
}

func GetUserID(c *gin.Context) (uuid.UUID, bool) {
	userID, exists := c.Get(UserIDContextKey)
	if !exists {
		return uuid.Nil, false
	}
	id, ok := userID.(uuid.UUID)
	return id, ok
}

func GetUserRole(c *gin.Context) (string, bool) {
	role, exists := c.Get(RoleContextKey)
	if !exists {
		return "", false
	}
	r, ok := role.(string)
	return r, ok
}

func GetClaims(c *gin.Context) (*auth.Claims, bool) {
	raw, exists := c.Get(UserContextKey)
	if !exists {
		return nil, false
	}
	claims, ok := raw.(*auth.Claims)
	return claims, ok
}

// CORS middleware is handled by gin-contrib/cors
// RateLimitMiddleware implements a simple in-memory token bucket limiter.
// Replace with Redis-backed limiting when running multiple instances.
//
// skip, when non-nil, marks the requests that must not consume the bucket.
// File transfers are the reason it exists: a chunked upload is one request per
// chunk by design — a 2 GB file is 205 of them — and counting those against the
// same per-minute budget as API calls made every upload over roughly 1.2 GB
// stop part-way with a 429. My own 2 GB test failed at chunk 118 that way.
// A transfer is still bounded, by authentication, the per-chunk body limit, the
// configured file-size ceiling and the owner's quota; the bucket was never what
// made it safe.
func RateLimitMiddleware(requestsPerMinute int, trustProxy bool, skip func(*gin.Context) bool) gin.HandlerFunc {
	type client struct {
		count     int
		resetTime int64
	}

	var mu sync.Mutex
	clients := make(map[string]*client)

	// Entries are dropped as they expire. They used to only ever be replaced,
	// never deleted, so the map grew one entry per distinct key forever — and
	// the key is a client-controlled address, which turns that from a slow leak
	// into unauthenticated memory exhaustion. Sweeping opportunistically on each
	// call keeps it self-limiting with no background goroutine to shut down.
	sweep := func(now int64) {
		for ip, entry := range clients {
			if now > entry.resetTime {
				delete(clients, ip)
			}
		}
	}

	return func(c *gin.Context) {
		if requestsPerMinute <= 0 || (skip != nil && skip(c)) {
			c.Next()
			return
		}

		// RemoteIP by default, ClientIP only when the operator has named their
		// proxies. ClientIP honours X-Forwarded-For, and gin trusts every source
		// unless SetTrustedProxies says otherwise — keying on it blindly handed
		// every request a fresh bucket by rotating a header. But the reverse was
		// also wrong: behind a proxy every request shares one RemoteIP, so one
		// client's traffic rate-limited everyone. With trusted proxies
		// configured, SetTrustedProxies has already restricted which sources
		// ClientIP will believe, so it is the accurate key there.
		ip := c.RemoteIP()
		if trustProxy {
			ip = c.ClientIP()
		}
		now := time.Now().Unix()

		mu.Lock()
		sweep(now)
		entry, exists := clients[ip]
		if !exists || now > entry.resetTime {
			entry = &client{count: 0, resetTime: now + 60}
			clients[ip] = entry
		}
		entry.count++
		count := entry.count
		retryAfter := entry.resetTime - now
		mu.Unlock()

		if count > requestsPerMinute {
			c.Header("Retry-After", strconv.Itoa(int(retryAfter)))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate limit exceeded",
				"retry_after": retryAfter,
			})
			return
		}

		c.Next()
	}
}
