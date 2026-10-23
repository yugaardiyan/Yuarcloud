package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

// The browser reads its worker count from this endpoint. It has to report the
// stored setting, fall back to three when nobody has set one, and clamp a
// hand-edited row instead of handing the client an unbounded pool.
func TestUploadLimitsReportsConcurrentUploads(t *testing.T) {
	h, db, _ := collisionHarness(t)
	if err := db.AutoMigrate(&model.Setting{}); err != nil {
		t.Fatalf("migrate settings: %v", err)
	}

	read := func() int {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/files/upload-limits", nil)
		h.UploadLimits(c)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
		}
		var body struct {
			MaxConcurrentUploads int `json:"max_concurrent_uploads"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.MaxConcurrentUploads
	}

	if got := read(); got != defaultMaxConcurrentUploads {
		t.Fatalf("with no setting: %d concurrent uploads, want %d", got, defaultMaxConcurrentUploads)
	}

	if err := db.Create(&model.Setting{
		Key: settingKeyMaxConcurrentUploads, Value: "7",
	}).Error; err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if got := read(); got != 7 {
		t.Fatalf("stored setting: %d concurrent uploads, want 7", got)
	}

	if err := db.Model(&model.Setting{}).
		Where("key = ?", settingKeyMaxConcurrentUploads).
		Update("value", "50").Error; err != nil {
		t.Fatalf("update setting: %v", err)
	}
	if got := read(); got != maxAllowedConcurrentUploads {
		t.Fatalf("oversized setting: %d concurrent uploads, want clamp to %d", got, maxAllowedConcurrentUploads)
	}
}
