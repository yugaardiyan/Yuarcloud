package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// A folder copy used to follow symlinks: os.Stat resolved the link, then
// os.Open copied the target's bytes into the destination. That turned an
// adopted library's link to /etc/passwd into a normal file in a copied folder,
// and a link to another directory into a recursive copy outside the root. The
// list and importer already refuse links; the copy path has to as well.
func TestCopySkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStorage(&config.Config{
		Storage: config.StorageConfig{
			Type:  "local",
			Local: config.LocalConfig{Root: root},
			Chunked: config.ChunkedConfig{
				TempDir: filepath.Join(root, ".yuarcloud-chunks"),
			},
		},
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	src := filepath.Join(root, "src")
	outside := t.TempDir()
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "escape")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".yuarcloud-chunks"), 0o755); err != nil {
		t.Fatalf("mkdir internal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".yuarcloud-chunks", "partial"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write internal: %v", err)
	}

	if err := store.Copy(context.Background(), "src", "dst"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "dst", "real.txt")); err != nil || string(data) != "real" {
		t.Fatalf("real copy = %q, err %v", data, err)
	}
	for _, rel := range []string{"dst/link.txt", "dst/escape", "dst/escape/secret.txt", "dst/.yuarcloud-chunks"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("copy created reserved or linked path %q (err %v)", rel, err)
		}
	}

	if err := store.Copy(context.Background(), "src/link.txt", "direct.txt"); err == nil {
		t.Fatal("copying a symlink directly should fail")
	}
	if _, err := os.Lstat(filepath.Join(root, "direct.txt")); !os.IsNotExist(err) {
		t.Fatalf("direct symlink copy created a file (err %v)", err)
	}
}
