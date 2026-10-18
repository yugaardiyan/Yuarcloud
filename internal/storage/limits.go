package storage

import (
	"sync/atomic"
	"time"
)

// Limits carries the upload ceilings an admin can change while the server runs.
//
// They used to be plain int64 fields copied out of config.yaml once, in
// NewLocalStorage and again in NewFileHandler. Saving them from the admin panel
// therefore wrote a row to the database that nothing ever read back: the numbers
// on screen moved, the numbers the upload path actually enforced did not, and the
// only way to change a real limit was to edit the YAML and restart. Every
// consumer reads through these accessors instead, so a save lands on the very
// next request.
type Limits struct {
	maxFileSize  atomic.Int64
	maxChunkSize atomic.Int64
	cleanup      atomic.Int64 // time.Duration, stored as nanoseconds
}

// NewLimits seeds the runtime from the config file. main overrides these with
// whatever an admin previously saved, so a restart does not silently discard a
// setting the panel accepted weeks earlier.
func NewLimits(maxFileSize, maxChunkSize int64, cleanup time.Duration) *Limits {
	l := &Limits{}
	l.SetMaxFileSize(maxFileSize)
	l.SetMaxChunkSize(maxChunkSize)
	l.SetCleanupInterval(cleanup)
	return l
}

func (l *Limits) MaxFileSize() int64 { return l.maxFileSize.Load() }

// SetMaxFileSize accepts zero as "no ceiling": a self-hosted owner who knows
// their own disk should not be forced to pick a number that will bite them on
// the next large backup.
func (l *Limits) SetMaxFileSize(v int64) {
	if v < 0 {
		v = 0
	}
	l.maxFileSize.Store(v)
}

func (l *Limits) MaxChunkSize() int64 { return l.maxChunkSize.Load() }

func (l *Limits) SetMaxChunkSize(v int64) {
	if v < 0 {
		v = 0
	}
	l.maxChunkSize.Store(v)
}

func (l *Limits) CleanupInterval() time.Duration {
	d := time.Duration(l.cleanup.Load())
	if d < minCleanupInterval {
		return minCleanupInterval
	}
	return d
}

// minCleanupInterval is the floor on the sweeper's period.
//
// The interval comes from config and from the admin panel, and the only lower
// bound it used to carry was "greater than zero". A sub-millisecond value made
// cleanupRoutine spin as fast as the scheduler would allow, calling
// cleanupExpiredUploads in a tight loop for the lifetime of the process — a
// pegged core and a log nobody asked for, from a setting that looks like a
// typo rather than an attack. Thirty seconds is comfortably inside the
// shortest session TTL worth reclaiming, so nothing useful is lost.
const minCleanupInterval = 30 * time.Second

func (l *Limits) SetCleanupInterval(d time.Duration) {
	if d <= 0 {
		d = time.Hour
	}
	l.cleanup.Store(int64(d))
}
