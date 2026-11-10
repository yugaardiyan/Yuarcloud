// Package keyedlock serialises work per string key and releases a key's lock
// state once no goroutine holds or waits for it.
//
// The usual shape — a sync.Map of channels or mutexes — never removed an entry,
// so a long-lived server accumulated one per upload id it had ever seen. A
// refcount lets the last release delete the key without racing a goroutine that
// is already waiting on it.
package keyedlock

import "sync"

// Locker is a set of locks addressed by key. The zero value is ready to use.
// It must not be copied after first use.
type Locker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	mu   sync.Mutex
	refs int
}

// Lock acquires the lock for key and returns its release function. The release
// function must be called exactly once, normally with defer.
func (l *Locker) Lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*entry)
	}
	e := l.locks[key]
	if e == nil {
		e = &entry{}
		l.locks[key] = e
	}
	// Count this waiter before releasing the map lock, so a concurrent unlock
	// cannot drop the entry while this goroutine is still about to block on it.
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

// size reports how many keys currently have state. Only for tests.
func (l *Locker) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
