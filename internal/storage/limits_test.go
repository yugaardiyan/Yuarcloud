package storage

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/yuarcloud/yuarcloud/internal/config"
)

// The whole point of Limits is that an admin's save reaches the running server.
// A field copied into the handler at construction passes every other test while
// leaving uploads on the pre-restart value forever.
func TestLimitsTakeEffectWithoutRestart(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()
	limits := ls.GetLimits()

	body := []byte("123456")

	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)

	// Tighten the chunk ceiling below the chunk we are about to send.
	limits.SetMaxChunkSize(3)
	if _, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader(body), 6); err != ErrChunkTooLarge {
		t.Fatalf("new chunk limit not enforced: want ErrChunkTooLarge, got %v", err)
	}

	// Loosen it again and the same chunk goes through.
	limits.SetMaxChunkSize(1 << 20)
	if _, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader(body), 6); err != nil {
		t.Fatalf("loosened chunk limit still rejecting: %v", err)
	}

	// Same for the plain Write path.
	limits.SetMaxFileSize(2)
	if err := ls.Write(ctx, "big.bin", bytes.NewReader(body), 6); err != ErrFileTooLarge {
		t.Fatalf("new file limit not enforced: want ErrFileTooLarge, got %v", err)
	}
	limits.SetMaxFileSize(1 << 20)
	if err := ls.Write(ctx, "big.bin", bytes.NewReader(body), 6); err != nil {
		t.Fatalf("loosened file limit still rejecting: %v", err)
	}
}

// Zero is the self-hosting escape hatch: an owner who knows their own disk sets
// no ceiling, and nothing may read that as "reject everything".
func TestZeroLimitMeansUnlimited(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()
	limits := ls.GetLimits()

	limits.SetMaxFileSize(0)
	if err := ls.Write(ctx, "huge.bin", bytes.NewReader(make([]byte, 4096)), 4096); err != nil {
		t.Fatalf("zero max_file_size rejected a write: %v", err)
	}

	limits.SetMaxChunkSize(0)
	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)
	if _, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader(make([]byte, 4096)), 4096); err != nil {
		t.Fatalf("zero max_chunk_size rejected a chunk: %v", err)
	}

	// A negative value is nonsense and must not flip the comparison either.
	limits.SetMaxFileSize(-5)
	limits.SetMaxChunkSize(-5)
	if err := ls.Write(ctx, "h2.bin", bytes.NewReader(make([]byte, 4096)), 4096); err != nil {
		t.Fatalf("negative max_file_size rejected a write: %v", err)
	}
}

// The sweep re-reads the interval each pass, so a change lands without a restart.
// A ticker built once in the constructor would keep the old cadence forever.
func TestCleanupIntervalChangeTakesEffect(t *testing.T) {
	ls := newTestStorage(t)
	ctx := context.Background()
	limits := ls.GetLimits()

	if limits.CleanupInterval() != time.Hour {
		t.Fatalf("seeded interval: want 1h, got %v", limits.CleanupInterval())
	}

	limits.SetCleanupInterval(15 * time.Minute)
	if got := limits.CleanupInterval(); got != 15*time.Minute {
		t.Fatalf("interval change ignored: want 15m, got %v", got)
	}

	// Nonsense intervals fall back to a usable value instead of a zero duration,
	// which would spin the sweeper.
	limits.SetCleanupInterval(0)
	limits.SetCleanupInterval(-time.Second)
	if got := limits.CleanupInterval(); got != time.Hour {
		t.Fatalf("non-positive interval: want 1h fallback, got %v", got)
	}

	// And a session inherits the current interval, not one frozen at boot.
	limits.SetCleanupInterval(90 * time.Minute)
	ls.CreateUploadSession("up", "f.bin", "f.bin", 12, 6, 2, time.Hour)
	ls.WriteChunk(ctx, "up", 0, bytes.NewReader([]byte("aaaaaa")), 6)
	session, _ := ls.GetUploadSession("up")
	want := time.Now().Add(90 * time.Minute)
	if delta := session.ExpiresAt.Sub(want); delta > time.Minute || delta < -time.Minute {
		t.Fatalf("session deadline %v is not ~90m out", session.ExpiresAt)
	}
}

func TestLimitsDefaultToConfigValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.Local.Root = t.TempDir()
	cfg.Storage.Chunked.TempDir = t.TempDir()
	cfg.Storage.Chunked.MaxFileSize = 555
	cfg.Storage.Chunked.MaxChunkSize = 77
	cfg.Storage.Chunked.CleanupInterval = 3 * time.Hour

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	defer os.RemoveAll(cfg.Storage.Local.Root)

	if got := ls.GetMaxChunkSize(); got != 77 {
		t.Fatalf("max chunk size: want 77, got %d", got)
	}
	if got := ls.GetMaxFileSize(); got != 555 {
		t.Fatalf("max file size: want 555, got %d", got)
	}
	if got := ls.GetLimits().CleanupInterval(); got != 3*time.Hour {
		t.Fatalf("cleanup interval: want 3h, got %v", got)
	}
}
