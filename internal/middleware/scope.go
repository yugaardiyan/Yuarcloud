package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

const ScopeContextKey = "scope"

// ScopeMiddleware resolves the caller's home directory into a scope.Scope and
// parks it on the request.
//
// It runs after AuthMiddleware and re-reads the user row rather than trusting
// a claim, because HomePath is mutable by an admin: changing someone's home
// has to take effect on their next request, not when their token is refreshed.
func ScopeMiddleware(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization header required"})
			return
		}

		var user model.User
		// is_active is part of the lookup, not just the comment. GORM's
		// soft-delete scoping already excludes a deleted account, so the query
		// looked like it did the job while quietly passing every row that had
		// been deactivated — and is_active was enforced in exactly one place in
		// the codebase, the login handler. A disabled account holding a live
		// token kept full access to its whole tree.
		if err := db.First(&user, "id = ? AND is_active = ?", userID, true).Error; err != nil {
			// The token is valid but the account is gone or deactivated;
			// either way there is no tree to hand out.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "account unavailable"})
			return
		}

		// Token version is what makes a password change revocation. A token
		// minted before the bump carries an older number and is refused here,
		// which covers every route at once instead of each having to remember.
		//
		// Preview tokens are the exception, and the exception is the point of
		// them. They are deliberately short-lived and already pinned to one
		// file, so they confer no authority a stolen access token would not —
		// and the media element holding one cannot re-mint it, so refusing it
		// does not strand the viewer either. Enforcing the version here meant
		// that changing your own password broke every image and video already
		// loaded on the page: the URLs were still valid for their remaining
		// four minutes but all of them answered 401, so a photo gallery
		// rendered as a wall of broken thumbnails until the user reloaded.
		// The account check above still applies — a deactivated or deleted
		// owner still cannot be previewed.
		if claims, ok := GetClaims(c); ok && claims.Purpose != auth.PurposePreview &&
			claims.TokenVersion != user.TokenVersion {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired, please sign in again"})
			return
		}

		c.Set(ScopeContextKey, scope.New(user.HomePath))
		c.Next()
	}
}

// GetScope returns the caller's directory scope. Handlers behind
// ScopeMiddleware can rely on it being present; the fallback keeps a
// programming mistake from turning into an unrestricted request.
func GetScope(c *gin.Context) scope.Scope {
	raw, exists := c.Get(ScopeContextKey)
	if !exists {
		return scope.New("")
	}
	s, ok := raw.(scope.Scope)
	if !ok {
		return scope.New("")
	}
	return s
}
