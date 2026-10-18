package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

func newTestStorage(t *testing.T) *LocalStorage {
	t.Helper()

	cfg := &config.Config{}
	cfg.Storage.Local.Root = t.TempDir()
	cfg.Storage.Chunked.TempDir = t.TempDir()
	cfg.Storage.Chunked.MaxFileSize = 10 * 1024 * 1024
	cfg.Storage.Chunked.MaxChunkSize = 1024 * 1024
	cfg.Storage.Chunked.CleanupInterval = time.Hour

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return ls
}

func TestWriteChunkRejectsOutOfRangeIndex(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()
	body := []byte("payload")

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)

	if _, err := ls.WriteChunk(ctx, "up", -1, bytes.NewReader(body), 6); err != ErrInvalidChunkIndex {
		t.Fatalf("negative index: want ErrInvalidChunkIndex, got %v", err)
	}
	if _, err := ls.WriteChunk(ctx, "up", 2, bytes.NewReader(body), 6); err != ErrInvalidChunkIndex {
		t.Fatalf("index == totalChunks: want ErrInvalidChunkIndex, got %v", err)
	}
	if _, err := ls.WriteChunk(ctx, "missing", 0, bytes.NewReader(body), 6); err != ErrUploadNotFound {
		t.Fatalf("unknown upload: want ErrUploadNotFound, got %v", err)
	}
}

// A chunk index sent twice must count once. Before this was tracked the retry
// after a dropped response pushed the counter to total_chunks while another
// index was still absent, so the session looked complete and assemble failed.
func TestWriteChunkRetryIsIdempotent(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)

	if isNew, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6); err != nil || !isNew {
		t.Fatalf("first write of index 0: isNew=%v err=%v", isNew, err)
	}
	if isNew, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("bbbbbb")), 6); err != nil || isNew {
		t.Fatalf("retry of index 0: want isNew=false, got isNew=%v err=%v", isNew, err)
	}

	session, ok := ls.GetUploadSession("up")
	if !ok {
		t.Fatal("session missing")
	}
	if session.UploadedChunks != 1 {
		t.Fatalf("retry double-counted: want 1, got %d", session.UploadedChunks)
	}

	// And the session must still accept the chunk it has not seen.
	if _, err := ls.WriteChunk(ctx, "up", 1, bytes.NewReader([]byte("cccccc")), 6); err != nil {
		t.Fatalf("index 1 after retry: %v", err)
	}
}

// The counter is only allowed to move once the bytes are on disk. A write that
// fails part-way must not leave the session claiming a chunk it never stored,
// which would strand the client: its retry would be rejected as "complete".
func TestWriteChunkFailureDoesNotCount(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)

	// Size the reader lies about: written != size, so the write must be refused.
	if _, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaa")), 6); err != ErrSizeMismatch {
		t.Fatalf("want ErrSizeMismatch, got %v", err)
	}

	session, _ := ls.GetUploadSession("up")
	if session.UploadedChunks != 0 {
		t.Fatalf("failed write was counted: got %d, want 0", session.UploadedChunks)
	}

	// The refused index must still be writable afterwards.
	if _, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6); err != nil {
		t.Fatalf("retry after failed write: %v", err)
	}
}

func TestAssembleChunksRejectsIncompleteUpload(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("up", "f.bin", "out/f.bin", 12, 6, 2, time.Hour)
	ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6)

	if _, err := ls.AssembleChunks(ctx, "up", "out/f.bin", 2, ""); err != ErrIncompleteUpload {
		t.Fatalf("want ErrIncompleteUpload, got %v", err)
	}
	if _, err := os.Stat(ls.resolvePath("out/f.bin")); !os.IsNotExist(err) {
		t.Fatal("a rejected assemble left a partial file behind")
	}
}

