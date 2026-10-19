package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/share"
)

// shareBrowseHarness builds a small tree:
//
//	Shared/            <- shared root
//	  Sub/             <- folder, direct child
//	    b.txt
//	  a.txt
//	  gone.txt         <- trashed
//	Other/             <- different tree, must never be listed
//	  c.txt
func shareBrowseHarness(t *testing.T) (*gin.Engine, *gorm.DB, model.User, model.File) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.ShareLink{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	owner := model.User{
		ID: uuid.New(), Username: "owner", Email: "owner@t.local",
		PasswordHash: "x", Role: "user", IsActive: true,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	root := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "Shared", Path: "Shared",
		IsDir: true, MimeType: "inode/directory",
	}
	if err := db.Create(&root).Error; err != nil {
		t.Fatalf("seed root: %v", err)
	}

	sub := model.File{
		ID: uuid.New(), UserID: owner.ID, ParentID: &root.ID, Name: "Sub",
		Path: "Shared/Sub", IsDir: true, MimeType: "inode/directory",
	}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("seed sub: %v", err)
	}

	rows := []model.File{
		{ID: uuid.New(), UserID: owner.ID, ParentID: &root.ID, Name: "a.txt", Path: "Shared/a.txt", Size: 3, MimeType: "text/plain"},
		{ID: uuid.New(), UserID: owner.ID, ParentID: &sub.ID, Name: "b.txt", Path: "Shared/Sub/b.txt", Size: 4, MimeType: "text/plain"},
		{ID: uuid.New(), UserID: owner.ID, ParentID: &root.ID, Name: "gone.txt", Path: "Shared/gone.txt", IsTrashed: true},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed child: %v", err)
		}
	}

	other := model.File{
		ID: uuid.New(), UserID: owner.ID, Name: "Other", Path: "Other",
		IsDir: true, MimeType: "inode/directory",
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := db.Create(&model.File{
		ID: uuid.New(), UserID: owner.ID, ParentID: &other.ID,
		Name: "c.txt", Path: "Other/c.txt", MimeType: "text/plain",
	}).Error; err != nil {
		t.Fatalf("seed other child: %v", err)
	}

	link := model.ShareLink{
		ID: uuid.New(), UserID: owner.ID, FileID: root.ID,
		Token: "tok", IsActive: true, Permission: "read",
	}
	if err := db.Create(&link).Error; err != nil {
		t.Fatalf("seed link: %v", err)
	}

	svc := share.NewShareService(db, "", 0, 0, false, 0)
	h := NewShareHandler(db, svc, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/shares/:token/children", h.ListSharedFolder)

	return router, db, owner, root
}

func getChildren(t *testing.T, router *gin.Engine, query string) (int, []model.FileResponse) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/shares/tok/children"+query, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		return w.Code, nil
	}

	var body struct {
		Items []model.FileResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}

	return w.Code, body.Items
}

func names(items []model.FileResponse) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}

	return out
}

func TestListSharedFolderShowsOnlyDirectChildren(t *testing.T) {
	router, _, _, _ := shareBrowseHarness(t)

	code, items := getChildren(t, router, "")
	if code != http.StatusOK {
		t.Fatalf("root listing: %d", code)
	}
	if got, want := names(items), []string{"Sub", "a.txt"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		// Folders sort first; gone.txt is trashed and b.txt is nested.
		t.Fatalf("root listing = %v, want %v", got, want)
	}
	for _, it := range items {
		if it.Path == "Shared/Sub/b.txt" {
			t.Fatalf("nested file leaked into direct children: %v", names(items))
		}
		if it.Name == "gone.txt" {
			t.Fatalf("trashed file listed: %v", names(items))
		}
	}
}

func TestListSharedFolderNavigatesIntoSubfolder(t *testing.T) {
	router, _, _, _ := shareBrowseHarness(t)

	code, items := getChildren(t, router, "?path=Shared%2FSub")
	if code != http.StatusOK {
		t.Fatalf("sub listing: %d", code)
	}
	if got := names(items); len(got) != 1 || got[0] != "b.txt" {
		t.Fatalf("sub listing = %v, want [b.txt]", got)
	}
}

func TestListSharedFolderRejectsEscapeAndFilePaths(t *testing.T) {
	router, _, _, _ := shareBrowseHarness(t)

	cases := []struct {
		name  string
		query string
		code  int
	}{
		{"sibling tree", "?path=Other", http.StatusNotFound},
		{"path traversal", "?path=Shared%2F..%2FOther", http.StatusNotFound},
		{"parent escape", "?path=..%2FShared", http.StatusNotFound},
		{"file instead of folder", "?path=Shared%2Fa.txt", http.StatusBadRequest},
		{"child file", "?path=Shared%2FSub%2Fb.txt", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := getChildren(t, router, tc.query)
			if code != tc.code {
				t.Fatalf("%s = %d, want %d", tc.name, code, tc.code)
			}
		})
	}
}

func TestCleanSharePath(t *testing.T) {
	cases := map[string]struct {
		out string
		ok  bool
	}{
		"":                {"", true},
		"Shared":          {"Shared", true},
		"/Shared/Sub/":    {"Shared/Sub", true},
		"Shared/Sub":      {"Shared/Sub", true},
		"Shared/../Other": {"", false},
		"Shared//Sub":     {"", false},
		"Shared/./Sub":    {"", false},
	}
	for in, want := range cases {
		out, ok := cleanSharePath(in)
		if out != want.out || ok != want.ok {
			t.Fatalf("cleanSharePath(%q) = (%q, %v), want (%q, %v)", in, out, ok, want.out, want.ok)
		}
	}
}
