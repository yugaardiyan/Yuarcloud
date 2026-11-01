package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Chunked upload sends one request per chunk, so a request-per-minute bucket
// applied to those requests caps the largest possible file at
// requestsPerMinute × chunkSize. At the shipped 120/min and 10 MB that is
// ~1.2 GB — the exact ceiling a 2 GB test hit at chunk 118. The skip predicate
// is what keeps the bucket off the transfer routes without turning it off for
// everything else.
func TestRateLimitMiddlewareSkipsTransferRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(RateLimitMiddleware(1, false, func(c *gin.Context) bool {
		return c.Request.URL.Path == "/upload/chunk"
	}))
	router.POST("/upload/chunk", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/api/files", func(c *gin.Context) { c.Status(http.StatusOK) })

	do := func(method, path string) int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}

	// Many multiples of the one-per-minute budget: every exempt request passes.
	for i := 0; i < 5; i++ {
		if got := do(http.MethodPost, "/upload/chunk"); got != http.StatusOK {
			t.Fatalf("transfer request %d = %d, want %d", i, got, http.StatusOK)
		}
	}

	// Everything else still draws from the same bucket.
	if got := do(http.MethodGet, "/api/files"); got != http.StatusOK {
		t.Fatalf("first API request = %d, want %d", got, http.StatusOK)
	}
	if got := do(http.MethodGet, "/api/files"); got != http.StatusTooManyRequests {
		t.Fatalf("second API request = %d, want %d", got, http.StatusTooManyRequests)
	}
}
