package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// An empty temp_dir has to follow the storage root. The Docker image ships it
// empty for exactly that reason: pointing storage.local.root at another volume
// must not leave partial uploads on the old one, or inside the container's
// writable layer, where a large upload would fill a disk nobody was watching.
func TestEmptyTempDirFollowsRoot(t *testing.T) {
	root := t.TempDir()
	storageRoot := filepath.Join(root, "library")

	cfg := &config.Config{
		Storage: config.StorageConfig{
			Type:  "local",
			Local: config.LocalConfig{Root: storageRoot},
		},
	}

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}

	want := filepath.Join(storageRoot, ".yuarcloud-chunks")
	if ls.tempDir != want {
		t.Fatalf("tempDir = %q, want %q", ls.tempDir, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("temp dir %q was not created: %v", want, err)
	}
}
