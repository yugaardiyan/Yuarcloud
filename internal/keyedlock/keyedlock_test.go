package keyedlock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockSerialisesSameKey(t *testing.T) {
	var l Locker
	const n = 64
	var active, peak int32
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := l.Lock("same")
			cur := atomic.AddInt32(&active, 1)
			for {
				high := atomic.LoadInt32(&peak)
				if cur <= high || atomic.CompareAndSwapInt32(&peak, high, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&active, -1)
			unlock()
		}()
	}

	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrent holders = %d, want 1", peak)
	}
	// Every goroutine released its key, so nothing may be left behind.
	if got := l.size(); got != 0 {
		t.Fatalf("size after all releases = %d, want 0", got)
	}
}

func TestLockDifferentKeysOverlap(t *testing.T) {
	var l Locker
	unlockA := l.Lock("a")

	done := make(chan struct{})
	go func() {
		unlockB := l.Lock("b")
		unlockB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("locking a different key blocked")
	}
	unlockA()
}
