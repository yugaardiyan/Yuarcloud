package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

// PasswordRotationRequiredMiddleware confines an account that is still on the
// shipped default password to the endpoints it has to reach.
//
// It returns 403 with a machine-readable marker on everything else, including
// every admin route. Without it the flag was decorative: the seeded
// `admin`/`admin` account had full panel access over a network, which is the
// same as no access control at all — that credential is published in the
// repository, the example config and the container image, so anyone who found
// the port already had it.
//
// Three routes stay open, and no more:
//   - POST /api/auth/refresh, or the panel boots, fails its first call and
//     bounces the owner straight back to the login screen
//   - POST /api/auth/change-password, the way out
//   - POST /api/auth/logout, so the session can be ended
//
// Unauthenticated callers are untouched: this only constrains accounts that
// exist and are still flagged.
func PasswordRotationRequiredMiddleware(db *gorm.DB, exempt map[string]bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			c.Next()
			return
		}

		if exempt[c.Request.Method+" "+c.FullPath()] {
			c.Next()
			return
		}

		// One primary-key read per request — cheap next to the authorisation
		// work the routes do anyway. Caching it across requests would mean the
		// flag could outlive the password change that cleared it, which is the
		// one moment it must not.
		var flagged int64
		if err := db.WithContext(c.Request.Context()).
			Model(&model.User{}).
			Where("id = ? AND must_rotate_pass = ?", userID, true).
			Count(&flagged).Error; err != nil || flagged == 0 {
			// A lookup failure must not lock the owner out of the one endpoint
			// that fixes it. Fail open here: the account is still protected by
			// every other control, and a transient error should not strand the
			// only account that can administer the server.
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error":                "change your password before continuing",
			"must_rotate_password": true,
		})
	}
}
