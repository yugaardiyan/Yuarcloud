package handler

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
)

func scopeTestHandler(t *testing.T) (*FileHandler, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.AuditLog{}, &model.UploadSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewFileHandler(db, nil, nil), db
}

// The LIKE metacharacters that appear in ordinary folder names are not exotic.
// `_` is in my_files, photos_2024 and data_1 — and without an explicit ESCAPE
// clause SQLite matches none of them, so every subtree operation silently did
// nothing. These pin that the escaping is actually in force.
func TestSubtreeMatchEscapesUnderscore(t *testing.T) {
	h, db := scopeTestHandler(t)
	user := quotaUser(t, db, 0, 1000)

	seed := func(path string, size int64) model.File {
		f := model.File{
			ID: uuid.New(), UserID: user, Name: path, Path: path,
			Size: size, MimeType: "text/plain", StorageType: "local", StoragePath: path,
		}
		if err := db.Create(&f).Error; err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		return f
	}

	seed("my_files", 0)
	seed("my_files/a.txt", 500)
	seed("my_files/nested/b.txt", 700)
	// Siblings whose names merely start with the same characters. The escape
	// clause must not widen the match to them.
	seed("my_files_backup/c.txt", 900)
	// A folder whose name is itself a LIKE metacharacter.
	seed("100%", 0)
	seed("100%/d.txt", 300)
	// `my_files` starts with `my`, which is itself a metacharacter pair: an
	// unescaped `my_files` would treat `_` as "any single character" and drag
	// in `myXfiles`.
	seed("myXfiles/e.txt", 400)

	t.Run("underscore", func(t *testing.T) {
		got := h.descendantsOf(user, "my_files")
		names := map[string]bool{}
		for _, d := range got {
			names[d.Path] = true
		}
		if !names["my_files/a.txt"] || !names["my_files/nested/b.txt"] {
			t.Fatalf("underscore folder lost its subtree; got %v", names)
		}
		if names["my_files_backup/c.txt"] || names["myXfiles/e.txt"] {
			t.Fatalf("the unescaped `_` acted as a wildcard; got %v", names)
		}
	})

	t.Run("percent", func(t *testing.T) {
		var rows []model.File
		whereSubtree(db.Where("user_id = ?", user), "path", "100%").Find(&rows)
		if len(rows) != 2 { // the folder row and its child
			t.Fatalf("percent folder matched %d rows, want 2 (itself + child)", len(rows))
		}
	})

	t.Run("size", func(t *testing.T) {
		if got := h.subtreeSize(user, "my_files"); got != 1200 {
			t.Fatalf("subtreeSize(my_files) = %d, want 1200", got)
		}
	})
}

// A home must not capture a sibling whose name merely shares its prefix. This
// is the app's primary security boundary: ScopePattern used to emit
// `Home + "%"`, so "team" matched "team-archive".
func TestScopeDoesNotCaptureSiblingWithSharedPrefix(t *testing.T) {
	sc := scope.New("team")

	inside := []string{"team", "team/x.txt", "team/deep/nested/y.txt"}
	for _, p := range inside {
		if !sc.Contains(p) {
			t.Errorf("Contains(%q) = false, want true", p)
		}
	}

	outside := []string{"team-archive", "team-archive/z.txt", "teammates", "team2", "other/team/x"}
	for _, p := range outside {
		if sc.Contains(p) {
			t.Errorf("Contains(%q) = true; a sibling sharing the prefix leaked into the scope", p)
		}
	}
}

// And the same has to hold at the SQL layer, which is what actually gates the
// database. Contains is only a Go-side helper; scoped() is the real filter.
func TestScopedQueryExcludesSiblingFolders(t *testing.T) {
	_, db := scopeTestHandler(t)
	owner := quotaUser(t, db, 0, 1000)
	other := quotaUser(t, db, 0, 1000)

	mk := func(userID uuid.UUID, path string) {
		f := model.File{
			ID: uuid.New(), UserID: userID, Name: path, Path: path,
			IsDir: true, StorageType: "local", StoragePath: path,
		}
		if err := db.Create(&f).Error; err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	mk(owner, "team")
	mk(owner, "team/mine.txt")
	mk(other, "team-archive")
	mk(other, "team-archive/theirs.txt")
	mk(other, "teammates")
	mk(other, "teammates/also-theirs.txt")

	sc := scope.New("team")
	var got []model.File
	scoped(sc, db).Find(&got)

	for _, f := range got {
		if f.Path != "team" && f.Path != "team/mine.txt" {
			t.Fatalf("scoped() returned %q, which is outside the home", f.Path)
		}
	}
	if len(got) != 2 {
		t.Fatalf("scoped() returned %d rows, want 2 (the home row and its child)", len(got))
	}
}

// The home row itself must still be visible. ScopePattern matches only what is
// strictly below, so the equality arm has to carry the folder itself — losing
// it would drop the user's own home out of their own listing.
func TestScopedQueryIncludesTheHomeItself(t *testing.T) {
	_, db := scopeTestHandler(t)
	user := quotaUser(t, db, 0, 1000)

	f := model.File{
		ID: uuid.New(), UserID: user, Name: "home", Path: "home",
		IsDir: true, StorageType: "local", StoragePath: "home",
	}
	if err := db.Create(&f).Error; err != nil {
		t.Fatal(err)
	}

	var got []model.File
	scoped(scope.New("home"), db).Find(&got)
	if len(got) != 1 || got[0].Path != "home" {
		t.Fatalf("scoped() = %d rows, want the home row itself", len(got))
	}
}

func TestUnrestrictedScopeAddsNoClause(t *testing.T) {
	sc := scope.New("")
	if clause, args := sc.ScopeClause(); clause != "" || args != nil {
		t.Fatalf("ScopeClause() = %q, %v; want empty", clause, args)
	}
	if got := sc.ScopePattern(); got != "" {
		t.Fatalf("ScopePattern() = %q, want empty", got)
	}
}
