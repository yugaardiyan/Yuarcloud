package storage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// dotfileStorage puts the storage root somewhere readable and returns it
// alongside, so a test can plant real files on disk and then list them.
func dotfileStorage(t *testing.T) (*LocalStorage, string) {
	t.Helper()

	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.Local.Root = root
	cfg.Storage.Chunked.TempDir = t.TempDir()
	cfg.Storage.Chunked.MaxFileSize = 10 * 1024 * 1024
	cfg.Storage.Chunked.MaxChunkSize = 1024 * 1024
	cfg.Storage.Chunked.CleanupInterval = time.Hour

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return ls, root
}

func names(files []*FileInfo) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// A dot-prefixed file is not automatically the server's.
//
// Listing skipped every entry whose name began with a dot, on the assumption
// that the dot marked internal bookkeeping. The server does not use that
// naming — everything it owns is prefixed ".yuarcloud" — so the rule hid a
// user's own dotfiles. Uploading .env returned success, wrote the file, and
// then showed an empty folder.
func TestListShowsUserDotfiles(t *testing.T) {
	ls, root := dotfileStorage(t)

	for _, name := range []string{".env", ".gitignore", ".htaccess", "Dokumen.pdf", "README"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := ls.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := []string{".env", ".gitignore", ".htaccess", "Dokumen.pdf", "README"}
	sort.Strings(want)

	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("List returned %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("List returned %v, want %v", gotNames, want)
		}
	}
}

// Narrowing the rule must not have widened it the other way: the trash and the
// chunk scratch directory still sit beside user data in the container layout
// and must not appear.
func TestListStillHidesInternalDirectories(t *testing.T) {
	ls, root := dotfileStorage(t)

	for _, dir := range []string{
		".yuarcloud-trash",
		".yuarcloud-chunks",
		".yuarcloud-anything-else",
		".yuarcloud",
	} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".yuarcloud-part"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write part file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keepme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write visible: %v", err)
	}

	got, err := ls.List(context.Background(), "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	gotNames := names(got)
	if len(gotNames) != 1 || gotNames[0] != "keepme.txt" {
		t.Fatalf("List returned %v, want [keepme.txt]", gotNames)
	}
}

// The prefix is shared with the handler package, which refuses to serve these
// names. If the two ever drift, a client could address the trash again — so
// assert the value both sides compile against.
func TestInternalPrefixValue(t *testing.T) {
	if InternalPrefix != ".yuarcloud" {
		t.Fatalf("InternalPrefix = %q, want %q", InternalPrefix, ".yuarcloud")
	}
}

// A file inside the trash still lists when asked for by path, which is how the
// trash view reads it. Only the top-level sweep is filtered.
func TestListInsideInternalDirStillWorks(t *testing.T) {
	ls, root := dotfileStorage(t)

	trash := filepath.Join(root, ".yuarcloud-trash")
	if err := os.Mkdir(trash, 0o700); err != nil {
		t.Fatalf("mkdir trash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(trash, "gone.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ls.List(context.Background(), ".yuarcloud-trash")
	if err != nil {
		t.Fatalf("List inside trash: %v", err)
	}
	if len(got) != 1 || got[0].Name != "gone.txt" {
		t.Fatalf("List inside trash returned %v, want [gone.txt]", names(got))
	}
}
