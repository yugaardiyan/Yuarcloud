package handler

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// The plain upload learned this lesson and refuses the name before it writes
// anything (TestUploadDoesNotOverwriteAnExistingFile). The chunked path did
// not: it accepted the session, took every chunk, and then AssembleChunks
// renamed its result over the target path. The unique index on files(path)
// rejected the row, and the failure path removed the path — so a name collision
// destroyed the file that was already there and left its row pointing at
// nothing, downloading as a 404.
//
// Both ends are checked here. Refusing at init is the courtesy: the client
// learns immediately instead of after sending the file. Refusing again at
// completion is the part that actually protects the data, because the name can
// be claimed by something else while the upload is in flight.
func TestChunkedUploadDoesNotOverwriteAnExistingFile(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	const original = "ORIGINAL CONTENT"
	onDisk := filepath.Join(root, "report.txt")
	if err := os.WriteFile(onDisk, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	row := model.File{
		ID: uuid.New(), UserID: user, Name: "report.txt", Path: "report.txt",
		Size: int64(len(original)), MimeType: "text/plain",
		StorageType: "local", StoragePath: "report.txt",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = initChunkedRequest(t, "report.txt", int64(len("REPLACEMENT")))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.InitChunkedUpload(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("init status = %d, want 409; body %s", w.Code, w.Body.String())
	}

	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("the pre-existing file was deleted: %v", err)
	}
	if string(got) != original {
		t.Fatalf("the pre-existing file was overwritten with %q", got)
	}
}

// The same guard at the end, for the case init cannot catch: the name is taken
// after the session exists but before the chunks are assembled.
func TestChunkedCompleteDoesNotStealANameClaimedMidUpload(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	const payload = "REPLACEMENT"

	// The name is free when the session opens.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = initChunkedRequest(t, "report.txt", int64(len(payload)))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.InitChunkedUpload(c)
	if w.Code != http.StatusOK {
		t.Fatalf("init status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode init: %v", err)
	}

	wc := httptest.NewRecorder()
	cc, _ := gin.CreateTestContext(wc)
	cc.Request = chunkRequest(t, resp.UploadID, []byte(payload))
	cc.Params = gin.Params{{Key: "uploadId", Value: resp.UploadID}, {Key: "chunkIndex", Value: "0"}}
	cc.Set(middleware.UserIDContextKey, user)
	cc.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadChunk(cc)
	if wc.Code != http.StatusOK {
		t.Fatalf("chunk status = %d, want 200; body %s", wc.Code, wc.Body.String())
	}

	// Somebody else claims report.txt while the upload is still in flight.
	onDisk := filepath.Join(root, "report.txt")
	const theirs = "THEIR CONTENT"
	if err := os.WriteFile(onDisk, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	theirRow := model.File{
		ID: uuid.New(), UserID: user, Name: "report.txt", Path: "report.txt",
		Size: int64(len(theirs)), MimeType: "text/plain",
		StorageType: "local", StoragePath: "report.txt",
	}
	if err := db.Create(&theirRow).Error; err != nil {
		t.Fatal(err)
	}

	wf := httptest.NewRecorder()
	cf, _ := gin.CreateTestContext(wf)
	cf.Request = httptest.NewRequest(http.MethodPost, "/api/files/upload/chunked/"+resp.UploadID+"/complete", nil)
	cf.Params = gin.Params{{Key: "uploadId", Value: resp.UploadID}}
	cf.Set(middleware.UserIDContextKey, user)
	cf.Set(middleware.ScopeContextKey, scope.New(""))
	h.CompleteChunkedUpload(cf)

	if wf.Code != http.StatusConflict {
		t.Fatalf("complete status = %d, want 409; body %s", wf.Code, wf.Body.String())
	}

	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("the file claimed mid-upload was deleted: %v", err)
	}
	if string(got) != theirs {
		t.Fatalf("the file claimed mid-upload was overwritten with %q", got)
	}

	var rows int64
	db.Model(&model.File{}).Where("path = ?", "report.txt").Count(&rows)
	if rows != 1 {
		t.Fatalf("%d rows for report.txt, want 1", rows)
	}
}

// The guards must not become a blanket refusal: a name nobody holds still
// completes, and it is the one file at that path when it does.
func TestChunkedUploadStillAcceptsAFreshName(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	const payload = "brand new"
	uploadID := startChunkedUpload(t, h, user, "novel.txt", payload)

	wf := httptest.NewRecorder()
	cf, _ := gin.CreateTestContext(wf)
	cf.Request = httptest.NewRequest(http.MethodPost, "/api/files/upload/chunked/"+uploadID+"/complete", nil)
	cf.Params = gin.Params{{Key: "uploadId", Value: uploadID}}
	cf.Set(middleware.UserIDContextKey, user)
	cf.Set(middleware.ScopeContextKey, scope.New(""))
	h.CompleteChunkedUpload(cf)

	if wf.Code != http.StatusCreated && wf.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 2xx; body %s", wf.Code, wf.Body.String())
	}

	got, err := os.ReadFile(filepath.Join(root, "novel.txt"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("stored %q, want %q", got, payload)
	}
}

func initChunkedRequest(t *testing.T, filename string, total int64) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"filename":     filename,
		"total_size":   total,
		"chunk_size":   1048576,
		"total_chunks": 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload/chunked/init", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func chunkRequest(t *testing.T, uploadID string, payload []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("chunk", "part")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/files/upload/chunked/"+uploadID+"/chunk/0", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// startChunkedUpload opens a session and pushes its single chunk, returning the
// upload id ready for CompleteChunkedUpload.
func startChunkedUpload(t *testing.T, h *FileHandler, user uuid.UUID, name, payload string) string {
	t.Helper()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = initChunkedRequest(t, name, int64(len(payload)))
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.InitChunkedUpload(c)
	if w.Code != http.StatusOK {
		t.Fatalf("init status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var resp struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode init: %v", err)
	}

	wc := httptest.NewRecorder()
	cc, _ := gin.CreateTestContext(wc)
	cc.Request = chunkRequest(t, resp.UploadID, []byte(payload))
	cc.Params = gin.Params{{Key: "uploadId", Value: resp.UploadID}, {Key: "chunkIndex", Value: "0"}}
	cc.Set(middleware.UserIDContextKey, user)
	cc.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadChunk(cc)
	if wc.Code != http.StatusOK {
		t.Fatalf("chunk status = %d, want 200; body %s", wc.Code, wc.Body.String())
	}
	return resp.UploadID
}
