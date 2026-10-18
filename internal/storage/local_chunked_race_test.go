package storage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// WriteChunk reads "is this index already here" under the lock, drops it for
// the duration of the disk write, and picks it up again afterwards. Deciding
// from the first read is a check-then-act across that gap: concurrent requests
// for one index all saw the index missing, all reported themselves new, and the
// session ended up claiming more chunks than the disk held. AssembleChunks
// then refused the upload forever with "not all chunks uploaded", and the only
// way out was to abandon it and start again.
//
// The index is now recorded under the same lock that decides it, which makes
// the check-and-set atomic: exactly one caller can win.
func TestConcurrentWritesOfOneIndexCountOnce(t *testing.T) {
	const racers = 8
	const chunkSize = 64 * 1024

	ls := newTestStorage(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("a"), chunkSize)

	// Five chunks' worth of declared size so the cap check never interferes
	// with what is being tested.
	ls.CreateUploadSession("up", "race.bin", "race.bin", 5*chunkSize, chunkSize, 5, time.Hour)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		newOnce int
		errs    []error
	)
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, so they land in the window
			isNew, err := ls.WriteChunk(ctx, "up", 0, bytes.NewReader(body), chunkSize)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if isNew {
				newOnce++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent writes returned errors: %v", errs)
	}
	if newOnce != 1 {
		t.Errorf("WriteChunk reported %d writes as new for one index, want exactly 1", newOnce)
	}

	ls.mu.RLock()
	got := ls.activeUploads["up"].UploadedChunks
	ls.mu.RUnlock()
	if got != 1 {
		t.Errorf("session counted %d chunks after 8 writes to index 0, want 1", got)
	}
}

// The counter above is what the session believes; what matters is whether the
// upload can still be finished. Fill the rest and assemble.
func TestConcurrentDuplicateDoesNotStrandTheSession(t *testing.T) {
	const chunkSize = 64 * 1024

	ls := newTestStorage(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("b"), chunkSize)

	ls.CreateUploadSession("up", "ok.bin", "ok.bin", 5*chunkSize, chunkSize, 5, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ls.WriteChunk(ctx, "up", 0, bytes.NewReader(body), chunkSize) //nolint:errcheck
		}()
	}
	wg.Wait()

	for i := 1; i < 5; i++ {
		if _, err := ls.WriteChunk(ctx, "up", i, bytes.NewReader(body), chunkSize); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	size, err := ls.AssembleChunks(ctx, "up", "ok.bin", 5, "")
	if err != nil {
		t.Fatalf("AssembleChunks after a concurrent duplicate: %v", err)
	}
	if size != 5*chunkSize {
		t.Errorf("assembled %d bytes, want %d", size, 5*chunkSize)
	}

	got, err := os.ReadFile(filepath.Join(ls.rootDir, "ok.bin"))
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("b"), 5*chunkSize)) {
		t.Error("assembled file is not five copies of the chunk")
	}
}

// A re-send after a dropped response has to keep working right up to the cap,
// which is why the duplicate test runs before the ceiling rather than after it:
// a last chunk that is already on disk must not be refused because re-counting
// it would have crossed the limit.
func TestDuplicateAtTheCapIsStillAccepted(t *testing.T) {
	const chunkSize = 64 * 1024

	ls := newTestStorage(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("c"), chunkSize)

	// The ceiling is exactly the file, so the fifth chunk lands on the limit.
	ls.limits.SetMaxFileSize(5 * chunkSize)
	ls.CreateUploadSession("up", "cap.bin", "cap.bin", 5*chunkSize, chunkSize, 5, time.Hour)

	for i := 0; i < 5; i++ {
		isNew, err := ls.WriteChunk(ctx, "up", i, bytes.NewReader(body), chunkSize)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if !isNew {
			t.Errorf("chunk %d reported as not new on its first write", i)
		}
	}

	// The client never saw the acknowledgement for the last chunk and sends it
	// again. That must succeed and must not be counted twice.
	isNew, err := ls.WriteChunk(ctx, "up", 4, bytes.NewReader(body), chunkSize)
	if err != nil {
		t.Fatalf("re-sending the final chunk at the ceiling: %v", err)
	}
	if isNew {
		t.Error("a repeated index at the ceiling was reported as new")
	}

	if _, err := ls.AssembleChunks(ctx, "up", "cap.bin", 5, ""); err != nil {
		t.Fatalf("AssembleChunks after retrying the final chunk: %v", err)
	}
}

// The sweeper can take a session while WriteChunk is writing its chunk. The
// pointer stays valid, but the session is unreachable and its chunks have been
// removed, so counting into it would record bytes nothing reclaims.
func TestWriteChunkAfterTheSweeperTakesTheSession(t *testing.T) {
	const chunkSize = 64 * 1024

	ls := newTestStorage(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("d"), chunkSize)

	ls.CreateUploadSession("gone", "x.bin", "x.bin", chunkSize, chunkSize, 1, time.Hour)
	ls.DeleteUploadSession("gone")

	if _, err := ls.WriteChunk(ctx, "gone", 0, bytes.NewReader(body), chunkSize); err != ErrUploadNotFound {
		t.Errorf("writing to a swept session returned %v, want ErrUploadNotFound", err)
	}
}

// Two sessions uploading at once must not see each other's counters. Run under
// -race this is what would have caught AssembleChunks reading UploadedChunks
// after releasing the lock it was fetched under.
func TestConcurrentSessionsKeepTheirOwnCounters(t *testing.T) {
	const sessions = 6
	const chunkSize = 32 * 1024

	ls := newTestStorage(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		id := fmt.Sprintf("up%d", s)
		ls.CreateUploadSession(id, "f.bin", fmt.Sprintf("f%d.bin", s), chunkSize, chunkSize, 1, time.Hour)
		wg.Add(1)
		go func(s int, id string) {
			defer wg.Done()
			if _, err := ls.WriteChunk(ctx, id, 0, bytes.NewReader(bytes.Repeat([]byte("e"), chunkSize)), chunkSize); err != nil {
				t.Errorf("%s: %v", id, err)
				return
			}
			if _, err := ls.AssembleChunks(ctx, id, fmt.Sprintf("f%d.bin", s), 1, ""); err != nil {
				t.Errorf("%s assemble: %v", id, err)
			}
		}(s, id)
	}
	wg.Wait()

	for s := 0; s < sessions; s++ {
		if _, err := os.Stat(filepath.Join(ls.rootDir, fmt.Sprintf("f%d.bin", s))); err != nil {
			t.Errorf("f%d.bin missing: %v", s, err)
		}
	}
}
