package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

// subtreeHarness builds a handler over a real directory, with the unique index
// on files(path) that production enforces.
//
// The test harnesses elsewhere AutoMigrate the models directly, which creates no
// such index — and a copy of a folder then happily produced two rows at one
// path, which is exactly what the index exists to prevent. Testing against a
// database shaped like the real one is the point.
func subtreeHarness(t *testing.T) (*FileHandler, string, []model.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// collisionHarness already points the handler at its own temp directory;
	// reuse it so the bytes written below are the bytes the handler operates on.
	h, db, root := collisionHarness(t)

	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The production index, applied to the harness database.
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_path_unique
		ON files(path) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("create index: %v", err)
	}

	users := []model.User{
		{Username: "owner", Email: "owner@example.com", PasswordHash: "x", Role: "user", QuotaBytes: 1 << 30, IsActive: true},
		{Username: "mate", Email: "mate@example.com", PasswordHash: "x", Role: "user", QuotaBytes: 1 << 30, IsActive: true},
	}
	for i := range users {
		if err := db.Create(&users[i]).Error; err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	return h, root, users
}

func seedSharedFolder(t *testing.T, h *FileHandler, root string, users []model.User) (folder model.File) {
	t.Helper()
	db := h.db

	if err := db.Create(&model.File{
		UserID: users[0].ID, Name: "proj", Path: "proj", IsDir: true, StoragePath: "proj",
	}).Error; err != nil {
		t.Fatalf("create folder: %v", err)
	}
	folder = model.File{}
	if err := db.Where("path = ?", "proj").First(&folder).Error; err != nil {
		t.Fatalf("load folder: %v", err)
	}

	// One child owned by each account: confinement here is by path, so a shared
	// folder holds rows from several owners and every subtree operation has to
	// move all of them.
	for _, u := range users {
		name := "f-" + u.Username + ".txt"
		p := "proj/" + name
		if err := db.Create(&model.File{
			UserID: u.ID, Name: name, Path: p, Size: 7, StoragePath: p,
		}).Error; err != nil {
			t.Fatalf("create child: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("content"), 0o644); err != nil {
			t.Fatalf("write child: %v", err)
		}
	}
	return folder
}

func act(t *testing.T, h *FileHandler, method, target string, user model.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	// Split the id off the path so the handlers see it as :id, matching main.go.
	segments := strings.Split(strings.Trim(target, "/"), "/")
	idSegment := ""
	for _, seg := range segments {
		if strings.Contains(seg, "-") && len(seg) == 36 {
			idSegment = seg
		}
	}
	if idSegment == "" {
		t.Fatalf("no id in target %q", target)
	}
	verb := segments[len(segments)-1]

	r := gin.New()
	r.Handle(method, "/:id/"+verb, func(c *gin.Context) {
		c.Set(middleware.UserIDContextKey, user.ID)
		c.Set(middleware.RoleContextKey, user.Role)
		c.Set(middleware.ScopeContextKey, scope.New(""))
		switch verb {
		case "rename":
			h.RenameFile(c)
		case "move":
			h.MoveFile(c)
		case "copy":
			h.CopyFile(c)
		default:
			c.Status(http.StatusNotFound)
		}
	})

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("{}")
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// Moving or renaming a folder rewrites the paths of every row beneath it, and
// the read that finds them has to happen *outside* the transaction. On SQLite a
// read through a second pooled connection while the transaction holds the write
// lock comes back SQLITE_BUSY, GORM's Find swallows it, and the loop rewrites
// nothing — so the bytes move and every descendant row keeps a path that no
// longer exists. The subtree then cannot be listed, opened, moved or deleted,
// because none of those can find it.
func TestSubtreeOperationsRewriteEveryOwnersPaths(t *testing.T) {
	h, root, users := subtreeHarness(t)
	folder := seedSharedFolder(t, h, root, users)

	t.Run("rename", func(t *testing.T) {
		rec := act(t, h, http.MethodPatch, "/"+folder.ID.String()+"/rename", users[0], `{"name":"renamed"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("rename -> %d %s", rec.Code, rec.Body.String())
		}
		for _, u := range users {
			var n int64
			h.db.Model(&model.File{}).Where("path = ?", "renamed/f-"+u.Username+".txt").Count(&n)
			if n != 1 {
				t.Errorf("%s's file was not rewritten by the rename (rows at the new path: %d)", u.Username, n)
			}
		}
		// The bytes must be where the rows now say they are.
		if _, err := os.Stat(filepath.Join(root, "renamed", "f-mate.txt")); err != nil {
			t.Errorf("the on-disk subtree did not move with the rows: %v", err)
		}
		// Undo, so the next subtest starts from a known shape.
		rec = act(t, h, http.MethodPatch, "/"+folder.ID.String()+"/rename", users[0], `{"name":"proj"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("rename back -> %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("move", func(t *testing.T) {
		// A destination the owner can address by id.
		if err := os.MkdirAll(filepath.Join(root, "dest"), 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		var dest model.File
		if err := h.db.Create(&model.File{
			UserID: users[0].ID, Name: "dest", Path: "dest", IsDir: true, StoragePath: "dest",
		}).Error; err != nil {
			t.Fatalf("create dest: %v", err)
		}
		if err := h.db.Where("path = ?", "dest").First(&dest).Error; err != nil {
			t.Fatalf("load dest: %v", err)
		}

		rec := act(t, h, http.MethodPost, "/"+folder.ID.String()+"/move", users[0],
			`{"target_parent_id":"`+dest.ID.String()+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("move -> %d %s", rec.Code, rec.Body.String())
		}
		for _, u := range users {
			var n int64
			h.db.Model(&model.File{}).Where("path = ?", "dest/proj/f-"+u.Username+".txt").Count(&n)
			if n != 1 {
				t.Errorf("%s's file was not rewritten by the move (rows at the new path: %d)", u.Username, n)
			}
			var stale int64
			h.db.Model(&model.File{}).Where("path = ?", "proj/f-"+u.Username+".txt").Count(&stale)
			if stale != 0 {
				t.Errorf("%s's file still points at the old path", u.Username)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "dest", "proj", "f-mate.txt")); err != nil {
			t.Errorf("the on-disk subtree did not move with the rows: %v", err)
		}
	})
}

// Every clone has to land inside the new root. The clone loop used to include
// the root itself, because the "path = root OR path LIKE root/%" match that
// finds a subtree also matches the subtree's own row — and the user_id filter
// that used to hide that was removed so shared folders would work.
func TestCopyDoesNotCloneTheSourceOntoItself(t *testing.T) {
	h, root, users := subtreeHarness(t)
	folder := seedSharedFolder(t, h, root, users)

	rec := act(t, h, http.MethodPost, "/"+folder.ID.String()+"/copy", users[1], `{"target_path":""}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("copy -> %d %s", rec.Code, rec.Body.String())
	}

	var copies []model.File
	if err := h.db.Where("path = ? OR path LIKE ?", "proj (2)", "proj (2)/%").Order("path").Find(&copies).Error; err != nil {
		t.Fatalf("query copies: %v", err)
	}

	var paths []string
	for _, c := range copies {
		paths = append(paths, c.Path)
	}
	want := []string{"proj (2)", "proj (2)/f-mate.txt", "proj (2)/f-owner.txt"}
	if len(paths) != len(want) {
		t.Fatalf("copy produced %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("copy produced %v, want %v", paths, want)
		}
	}
	for _, c := range copies {
		if c.UserID != users[1].ID {
			t.Errorf("clone of %q is owned by %s, want the copier", c.Path, c.UserID)
		}
	}

	// And the source is still exactly what it was.
	var original []model.File
	h.db.Where("path = ? OR path LIKE ?", "proj", "proj/%").Find(&original)
	if len(original) != 3 {
		t.Errorf("the source subtree now has %d rows, want 3", len(original))
	}

	// The bytes must exist for every clone, or the row points at nothing.
	for _, c := range copies {
		if c.IsDir {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, c.Path)); err != nil {
			t.Errorf("no bytes at the clone's path %q: %v", c.Path, err)
		}
	}
	_ = context.Background()
}
