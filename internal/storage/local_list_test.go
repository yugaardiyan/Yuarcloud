package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// List has to stay blind to the server's own directories. That rule is what
// keeps .yuarcloud-trash out of the browser, and it is also the only thing
// standing between a user and a folder of half-written upload chunks: in the
// container layout the chunk directory is /srv/.yuarcloud-chunks, sitting right
// beside their files.
//
// It reads as a dot-prefix test but is not one — the filter is the `.yuarcloud`
// prefix, not a leading dot. See TestListShowsUserDotFolders for why that
// distinction is the whole fix.
func TestListHidesDotPrefixedEntries(t *testing.T) {
	root := t.TempDir()

	cfg := &config.Config{
		Storage: config.StorageConfig{
			Type:  "local",
			Local: config.LocalConfig{Root: root},
			// Point the scratch directory inside the storage root, which is what
			// the shipped container config does.
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	}

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, ".yuarcloud-trash"), 0o755); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".yuarcloud-chunks", "session-1"), 0o755); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Documents"), 0o755); err != nil {
		t.Fatalf("mkdir documents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	entries, err := ls.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name] = true
	}

	for _, hidden := range []string{".yuarcloud-trash", ".yuarcloud-chunks"} {
		if seen[hidden] {
			t.Errorf("List surfaced %q; internal directories must never appear as user files", hidden)
		}
	}
	for _, want := range []string{"Documents", "readme.txt"} {
		if !seen[want] {
			t.Errorf("List is missing %q; hiding dotfiles must not hide real content", want)
		}
	}
}

// A user-chosen name that starts with a dot is visible.
//
// This reverses a deliberate decision recorded here before: "a user-chosen
// folder that happens to start with a dot is also invisible. That is a
// deliberate trade: the alternative is exposing upload scratch and the trash."
//
// The trade is no longer necessary, because the reason for it is gone. Both
// internal directories are named `.yuarcloud-*`, and the handler already
// refuses to serve, create or address anything carrying that prefix
// (internal/handler/file.go, reservedName). Hiding every dot was a proxy for
// that prefix, taken by a layer that could not see it — and the proxy cost a
// user their dotfiles: uploading .env reported success, wrote the bytes, and
// then showed an empty folder.
//
// TestListHidesDotPrefixedEntries above is the guarantee that matters, and it
// still passes unchanged: the trash and the chunk directory stay invisible.
// This test exists to pin the other half — that narrowing the rule did not
// narrow it past what it should cover.
func TestListShowsUserDotFolders(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageConfig{
			Type:    "local",
			Local:   config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{TempDir: filepath.Join(root, ".yuarcloud-chunks")},
		},
	}
	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(root, ".hidden-notes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries, err := ls.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name] = true
	}
	if !seen[".hidden-notes"] {
		t.Fatal("a dot-prefixed folder the user made is missing; only server-owned names should be filtered")
	}
	// The narrow rule must still cover the prefix, or the next internal
	// directory added under it would surface without anyone updating this file.
	for _, hidden := range []string{".yuarcloud-trash", ".yuarcloud-chunks"} {
		if seen[hidden] {
			t.Fatalf("List surfaced %q; internal directories must never appear as user files", hidden)
		}
	}
}
