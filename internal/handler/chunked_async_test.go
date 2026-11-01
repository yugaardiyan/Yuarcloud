package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// Completion of a large upload must not be held inside the request.
//
// Cloudflare closes a tunnelled response after roughly 100–125 seconds, and
// assembling a multi-gigabyte file can take longer than that (especially on
// spinning disks). The handler answers 202 and reports the outcome through the
// status endpoint instead. The threshold is lowered here so a tiny payload
// exercises the same path a 20–50 GB upload takes.
func TestLargeChunkedUploadCompletesInBackground(t *testing.T) {
	previous := chunkedAsyncAssemblyThreshold
	chunkedAsyncAssemblyThreshold = 0
	defer func() { chunkedAsyncAssemblyThreshold = previous }()

	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "assembled in the background"
	uploadID := startChunkedUpload(t, h, user, "async.txt", payload)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost,
		"/api/files/upload/chunked/"+uploadID+"/complete", nil)
	c.Params = gin.Params{{Key: "uploadId", Value: uploadID}}
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.CompleteChunkedUpload(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("complete status = %d, want %d; body %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var status map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		sw := httptest.NewRecorder()
		sc, _ := gin.CreateTestContext(sw)
		sc.Request = httptest.NewRequest(http.MethodGet,
			"/api/files/upload/chunked/"+uploadID+"/status", nil)
		sc.Params = gin.Params{{Key: "uploadId", Value: uploadID}}
		sc.Set(middleware.UserIDContextKey, user)
		sc.Set(middleware.ScopeContextKey, scope.New(""))
		h.GetUploadStatus(sc)

		if sw.Code != http.StatusOK {
			t.Fatalf("status code = %d, want 200; body %s", sw.Code, sw.Body.String())
		}
		if err := json.Unmarshal(sw.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}

		switch status["status"] {
		case "completed":
			goto done
		case "failed":
			t.Fatalf("background assembly failed: %v", status["error"])
		}

		if time.Now().After(deadline) {
			t.Fatalf("upload still %v after 5s", status["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}

done:
	file, ok := status["file"].(map[string]any)
	if !ok {
		t.Fatalf("completed status has no file record: %v", status)
	}
	if file["name"] != "async.txt" {
		t.Fatalf("file name = %v, want async.txt", file["name"])
	}

	got, err := os.ReadFile(filepath.Join(root, "async.txt"))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("assembled %q, want %q", got, payload)
	}

	var session model.UploadSession
	if err := db.First(&session, "id = ?", uploadID).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	if session.Status != "completed" || session.FileID == nil {
		t.Fatalf("session = %s, file_id = %v; want completed with a file", session.Status, session.FileID)
	}

	var stored model.User
	if err := db.First(&stored, "id = ?", user).Error; err != nil {
		t.Fatalf("load user: %v", err)
	}
	if stored.UsedBytes != int64(len(payload)) {
		t.Fatalf("used_bytes = %d, want %d", stored.UsedBytes, len(payload))
	}
}