func TestAssembleChunksVerifiesChecksum(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	full := []byte("hello world, this is the whole file")
	sum := sha256.Sum256(full)
	want := hex.EncodeToString(sum[:])

	ls.CreateUploadSession("up", "f.bin", "out/f.bin", int64(len(full)), 12, 3, time.Hour)
	for i := 0; i < 3; i++ {
		start := i * 12
		end := start + 12
		if end > len(full) {
			end = len(full) // last chunk is short, exactly as the client sends it
		}
		part := full[start:end]
		if _, err := ls.WriteChunk(ctx, "up", i, bytes.NewReader(part), int64(len(part))); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	if _, err := ls.AssembleChunks(ctx, "up", "out/f.bin", 3, want); err != nil {
		t.Fatalf("assemble with correct checksum: %v", err)
	}
	got, err := os.ReadFile(ls.resolvePath("out/f.bin"))
	if err != nil {
		t.Fatalf("read assembled: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("assembled bytes differ:\n got %q\nwant %q", got, full)
	}

	// A wrong checksum must not leave the file in place.
	ls.CreateUploadSession("bad", "f.bin", "out/bad.bin", int64(len(full)), 12, 3, time.Hour)
	for i := 0; i < 3; i++ {
		start := i * 12
		end := start + 12
		if end > len(full) {
			end = len(full)
		}
		part := full[start:end]
		ls.WriteChunk(ctx, "bad", i, bytes.NewReader(part), int64(len(part)))
	}
	if _, err := ls.AssembleChunks(ctx, "bad", "out/bad.bin", 3, "00ff"); err != ErrChecksumMismatch {
		t.Fatalf("want ErrChecksumMismatch, got %v", err)
	}
	if _, err := os.Stat(ls.resolvePath("out/bad.bin")); !os.IsNotExist(err) {
		t.Fatal("failed checksum left the bad file on disk")
	}
}

func TestCleanupChunksRemovesTempDir(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)
	ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6)

	chunkDir := filepath.Join(ls.tempDir, "up")
	if _, err := os.Stat(chunkDir); err != nil {
		t.Fatalf("temp chunk dir missing: %v", err)
	}

	if err := ls.CleanupChunks(ctx, "up"); err != nil {
		t.Fatalf("CleanupChunks: %v", err)
	}
	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Fatal("temp chunk dir survived cleanup")
	}
}

func TestCleanupExpiredUploadsDropsSession(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("stale", "f.bin", "f.bin", 12, 6, 2, time.Hour)
	ls.CreateUploadSession("live", "g.bin", "g.bin", 12, 6, 2, time.Hour)
	ls.WriteChunk(ctx, "stale", 0, bytes.NewReader([]byte("aaaaaa")), 6)

	// Expire one directly rather than through creation: CreateUploadSession
	// deliberately refuses a deadline shorter than the sweep interval, so a short
	// ttl at construction is not a way to make a session stale.
	ls.mu.Lock()
	ls.activeUploads["stale"].ExpiresAt = time.Now().Add(-time.Minute)
	ls.mu.Unlock()

	ls.cleanupExpiredUploads()

	if _, ok := ls.GetUploadSession("stale"); ok {
		t.Fatal("expired session still in memory")
	}
	if _, ok := ls.GetUploadSession("live"); !ok {
		t.Fatal("live session was swept")
	}
	if _, err := os.Stat(filepath.Join(ls.tempDir, "stale")); !os.IsNotExist(err) {
		t.Fatal("expired session's chunks were left on disk")
	}
}

// Sliding expiry: a chunk that keeps arriving must keep its session alive well
// past the interval. Without it, any transfer longer than one cleanup interval
// is killed mid-upload by the expiry check, which is a very ordinary thing for a
// large file on a slow link to be.
func TestWriteChunkSlidesExpiryForward(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	limits := ls.GetLimits()
	limits.SetCleanupInterval(50 * time.Millisecond)
	ls.CreateUploadSession("up", "f.bin", "f.bin", 24, 6, 4, 50*time.Millisecond)

	// Copy the timestamp, not the pointer: GetUploadSession hands back the live
	// session, so holding it would show the post-write value and the comparison
	// would always pass.
	beforeSession, _ := ls.GetUploadSession("up")
	before := beforeSession.ExpiresAt

	// Three refreshes, each one interval apart, so the deadline has to move three
	// times or the sweeper would have caught it between them.
	for i := 0; i < 3; i++ {
		if _, err := ls.WriteChunk(ctx, "up", i, bytes.NewReader([]byte("aaaaaa")), 6); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	after, _ := ls.GetUploadSession("up")
	if !after.ExpiresAt.After(before) {
		t.Fatalf("expiry did not slide: before=%v after=%v", before, after.ExpiresAt)
	}

	ls.cleanupExpiredUploads()
	if _, ok := ls.GetUploadSession("up"); !ok {
		t.Fatal("actively-uploading session was swept")
	}
}

func TestDeleteUploadSessionRemovesSession(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)
	ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6)

	ls.DeleteUploadSession("up")

	if _, ok := ls.GetUploadSession("up"); ok {
		t.Fatal("session still in memory after delete")
	}
}
