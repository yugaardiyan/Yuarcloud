package storage

import (
	"bytes"
	"context"
	"github.com/yuarcloud/yuarcloud/internal/config"
	"os"
	"path/filepath"
	"testing"
)

// total_size arrives in the init request and used to be taken at face value all
// the way to the database row and the quota charge. A client could declare one
// byte and stream an arbitrary amount past it, and max_file_size was checked
// only against the claim. These pin that the ceiling and the recorded size are
// now measured.
func newChunkStore(t *testing.T, maxFile int64) (*LocalStorage, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Storage.Local.Root = root
	cfg.Storage.Chunked.TempDir = filepath.Join(root, ".yuarcloud-chunks")
	cfg.Storage.Chunked.MaxFileSize = maxFile

	ls, err := NewLocalStorage(cfg)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return ls, root
}

func seedSession(ls *LocalStorage, id string, totalSize int64, totalChunks int) {
	ls.CreateUploadSession(id, "f.bin", "out/f.bin", totalSize, 1, totalChunks, 0)
}

// The exploit: declare a trivial size, then send more than the ceiling allows.
func TestChunkBytesCannotExceedMaxFileSize(t *testing.T) {
	ls, root := newChunkStore(t, 1000)
	ctx := context.Background()

	// Declares 1 byte total, but total_chunks says 1000.
	seedSession(ls, "liar", 1, 1000)

	payload := bytes.Repeat([]byte("x"), 400)
	accepted := 0
	var lastErr error
	for i := 0; i < 10; i++ {
		_, err := ls.WriteChunk(ctx, "liar", i, bytes.NewReader(payload), int64(len(payload)))
		if err != nil {
			lastErr = err
			break
		}
		accepted++
	}

	if lastErr != ErrFileTooLarge {
		t.Fatalf("error = %v, want ErrFileTooLarge after %d accepted chunk(s)", lastErr, accepted)
	}

	// 1000 bytes of ceiling, 400-byte chunks: two fit, the third must not.
	if accepted != 2 {
		t.Fatalf("accepted %d chunks of 400 bytes against a 1000 byte ceiling, want 2", accepted)
	}

	// Nothing may be left on disk past the point of refusal.
	var total int64
	entries, _ := os.ReadDir(filepath.Join(root, ".yuarcloud-chunks", "liar"))
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total > 1000 {
		t.Fatalf("%d bytes of chunk files on disk, want at most the 1000 byte ceiling", total)
	}
}

// A session whose delivered bytes disagree with its declaration must not
// assemble, and must not leave a half-written file behind.
func TestAssembleRejectsASizeThatDoesNotMatch(t *testing.T) {
	ls, root := newChunkStore(t, 0)
	ctx := context.Background()

	body := []byte("PAYLOAD PAYLOAD PAYLOAD")
	seedSession(ls, "liar2", 1, 2)
	for i := 0; i < 2; i++ {
		if _, err := ls.WriteChunk(ctx, "liar2", i, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	if _, err := ls.AssembleChunks(ctx, "liar2", "out/f.bin", 2, ""); err != ErrSizeMismatch {
		t.Fatalf("error = %v, want ErrSizeMismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, "out", "f.bin")); !os.IsNotExist(err) {
		t.Fatal("a rejected assembly left the output file behind")
	}
	if _, err := os.Stat(filepath.Join(root, "out", "f.bin.yuarcloud-part")); !os.IsNotExist(err) {
		t.Fatal("a rejected assembly left its .part file behind")
	}
}

// The honest case still works, and reports the measured length.
func TestAssembleReportsTheMeasuredSize(t *testing.T) {
	ls, root := newChunkStore(t, 0)
	ctx := context.Background()

	body := []byte("the real bytes")
	seedSession(ls, "honest", int64(len(body)), 1)
	if _, err := ls.WriteChunk(ctx, "honest", 0, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	n, err := ls.AssembleChunks(ctx, "honest", "out/honest.bin", 1, "")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("returned size = %d, want %d", n, len(body))
	}
	got, err := os.ReadFile(filepath.Join(root, "out", "honest.bin"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("assembled content = %q (%v)", got, err)
	}
}

// Assembly must never destroy a file that is already at the destination. It
// used to os.Create the real path, so any later failure branch removed the
// user's file along with its own output.
func TestAssembleNeverDestroysAnExistingDestination(t *testing.T) {
	ls, root := newChunkStore(t, 0)
	ctx := context.Background()

	dest := filepath.Join(root, "out", "precious.bin")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("PRECIOUS"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A session that will fail its size check.
	long := []byte("much longer payload than declared")
	seedSession(ls, "clobber", 1, 1)
	if _, err := ls.WriteChunk(ctx, "clobber", 0, bytes.NewReader(long), int64(len(long))); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AssembleChunks(ctx, "clobber", "out/precious.bin", 1, ""); err != ErrSizeMismatch {
		t.Fatalf("error = %v, want ErrSizeMismatch", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "PRECIOUS" {
		t.Fatalf("the existing file was destroyed by a failed assembly: %q (%v)", got, err)
	}
}

// A checksum mismatch must also leave the destination alone.
func TestAssembleChecksumFailureLeavesTheDestination(t *testing.T) {
	ls, root := newChunkStore(t, 0)
	ctx := context.Background()

	dest := filepath.Join(root, "out", "precious.bin")
	os.MkdirAll(filepath.Dir(dest), 0o755)
	os.WriteFile(dest, []byte("PRECIOUS"), 0o644)

	body := []byte("new content")
	seedSession(ls, "badsum", int64(len(body)), 1)
	if _, err := ls.WriteChunk(ctx, "badsum", 0, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AssembleChunks(ctx, "badsum", "out/precious.bin", 1, "00ff"); err != ErrChecksumMismatch {
		t.Fatalf("error = %v, want ErrChecksumMismatch", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "PRECIOUS" {
		t.Fatalf("checksum failure destroyed the destination: %q", got)
	}
}
