package handler

import (
	"bytes"
	"encoding/json"
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

// A trashed row is looked up without a scope on purpose, so a peer's item in a
// shared home can be deleted. That lookup used to be the whole check: any
// authenticated user who knew a trashed UUID could permanently destroy another
// account's bytes.
func TestDeletePermanentlyRejectsAnotherUsersTrash(t *testing.T) {
	h, db, root := collisionHarness(t)
	alice := quotaUser(t, db, 0, 1000)
	bob := quotaUser(t, db, 0, 1000)

	id := uuid.New()
	trashPath := ".yuarcloud-trash/" + id.String() + "/secret.txt"
	onDisk := filepath.Join(root, filepath.FromSlash(trashPath))
	if err := os.MkdirAll(filepath.Dir(onDisk), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onDisk, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	row := model.File{
		ID: id, UserID: alice, Name: "secret.txt", Path: trashPath,
		OriginalPath: "alice-home/secret.txt", IsTrashed: true,
		Size: 6, StorageType: "local", StoragePath: trashPath,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/files/"+id.String()+"/permanent", nil)
	c.Params = gin.Params{{Key: "id", Value: id.String()}}
	c.Set(middleware.UserIDContextKey, bob)
	c.Set(middleware.ScopeContextKey, scope.New("bob-home"))
	h.DeletePermanently(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("another account's trashed file was deleted: %v", err)
	}
}

// The row-only collision check let a rename replace a file that exists on the
// volume but has no row yet — an un-adopted library item, an external sync.
func TestRenameRefusesUnindexedDestination(t *testing.T) {
	h, db, root := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	const untouched = "DO NOT LOSE ME"
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte(untouched), 0o644); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	row := model.File{
		ID: id, UserID: user, Name: "a.txt", Path: "a.txt",
		Size: 1, StorageType: "local", StoragePath: "a.txt",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"name": "b.txt"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/files/"+id.String()+"/rename", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: id.String()}}
	c.Set(middleware.UserIDContextKey, user)
	c.Set(middleware.ScopeContextKey, scope.New(""))
	h.RenameFile(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "b.txt"))
	if err != nil || string(got) != untouched {
		t.Fatalf("the unindexed destination was overwritten: %q, %v", got, err)
	}
}

// A session declares a total size and a chunk size, but nothing forced each
// part to be that size: a 10-byte upload could write max_chunk_size per chunk
// while the quota reserved only 10 bytes.
func TestChunkedUploadRejectsWrongChunkSize(t *testing.T) {
	h, db, _ := collisionHarness(t)
	user := quotaUser(t, db, 0, 1000)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = initChunkedRequest(t, "small.bin", 10)
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
		t.Fatal(err)
	}

	wc := httptest.NewRecorder()
	cc, _ := gin.CreateTestContext(wc)
	// Declared 10 bytes; send 5.
	cc.Request = chunkRequest(t, resp.UploadID, []byte("12345"))
	cc.Params = gin.Params{{Key: "uploadId", Value: resp.UploadID}, {Key: "chunkIndex", Value: "0"}}
	cc.Set(middleware.UserIDContextKey, user)
	cc.Set(middleware.ScopeContextKey, scope.New(""))
	h.UploadChunk(cc)

	if wc.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", wc.Code, wc.Body.String())
	}
}
