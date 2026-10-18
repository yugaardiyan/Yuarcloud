package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// WriteChunk stores one chunk of an in-flight upload.
//
// It reports whether the chunk index was newly recorded. A client that retries
// a chunk after a dropped connection sends the same index twice, and that must
// not inflate the counter — otherwise the session reports itself complete while
// a different chunk is still missing from disk, and the assemble step fails on a
// file that was never actually fully uploaded. So the index set is what counts,
// not the number of write calls, and a repeat write overwrites the old file in
// place and reports false.
func (ls *LocalStorage) WriteChunk(ctx context.Context, uploadID string, chunkIndex int, reader io.Reader, size int64) (bool, error) {
	// A zero ceiling means no ceiling. Comparing against it unguarded would read
	// every chunk as too large and refuse all uploads — the opposite of what
	// "unlimited" is supposed to mean.
	if max := ls.limits.MaxChunkSize(); max > 0 && size > max {
		return false, ErrChunkTooLarge
	}

	if chunkIndex < 0 {
		return false, ErrInvalidChunkIndex
	}

	ls.mu.Lock()
	session, exists := ls.activeUploads[uploadID]
	if !exists {
		ls.mu.Unlock()
		return false, ErrUploadNotFound
	}

	if chunkIndex >= session.TotalChunks {
		ls.mu.Unlock()
		return false, ErrInvalidChunkIndex
	}

	_, duplicate := session.received[chunkIndex]

	// Reject only when every chunk is already in hand and this one adds nothing.
	// A duplicate arriving at the cap is still allowed, otherwise a retry of the
	// final chunk after a dropped response would be rejected as "complete".
	if !duplicate && session.UploadedChunks >= session.TotalChunks {
		ls.mu.Unlock()
		return false, ErrUploadComplete
	}
	ls.mu.Unlock()

	chunkDir := ls.resolveTempPath(uploadID, "")
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		return false, err
	}

	chunkPath := filepath.Join(chunkDir, fmt.Sprintf("chunk_%05d", chunkIndex))
	file, err := os.Create(chunkPath)
	if err != nil {
		return false, err
	}

	written, err := io.Copy(file, reader)
	if err != nil {
		file.Close()         //nolint:errcheck
		os.Remove(chunkPath) //nolint:errcheck
		return false, err
	}

	if written != size {
		file.Close()         //nolint:errcheck
		os.Remove(chunkPath) //nolint:errcheck
		return false, ErrSizeMismatch
	}

	// Durable before the session is told it has the chunk. Recorded before
	// this, a crash between the two left the session claiming a chunk that
	// was not on disk, and AssembleChunks concatenated whatever survived —
	// producing a file of the right length and the wrong contents, with a
	// checksum that had only been computed over what was there.
	if err := file.Sync(); err != nil {
		file.Close()         //nolint:errcheck
		os.Remove(chunkPath) //nolint:errcheck
		return false, err
	}
	if err := file.Close(); err != nil {
		os.Remove(chunkPath) //nolint:errcheck
		return false, err
	}

	// Everything from here happens under the lock the "is this a new chunk"
	// decision is made under, because the lock had to be dropped for the write.
	//
	// Deciding from the earlier read was a check-then-act across a lock
	// boundary: two requests for the same index both saw the index missing and
	// both returned true, so the caller counted one chunk twice. The session
	// then claimed more chunks than the disk held and could never be assembled.
	ls.mu.Lock()

	// The sweeper may have taken the session while the lock was dropped. The
	// pointer is still valid — Go does not free it — but its chunks are gone
	// from disk and nothing can reach the session again, so recording a chunk
	// against it would count bytes that nothing is left to reclaim.
	live, stillThere := ls.activeUploads[uploadID]
	if !stillThere {
		ls.mu.Unlock()
		os.Remove(chunkPath) //nolint:errcheck
		return false, ErrUploadNotFound
	}

	// A repeat of an index already on disk is still a success for the caller —
	// the write above simply overwrote the same path — it just must not be
	// counted twice. Checked here, where recording the index is atomic with it.
	if _, seen := live.received[chunkIndex]; seen {
		ls.mu.Unlock()
		return false, nil
	}

	// The ceiling is applied to measured bytes, not to the declared total.
	// total_chunks is client-supplied and unbounded, so a session that declares
	// total_size: 1 would otherwise stream an arbitrary amount past both
	// max_file_size and the user's quota — and have the recorded size agree,
	// because the handler charges what the client said.
	if max := ls.limits.MaxFileSize(); max > 0 && live.ReceivedBytes+size > max {
		ls.mu.Unlock()
		os.Remove(chunkPath) //nolint:errcheck
		return false, ErrFileTooLarge
	}
	live.ReceivedBytes += size
	// Through the setter, not inline: it also guards a nil map, and the
	// bookkeeping it keeps (UploadedChunks) is the same counter the duplicate
	// check above reads. Two copies of this is how they drift.
	live.MarkReceived(chunkIndex)
	// Sliding expiry. A fixed TTL cannot be both generous enough for a 10 GB
	// upload on a slow link and short enough to reclaim disk quickly — sized for
	// the first, a stalled upload holds its chunks for the whole window. Pushing
	// the deadline forward on every chunk means only sessions that actually go
	// quiet get swept, and the sweeper interval becomes the real time-to-reclaim.
	live.ExpiresAt = time.Now().Add(ls.limits.CleanupInterval())
	ls.mu.Unlock()

	return true, nil
}

