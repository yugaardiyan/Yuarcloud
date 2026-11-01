package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/config"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// tusCall invokes one tus handler with the same context the router would build.
func tusCall(t *testing.T, h *FileHandler, user uuid.UUID, method, id, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	path := "/api/files/tus"
	if id != "" {
		path += "/" + id
	}
	req := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	c.Request = req
	if id != "" {
		c.Params = gin.Params{{Key: "uploadId", Value: id}}
	}
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))

	switch method {
	case http.MethodPost:
		h.TusCreate(c)
	case http.MethodHead:
		h.TusHead(c)
	case http.MethodPatch:
		h.TusPatch(c)
	case http.MethodGet:
		h.TusStatus(c)
	case http.MethodDelete:
		h.TusDelete(c)
	default:
		t.Fatalf("unsupported method %s", method)
	}
	// gin flushes the accumulated status when the engine finishes a handler;
	// these tests call the handler directly and have to do it themselves.
	c.Writer.WriteHeaderNow()
	return w
}

func tusMetadata(name string) string {
	return "filename " + base64.StdEncoding.EncodeToString([]byte(name))
}

func tusCreate(t *testing.T, h *FileHandler, user uuid.UUID, name string, size int) string {
	t.Helper()

	w := tusCall(t, h, user, http.MethodPost, "", "", map[string]string{
		"Tus-Resumable":   "1.0.0",
		"Upload-Length":   strconv.Itoa(size),
		"Upload-Metadata": tusMetadata(name),
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body %s", w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if location == "" {
		t.Fatal("create did not return a Location")
	}
	if got := w.Header().Get("Upload-Offset"); got != "0" {
		t.Fatalf("create Upload-Offset = %q, want 0", got)
	}
	return filepath.Base(location)
}

func tusPatch(t *testing.T, h *FileHandler, user uuid.UUID, id string, offset int, payload string) *httptest.ResponseRecorder {
	t.Helper()

	return tusCall(t, h, user, http.MethodPatch, id, payload, map[string]string{
		"Tus-Resumable": "1.0.0",
		"Upload-Offset": strconv.Itoa(offset),
		"Content-Type":  "application/offset+octet-stream",
	})
}

// A tus upload must survive in slices: create, inspect, append, append the
// rest, and only then become a file. The resume half is the point of the whole
// protocol — the second PATCH is sent as if the client had just reloaded.
func TestTusUploadResumesAndCompletes(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "the quick brown fox jumps over the lazy dog"
	id := tusCreate(t, h, user, "resume.txt", len(payload))

	half := len(payload) / 2

	first := tusPatch(t, h, user, id, 0, payload[:half])
	if first.Code != http.StatusNoContent {
		t.Fatalf("first patch = %d, want 204; body %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("Upload-Offset"); got != strconv.Itoa(half) {
		t.Fatalf("first patch offset = %q, want %d", got, half)
	}

	// This is what a client does after a reload: ask where the server is.
	head := tusCall(t, h, user, http.MethodHead, id, "", map[string]string{"Tus-Resumable": "1.0.0"})
	if head.Code != http.StatusOK {
		t.Fatalf("head = %d, want 200", head.Code)
	}
	if got := head.Header().Get("Upload-Offset"); got != strconv.Itoa(half) {
		t.Fatalf("head offset = %q, want %d", got, half)
	}
	if got := head.Header().Get("Upload-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("head length = %q, want %d", got, len(payload))
	}

	second := tusPatch(t, h, user, id, half, payload[half:])
	if second.Code != http.StatusNoContent {
		t.Fatalf("second patch = %d, want 204; body %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Upload-Offset"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("second patch offset = %q, want %d", got, len(payload))
	}

	status := tusCall(t, h, user, http.MethodGet, id, "", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status.Code, status.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body["status"] != "completed" {
		t.Fatalf("status = %v, want completed (%v)", body["status"], body)
	}
	file, ok := body["file"].(map[string]any)
	if !ok {
		t.Fatalf("completed status has no file record: %v", body)
	}
	if file["name"] != "resume.txt" {
		t.Fatalf("file name = %v, want resume.txt", file["name"])
	}

	stored, err := os.ReadFile(filepath.Join(root, "resume.txt"))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if string(stored) != payload {
		t.Fatalf("stored %q, want %q", stored, payload)
	}

	var userRow model.User
	if err := db.First(&userRow, "id = ?", user).Error; err != nil {
		t.Fatalf("load user: %v", err)
	}
	if userRow.UsedBytes != int64(len(payload)) {
		t.Fatalf("used_bytes = %d, want %d", userRow.UsedBytes, len(payload))
	}
}

// A stale offset is not a failed upload: the server answers 409 with the offset
// it actually has, which is how a client recovers without starting over.
func TestTusPatchRejectsStaleOffset(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "0123456789"
	id := tusCreate(t, h, user, "offset.txt", len(payload))

	if w := tusPatch(t, h, user, id, 0, payload[:4]); w.Code != http.StatusNoContent {
		t.Fatalf("first patch = %d, want 204; body %s", w.Code, w.Body.String())
	}

	stale := tusPatch(t, h, user, id, 0, payload[4:])
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale patch = %d, want 409; body %s", stale.Code, stale.Body.String())
	}
	if got := stale.Header().Get("Upload-Offset"); got != "4" {
		t.Fatalf("conflict offset = %q, want 4", got)
	}
}

// The state that makes resume work lives in the database and on disk, not in
// the handler. A new handler over the same database and root — a restarted
// process — must still answer HEAD with the bytes the old one received.
func TestTusResumesAfterHandlerRestart(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "survives a restart between the two halves"
	id := tusCreate(t, h, user, "restart.txt", len(payload))

	half := len(payload) / 2
	if w := tusPatch(t, h, user, id, 0, payload[:half]); w.Code != http.StatusNoContent {
		t.Fatalf("first patch = %d, want 204; body %s", w.Code, w.Body.String())
	}

	store, err := storage.NewLocalStorage(&config.Config{
		Storage: config.StorageConfig{
			Type:    "local",
			Local:   config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	})
	if err != nil {
		t.Fatalf("restart storage: %v", err)
	}
	restarted := NewFileHandler(db, store, nil)

	head := tusCall(t, restarted, user, http.MethodHead, id, "", map[string]string{"Tus-Resumable": "1.0.0"})
	if head.Code != http.StatusOK {
		t.Fatalf("head after restart = %d, want 200", head.Code)
	}
	if got := head.Header().Get("Upload-Offset"); got != strconv.Itoa(half) {
		t.Fatalf("offset after restart = %q, want %d", got, half)
	}

	if w := tusPatch(t, restarted, user, id, half, payload[half:]); w.Code != http.StatusNoContent {
		t.Fatalf("second patch = %d, want 204; body %s", w.Code, w.Body.String())
	}

	stored, err := os.ReadFile(filepath.Join(root, "restart.txt"))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if string(stored) != payload {
		t.Fatalf("stored %q, want %q", stored, payload)
	}
}

// A PATCH may not write past Upload-Length: otherwise the declared size would
// bound every quota and size check while the file grew beyond it.
func TestTusRejectsWritingPastDeclaredLength(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	id := tusCreate(t, h, user, "overrun.txt", 4)

	w := tusPatch(t, h, user, id, 0, "far too many bytes")
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("overlong patch = %d, want 413; body %s", w.Code, w.Body.String())
	}

	head := tusCall(t, h, user, http.MethodHead, id, "", map[string]string{"Tus-Resumable": "1.0.0"})
	if got := head.Header().Get("Upload-Offset"); got != "4" {
		t.Fatalf("offset after overrun = %q, want 4", got)
	}
}

// A completed upload is not a resumable one. HEAD still answers with the final
// offset, but the custom status header tells our client to create a new upload
// rather than silently reuse the file that already exists.
// A crash between claiming an upload for finalisation and writing the completed
// row used to leave it unfinishable: PATCH refuses a "finalizing" row and the
// claim can only move "uploading" forward. Startup reconciliation resets it, and
// the status poll the client is already making finishes it.
func TestTusRecoversUploadStuckInFinalizing(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "bytes arrived, but the process died before the row did"
	id := tusCreate(t, h, user, "stuck.txt", len(payload))

	// Write the bytes without going through TusPatch, which would have
	// completed the upload inline.
	if _, err := h.storage.TusAppend(context.Background(), id, 0, int64(len(payload)), strings.NewReader(payload)); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The state a crash between claimTus and finalizeTusClaimed leaves behind.
	if err := db.Model(&model.TusUpload{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status": "finalizing",
		"offset": len(payload),
	}).Error; err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	h.ReconcileTus()

	status := tusCall(t, h, user, http.MethodGet, id, "", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status.Code, status.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if body["status"] != "completed" {
		t.Fatalf("status = %v, want completed (%v)", body["status"], body)
	}

	stored, err := os.ReadFile(filepath.Join(root, "stuck.txt"))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if string(stored) != payload {
		t.Fatalf("stored %q, want %q", stored, payload)
	}
}

func TestTusCompletedUploadIsNotResumed(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 0)

	const payload = "done"
	id := tusCreate(t, h, user, "done.txt", len(payload))
	if w := tusPatch(t, h, user, id, 0, payload); w.Code != http.StatusNoContent {
		t.Fatalf("patch = %d, want 204; body %s", w.Code, w.Body.String())
	}

	head := tusCall(t, h, user, http.MethodHead, id, "", map[string]string{"Tus-Resumable": "1.0.0"})
	if head.Code != http.StatusOK {
		t.Fatalf("head = %d, want 200", head.Code)
	}
	if got := head.Header().Get("X-Yuarcloud-Status"); got != "completed" {
		t.Fatalf("status header = %q, want completed", got)
	}
	if got := head.Header().Get("Upload-Offset"); got != fmt.Sprintf("%d", len(payload)) {
		t.Fatalf("offset = %q, want %d", got, len(payload))
	}
}
