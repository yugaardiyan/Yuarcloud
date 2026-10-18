package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// A quota change is expressed in terms of where the rows are *now*, and the
// operation that moves them runs in the same function. Whichever runs second
// finds nothing.
//
// Both were wrong in the same way. Trashing rewrote every descendant's path
// into the trash folder and only then asked how many bytes to release, so the
// answer was zero every time: trashing released no quota at all, and
// used_bytes ratcheted up until the account could not upload anything more.
// Restoring had it backwards the same way — it re-charged after the rows were
// back at their live paths, so restoring put live bytes into the tree without
// charging anyone, and cycling an item through the trash cost nothing while the
// disk filled up.
func TestTrashAndRestoreAdjustQuotaWhileTheRowsAreStillWhereTheyAreCounted(t *testing.T) {
	h, root, users := subtreeHarness(t)
	db := h.db

	owner, mate := users[0], users[1]
	seedSharedFolder(t, h, root, users)

	const size = 4096

	// Charge each member for their own file directly, so the assertion is about
	// the trash path rather than about upload bookkeeping.
	paths := map[uuid.UUID]string{}
	for _, u := range users {
		name := "f-" + u.Username + ".txt"
		p := "proj/" + name
		paths[u.ID] = p
		if err := db.Model(&model.File{}).Where("path = ?", p).
			Updates(map[string]any{"size": size, "user_id": u.ID}).Error; err != nil {
			t.Fatalf("size: %v", err)
		}
		if err := db.Model(&model.User{}).Where("id = ?", u.ID).
			UpdateColumn("used_bytes", size).Error; err != nil {
			t.Fatalf("charge: %v", err)
		}
	}

	r := gin.New()
	r.POST("/:id/trash", func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, owner.ID)
		c.Set(middleware.RoleContextKey, "user")
		c.Set(middleware.ScopeContextKey, scope.New(""))
		h.MoveToTrash(c)
	})
	r.POST("/:id/restore", func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, owner.ID)
		c.Set(middleware.RoleContextKey, "user")
		c.Set(middleware.ScopeContextKey, scope.New(""))
		h.RestoreFromTrash(c)
	})

	used := func(u model.User) int64 {
		var n int64
		db.Model(&model.User{}).Where("id = ?", u.ID).Select("used_bytes").Scan(&n)
		return n
	}

	var folderID string
	db.Model(&model.File{}).Where("path = ?", "proj").Pluck("id", &folderID)

	call := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/" + folderID + "/trash"); rec.Code != http.StatusOK {
		t.Fatalf("trash -> %d %s", rec.Code, rec.Body.String())
	}
	for _, u := range users {
		if n := used(u); n != 0 {
			t.Errorf("after trash, %s is charged %d bytes; trashed bytes must not count", u.Username, n)
		}
	}

	if rec := call("/" + folderID + "/restore"); rec.Code != http.StatusOK {
		t.Fatalf("restore -> %d %s", rec.Code, rec.Body.String())
	}
	for _, u := range users {
		if n := used(u); n != size {
			t.Errorf("after restore, %s is charged %d bytes, want %d", u.Username, n, size)
		}
	}

	// Every cycle must land in the same place, and the other member's file has
	// to come back each time — restoring with an owner filter on the rewrite
	// leaves their row pointing at a trash folder that no longer exists.
	for i := 0; i < 3; i++ {
		call("/" + folderID + "/trash")
		call("/" + folderID + "/restore")

		for _, u := range users {
			if n := used(u); n != size {
				t.Fatalf("cycle %d: %s charged %d, want %d", i+1, u.Username, n, size)
			}
		}
		var live int64
		db.Model(&model.File{}).Where("path = ? AND is_trashed = ?", paths[mate.ID], false).Count(&live)
		if live != 1 {
			t.Fatalf("cycle %d: %s's file did not come back", i+1, mate.Username)
		}
	}
}

// Permanently deleting a live file has to release quota, and it has to do it
// before the rows are gone — releaseSubtreeUsage measures by querying the
// subtree, so running it after the delete finds nothing and releases nothing.
func TestPermanentDeleteOfALiveFileReleasesQuota(t *testing.T) {
	h, root, users := subtreeHarness(t)
	db := h.db
	seedSharedFolder(t, h, root, users)
	owner := users[0]

	const size = 777
	if err := db.Model(&model.File{}).Where("path = ?", "proj/f-owner.txt").
		Updates(map[string]any{"size": size}).Error; err != nil {
		t.Fatalf("size: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", owner.ID).
		UpdateColumn("used_bytes", size).Error; err != nil {
		t.Fatalf("charge: %v", err)
	}

	var id string
	db.Model(&model.File{}).Where("path = ?", "proj/f-owner.txt").Pluck("id", &id)

	r := gin.New()
	r.DELETE("/:id", func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, owner.ID)
		c.Set(middleware.RoleContextKey, "user")
		c.Set(middleware.ScopeContextKey, scope.New(""))
		c.Request.URL.RawQuery = "permanent=true"
		h.DeleteFile(c)
	})

	req := httptest.NewRequest(http.MethodDelete, "/"+id+"?permanent=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete -> %d %s", rec.Code, rec.Body.String())
	}

	var n int64
	db.Model(&model.User{}).Where("id = ?", owner.ID).Select("used_bytes").Scan(&n)
	if n != 0 {
		t.Errorf("used_bytes is %d after deleting the only charged file, want 0", n)
	}
}
