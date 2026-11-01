package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Tus upload storage.
//
// A tus upload is a single append-only file, which is the opposite shape from
// the chunked path's one-file-per-chunk directory. It lives under the storage
// root (not tempDir) so that finalising it is a rename on the same filesystem
// and so the startup chunk sweep cannot delete a resumable upload. The name
// carries InternalPrefix, which keeps it out of listings and out of any path a
// client can address.
//
// The offset is enforced twice on purpose: the handler compares the client's
// Upload-Offset header with the size it reads here, and TusAppend re-checks the
// size while holding a per-upload lock. Between the handler's check and the
// write, a sweep or a second request could otherwise move the file.

var (
	// ErrTusOffsetMismatch reports that the data file is not the length the
	// caller expected; the returned size is the real one.
	ErrTusOffsetMismatch = errors.New("tus upload offset does not match")
	// ErrTusTooLarge reports a client trying to write past Upload-Length.
	ErrTusTooLarge = errors.New("tus upload exceeds declared size")
	// ErrTusChecksumMismatch reports that the optional checksum did not match.
	ErrTusChecksumMismatch = errors.New("tus upload checksum mismatch")
	// ErrTusTargetExists reports that something already occupies the final path.
	ErrTusTargetExists = errors.New("tus target path already exists")
)

// tusLock serialises work on one upload. The returned function unlocks it.
func (ls *LocalStorage) tusLock(uploadID string) func() {
	value, _ := ls.tusLocks.LoadOrStore(uploadID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (ls *LocalStorage) tusDataPath(uploadID string) string {
	return filepath.Join(ls.tusDir, safePathElement(uploadID)+".data")
}

// TusSize reports how many bytes of this upload are on disk. A session that has
// not received its first byte yet has no file, which is zero rather than an
// error: HEAD has to answer Upload-Offset: 0 for a freshly created upload.
func (ls *LocalStorage) TusSize(uploadID string) (int64, error) {
	info, err := os.Stat(ls.tusDataPath(uploadID))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// TusAppend writes reader at exactly offset and returns the new length.
//
// maxSize is the Upload-Length the client declared. Reading one byte past it is
// how an overlong PATCH is detected: without that check a client could declare
// 1 MB, send 10 GB, and every limit — max_file_size, quota, the eventual file
// size — would be judged against the declaration.
func (ls *LocalStorage) TusAppend(ctx context.Context, uploadID string, offset, maxSize int64, reader io.Reader) (int64, error) {
	if offset < 0 || maxSize < 0 || offset > maxSize {
		return 0, ErrTusOffsetMismatch
	}

	unlock := ls.tusLock(uploadID)
	defer unlock()

	path := ls.tusDataPath(uploadID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() != offset {
		return info.Size(), ErrTusOffsetMismatch
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}

	allowed := maxSize - offset
	n, err := io.Copy(file, io.LimitReader(reader, allowed+1))
	if err != nil {
		// A failed append must not leave a partial tail behind: the next PATCH
		// asks to continue from the old offset, and a file longer than that
		// would answer "offset mismatch" forever.
		_ = file.Truncate(offset)
		_ = file.Sync()
		return offset, err
	}
	if n > allowed {
		_ = file.Truncate(maxSize)
		_ = file.Sync()
		return maxSize, ErrTusTooLarge
	}

	// The bytes have to be durable before the response says they arrived. A
	// client that then closes the tab trusts that answer, and a power cut that
	// loses the tail would make the file permanently short with no request left
	// to notice.
	if err := file.Sync(); err != nil {
		return offset, err
	}
	return offset + n, nil
}

// TusFinalize checks the optional checksum and moves the data file into place.
//
// The move is a rename within the storage root, so even a 50 GB upload does not
// copy anything here. The checksum path does read the whole file, which is why
// the client only sends one below its own hashing ceiling; for larger files it
// is empty and the read is skipped.
func (ls *LocalStorage) TusFinalize(ctx context.Context, uploadID, finalPath, expectedChecksum string) (int64, error) {
	unlock := ls.tusLock(uploadID)
	defer unlock()

	dataPath := ls.tusDataPath(uploadID)
	info, err := os.Stat(dataPath)
	if err != nil {
		return 0, err
	}
	size := info.Size()

	if expectedChecksum != "" {
		actual, err := sha256File(dataPath)
		if err != nil {
			return 0, err
		}
		if !strings.EqualFold(actual, strings.TrimSpace(expectedChecksum)) {
			return 0, ErrTusChecksumMismatch
		}
	}

	finalFullPath := ls.resolvePath(finalPath)
	if err := os.MkdirAll(filepath.Dir(finalFullPath), 0755); err != nil {
		return 0, err
	}
	if _, err := os.Stat(finalFullPath); err == nil {
		return 0, ErrTusTargetExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	// Make sure the last append is on stable storage before it becomes visible
	// under a user-facing name.
	if file, err := os.OpenFile(dataPath, os.O_RDWR, 0); err == nil {
		_ = file.Sync()
		_ = file.Close()
	}

	if err := os.Rename(dataPath, finalFullPath); err != nil {
		// temp_dir and the storage root are normally the same volume, but an
		// operator can point them at different ones. Fall back to a durable
		// copy rather than failing an upload that has already arrived in full.
		if copyErr := copyFileDurable(dataPath, finalFullPath); copyErr != nil {
			return 0, copyErr
		}
		_ = os.Remove(dataPath)
	}
	return size, nil
}

// TusRemove deletes an upload's partial data. Missing is success: cancel and
// the expiry sweep both race with each other and neither should report an error
// for being second.
func (ls *LocalStorage) TusRemove(uploadID string) error {
	unlock := ls.tusLock(uploadID)
	defer unlock()

	err := os.Remove(ls.tusDataPath(uploadID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SweepTusOrphans removes data files no database row knows about.
//
// Reaching here with an orphan means a crash landed between deleting the row and
// deleting the file. The list is read once at startup, so a stale entry cannot
// remove a session created after the list was taken.
func (ls *LocalStorage) SweepTusOrphans(keep map[string]struct{}) {
	entries, err := os.ReadDir(ls.tusDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".data") {
			continue
		}
		id := strings.TrimSuffix(name, ".data")
		if _, ok := keep[id]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(ls.tusDir, name)); err != nil {
			fmt.Printf("tus sweep: removing orphan data %s failed: %v\n", name, err)
		}
	}
}

// sha256File hashes a file without holding it in memory.
func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.CopyBuffer(hasher, file, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// copyFileDurable writes src to dst through a temporary sibling, fsyncs it and
// only then renames it into place. Used when the two paths are on different
// volumes, where os.Rename cannot work.
func copyFileDurable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	part, err := os.CreateTemp(filepath.Dir(dst), ".yuarcloud-tus-part-*")
	if err != nil {
		return err
	}
	partPath := part.Name()
	defer os.Remove(partPath) //nolint:errcheck

	if _, err := io.CopyBuffer(part, in, make([]byte, 1<<20)); err != nil {
		part.Close()
		return err
	}
	if err := part.Sync(); err != nil {
		part.Close()
		return err
	}
	if err := part.Close(); err != nil {
		return err
	}
	return os.Rename(partPath, dst)
}