func (ls *LocalStorage) AssembleChunks(ctx context.Context, uploadID, finalPath string, totalChunks int, expectedChecksum string) (int64, error) {
	ls.mu.RLock()
	session, exists := ls.activeUploads[uploadID]
	if !exists {
		ls.mu.RUnlock()
		return 0, ErrUploadNotFound
	}
	// Read inside the lock rather than after it. WriteChunk updates this
	// counter from another goroutine and a client can assemble while its last
	// chunk request is still in flight, so reading it unlocked is a data race —
	// and one that happens to return the right answer most of the time.
	uploaded := session.UploadedChunks
	totalSize := session.TotalSize
	ls.mu.RUnlock()

	if uploaded != totalChunks {
		return 0, ErrIncompleteUpload
	}

	chunkDir := ls.resolveTempPath(uploadID, "")
	finalFullPath := ls.resolvePath(finalPath)

	if err := os.MkdirAll(filepath.Dir(finalFullPath), 0755); err != nil {
		return 0, err
	}

	// Assembled beside the destination and moved into place only once every
	// check has passed. os.Create on the real path truncates whatever is there,
	// and each failure branch below removes it — so assembling in place meant
	// any error destroyed a pre-existing file the user still had a row for.
	partPath := finalFullPath + ".yuarcloud-part"
	finalFile, err := os.Create(partPath)
	if err != nil {
		return 0, err
	}
	defer finalFile.Close()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(finalFile, hasher)

	var written int64
	for i := 0; i < totalChunks; i++ {
		chunkPath := filepath.Join(chunkDir, fmt.Sprintf("chunk_%05d", i))
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			// A missing chunk file means the counter and the disk disagree.
			finalFile.Close()
			os.Remove(partPath) //nolint:errcheck
			return 0, ErrIncompleteUpload
		}

		n, err := io.Copy(multiWriter, chunkFile)
		chunkFile.Close()
		if err != nil {
			finalFile.Close()
			os.Remove(partPath) //nolint:errcheck
			return 0, err
		}
		written += n
	}

	// The declared size is a claim. The assembled length is the fact, and it is
	// the one the caller stores and charges — otherwise a client that declared
	// a byte could write a terabyte and have used_bytes record one.
	if written != totalSize {
		finalFile.Close()
		os.Remove(partPath) //nolint:errcheck
		return 0, ErrSizeMismatch
	}

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if expectedChecksum != "" && actualChecksum != expectedChecksum {
		finalFile.Close()
		os.Remove(partPath) //nolint:errcheck
		return 0, ErrChecksumMismatch
	}

	// Durable before it is visible. Close alone only reaches the page cache, so
	// a power cut could leave the row saying "complete" over a truncated file.
	if err := finalFile.Sync(); err != nil {
		os.Remove(partPath) //nolint:errcheck
		return 0, err
	}
	if err := finalFile.Close(); err != nil {
		os.Remove(partPath) //nolint:errcheck
		return 0, err
	}
	if err := os.Rename(partPath, finalFullPath); err != nil {
		os.Remove(partPath) //nolint:errcheck
		return 0, err
	}
	// The rename itself is a directory mutation. Without this the name can be
	// lost even though the contents were fsynced, which is the same torn state
	// the sync above exists to prevent — just one directory entry later.
	if err := syncDir(filepath.Dir(finalFullPath)); err != nil {
		return 0, err
	}

	ls.mu.Lock()
	session.Checksum = actualChecksum
	ls.mu.Unlock()

	return written, nil
}

func (ls *LocalStorage) CleanupChunks(ctx context.Context, uploadID string) error {
	chunkDir := ls.resolveTempPath(uploadID, "")
	return os.RemoveAll(chunkDir)
}

func (ls *LocalStorage) GetTempDir() string {
	return ls.tempDir
}

func (ls *LocalStorage) GetLimits() *Limits {
	return ls.limits
}

func (ls *LocalStorage) GetMaxFileSize() int64 {
	return ls.limits.MaxFileSize()
}

func (ls *LocalStorage) GetMaxChunkSize() int64 {
	return ls.limits.MaxChunkSize()
}

