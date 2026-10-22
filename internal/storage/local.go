package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// InternalPrefix marks every name the server owns: the trash, the chunk
// scratch directory, and the half-written part file of an in-flight upload.
// Anything carrying it is bookkeeping and must never surface as a user's.
//
// It lives here rather than in the handler package because the storage layer
// needs it to decide what to list, and it was deciding with a looser rule
// instead: it skipped *every* dot-prefixed entry, on the reasoning that a
// leading dot means "ours". That is true of nothing the server creates — it
// creates none of them under that name — and false of a great deal the user
// owns. Uploading .env reported success, wrote the file, and then showed an
// empty folder: the bytes were on disk and unreachable from the UI.
//
// The handler package has carried its own copy of this prefix for the
// addressability checks. Two constants would be free to drift, so it refers
// to this one; see internal/handler/file.go.
const InternalPrefix = ".yuarcloud"

type LocalStorage struct {
	rootDir       string
	tempDir       string
	limits        *Limits
	mu            sync.RWMutex
	activeUploads map[string]*UploadSession
}

// isInternal reports whether a single directory entry belongs to the server.
func isInternal(name string) bool {
	return strings.HasPrefix(name, InternalPrefix)
}

func NewLocalStorage(cfg *config.Config) (*LocalStorage, error) {
	return NewLocalStorageWithLimits(cfg, NewLimits(
		cfg.Storage.Chunked.MaxFileSize,
		cfg.Storage.Chunked.MaxChunkSize,
		cfg.Storage.Chunked.CleanupInterval,
	))
}

// NewLocalStorageWithLimits takes the ceilings as a parameter instead of
// reading them out of the config, so main can seed them from the admin panel's
// saved values before the sweeper starts.
func NewLocalStorageWithLimits(cfg *config.Config, limits *Limits) (*LocalStorage, error) {
	rootDir := cfg.Storage.Local.Root
	tempDir := cfg.Storage.Chunked.TempDir

	// An empty temp_dir means "keep partial uploads under the storage root".
	// The Docker image ships it empty for exactly that reason: pointing
	// storage.local.root at another volume must not leave the scratch dir on
	// the old one (or, worse, inside the container's writable layer), where a
	// large upload would fill a disk the operator was not watching. An
	// explicit value still wins.
	if tempDir == "" {
		tempDir = filepath.Join(rootDir, InternalPrefix+"-chunks")
	}

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("create root dir: %w", err)
	}
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	ls := &LocalStorage{
		rootDir:       rootDir,
		tempDir:       tempDir,
		limits:        limits,
		activeUploads: make(map[string]*UploadSession),
	}

	// A crash or a restart leaves chunk directories with no session behind
	// them: the in-memory map is the only record of what is in flight, so
	// anything a previous process wrote is invisible to the sweeper from the
	// moment it starts. Those are the chunk directories of uploads that can
	// never be completed — no client holds the id any more — and they are the
	// largest thing on a storage volume for the rest of the process's life.
	// Nothing else ever removes them.
	ls.sweepOrphanedChunks()

	go ls.cleanupRoutine()

	return ls, nil
}

// sweepOrphanedChunks removes chunk directories left by a previous process.
//
// Deliberately unconditional rather than "older than an hour": at startup no
// session from this process exists yet, so every directory is by definition
// orphaned, and the only risk of removing one too eagerly is a second server
// process sharing the same temp dir — which would be misconfiguration that the
// chunk session store cannot support anyway.
func (ls *LocalStorage) sweepOrphanedChunks() {
	entries, err := os.ReadDir(ls.tempDir)
	if err != nil {
		// A missing or unreadable temp dir is not fatal — the chunk paths will
		// fail loudly on their own when they are used.
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(ls.tempDir, entry.Name())); err != nil {
			fmt.Printf("startup sweep: removing orphan chunk dir %s failed: %v\n", entry.Name(), err)
		}
	}
}

func (ls *LocalStorage) resolvePath(path string) string {
	path = filepath.Clean(path)
	path = strings.TrimPrefix(path, "/")
	return filepath.Join(ls.rootDir, path)
}

// resolveTempPath builds the on-disk location of an in-flight upload's chunks.
//
// The upload id is a single path element, so it is reduced to one the same way
// a filename is. It is generated server-side today, which is what makes this
// defence in depth rather than a live bug — but the value is derived from a
// request-derived sequence, and a single missing filepath.Base here turns any
// future id that is not that safe into a write outside tempDir. Cheap to make
// unconditional.
func (ls *LocalStorage) resolveTempPath(uploadID, filename string) string {
	return filepath.Join(ls.tempDir, safePathElement(uploadID), safePathElement(filename))
}

// safePathElement reduces a value to one path element that cannot climb out of
// its parent.
func safePathElement(raw string) string {
	if raw == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(strings.ReplaceAll(raw, "\\", "/")))
}

func (ls *LocalStorage) Stat(ctx context.Context, path string) (*FileInfo, error) {
	fullPath := ls.resolvePath(path)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	// No checksum here, and deliberately so — see the note on FileInfo.
	return &FileInfo{
		Name:     info.Name(),
		Path:     path,
		Size:     info.Size(),
		MimeType: getMimeType(info.Name(), info.IsDir()),
		IsDir:    info.IsDir(),
		ModTime:  info.ModTime(),
	}, nil
}

func (ls *LocalStorage) List(ctx context.Context, path string) ([]*FileInfo, error) {
	fullPath := ls.resolvePath(path)

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	var files []*FileInfo
	for _, entry := range entries {
		// Only the server's own bookkeeping is skipped — the trash, the chunk
		// scratch directory, a half-written upload. A user's dotfile is theirs:
		// .env, .gitignore and .htaccess are ordinary files that were being
		// hidden here, which made a successful upload look like it had lost
		// the data.
		if isInternal(entry.Name()) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		relPath := filepath.Join(path, entry.Name())
		relPath = strings.TrimPrefix(relPath, "/")

		files = append(files, &FileInfo{
			Name:     entry.Name(),
			Path:     relPath,
			Size:     info.Size(),
			MimeType: getMimeType(entry.Name(), entry.IsDir()),
			IsDir:    entry.IsDir(),
			ModTime:  info.ModTime(),
		})
	}

	return files, nil
}

// Mkdir creates one directory and reports whether it had to.
//
// os.MkdirAll silently materialises every missing ancestor. Move, copy and
// upload call it to make sure a destination's parent exists — but a path that
// was never supposed to exist then produces real directories on disk with no
// database row for any of them. They are invisible in the listing, which
// intersects the disk with the rows, and unremovable through the API because
// there is no id to address. A user who then uploads into what they believe is
// an empty folder lands inside a phantom tree that no request will ever list
// or delete.
func (ls *LocalStorage) Mkdir(ctx context.Context, path string) error {
	fullPath := ls.resolvePath(path)

	if err := os.Mkdir(fullPath, 0755); err == nil {
		return syncDir(filepath.Dir(fullPath))
	} else if !os.IsExist(err) {
		return err
	}

	// Already there — but it must be a directory, and it must be a directory
	// the process can actually write into. Reporting success for a regular
	// file at this path is how a later write fails with a confusing error
	// about the wrong thing.
	info, err := os.Stat(fullPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrNotADirectory
	}
	return nil
}

func (ls *LocalStorage) Remove(ctx context.Context, path string) error {
	fullPath := ls.resolvePath(path)
	return os.RemoveAll(fullPath)
}