func (ls *LocalStorage) Exists(ctx context.Context, path string) (bool, error) {
	fullPath := ls.resolvePath(path)
	_, err := os.Stat(fullPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (ls *LocalStorage) GetDiskUsage(ctx context.Context, path string) (used, free int64, err error) {
	fullPath := ls.resolvePath(path)

	var stat unix.Statfs_t
	if err := unix.Statfs(fullPath, &stat); err != nil {
		return 0, 0, err
	}

	free = int64(stat.Bavail) * int64(stat.Bsize)
	total := int64(stat.Blocks) * int64(stat.Bsize)
	used = total - int64(stat.Bfree)*int64(stat.Bsize)

	return used, free, nil
}

// cleanupRoutine sweeps expired upload sessions on a self-rescheduling timer.
//
// A fixed ticker baked in the interval it was constructed with, so changing the
// setting from the admin panel did nothing until a restart. Re-reading the
// interval after every sweep means a new value takes effect on the next tick —
// at worst one old interval later, which for a sweep that only reclaims disk
// space is the right trade against a goroutine that has to be torn down.
func (ls *LocalStorage) cleanupRoutine() {
	for {
		timer := time.NewTimer(ls.limits.CleanupInterval())
		<-timer.C
		ls.cleanupExpiredUploads()
	}
}

// cleanupExpiredUploads reclaims sessions whose TTL has passed.
//
// The directory removal happens outside the mutex. It did not: the lock was
// held across os.RemoveAll, which walks and unlinks an entire chunk tree, and
// that is the same lock every WriteChunk takes. One sweep of a large abandoned
// upload therefore blocked all chunked uploads in the process for as long as
// the unlinking took — on a slow disk, long enough to look like the server had
// hung. Collect under the lock, delete outside it.
func (ls *LocalStorage) cleanupExpiredUploads() {
	now := time.Now()

	var expired []string
	ls.mu.Lock()
	for id, session := range ls.activeUploads {
		if now.After(session.ExpiresAt) {
			expired = append(expired, id)
			delete(ls.activeUploads, id)
		}
	}
	ls.mu.Unlock()

	for _, id := range expired {
		if err := ls.CleanupChunks(context.Background(), id); err != nil {
			// Leave it be and say so. The bytes are unreachable from the API
			// either way, and sweeping again on the next pass costs nothing —
			// silently ignoring this is how a temp directory fills up.
			fmt.Printf("cleanup: removing chunks for upload %s failed: %v\n", id, err)
		}
	}
}

func (ls *LocalStorage) CreateUploadSession(uploadID, filename, targetPath string, totalSize, chunkSize int64, totalChunks int, ttl time.Duration) *UploadSession {
	// The caller-supplied ttl is only a floor. The sweeper runs on the cleanup
	// interval, so a session that outlives it would be reclaimed out from under
	// an upload that is still going — take whichever is longer.
	if sweep := ls.limits.CleanupInterval(); sweep > ttl {
		ttl = sweep
	}

	session := &UploadSession{
		ID:             uploadID,
		Filename:       filename,
		TargetPath:     targetPath,
		TotalSize:      totalSize,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		UploadedChunks: 0,
		received:       make(map[int]struct{}),
		TempDir:        ls.resolveTempPath(uploadID, ""),
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(ttl),
	}

	ls.mu.Lock()
	ls.activeUploads[uploadID] = session
	ls.mu.Unlock()

	return session
}

func (ls *LocalStorage) GetUploadSession(uploadID string) (*UploadSession, bool) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	session, exists := ls.activeUploads[uploadID]
	return session, exists
}

func (ls *LocalStorage) DeleteUploadSession(uploadID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.activeUploads, uploadID)
}

var (
	ErrNotFound          = &StorageError{Code: "NOT_FOUND", Message: "file not found"}
	ErrFileTooLarge      = &StorageError{Code: "FILE_TOO_LARGE", Message: "file size exceeds maximum allowed"}
	ErrChunkTooLarge     = &StorageError{Code: "CHUNK_TOO_LARGE", Message: "chunk size exceeds maximum allowed"}
	ErrInvalidChunkIndex = &StorageError{Code: "INVALID_CHUNK_INDEX", Message: "chunk index is outside the range this upload accepts"}
	ErrUploadNotFound    = &StorageError{Code: "UPLOAD_NOT_FOUND", Message: "upload session not found"}
	ErrUploadComplete    = &StorageError{Code: "UPLOAD_COMPLETE", Message: "all chunks already uploaded"}
	ErrIncompleteUpload  = &StorageError{Code: "INCOMPLETE_UPLOAD", Message: "not all chunks uploaded"}
	ErrChecksumMismatch  = &StorageError{Code: "CHECKSUM_MISMATCH", Message: "checksum verification failed"}
	ErrSizeMismatch      = &StorageError{Code: "SIZE_MISMATCH", Message: "written size does not match expected"}
	ErrInvalidRange      = &StorageError{Code: "INVALID_RANGE", Message: "range offset and length must not be negative"}
	ErrNotADirectory     = &StorageError{Code: "NOT_A_DIRECTORY", Message: "a file already exists at that path"}
)

type StorageError struct {
	Code    string
	Message string
}

func (e *StorageError) Error() string {
	return e.Message
}

func getMimeType(name string, isDir bool) string {
	if isDir {
		return "inode/directory"
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md":
		return "text/plain"
	case ".zip":
		return "application/zip"
	case ".json":
		return "application/json"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	default:
		return "application/octet-stream"
	}
}
