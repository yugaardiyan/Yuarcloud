package database

import (
	"testing"
	"time"

	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

// Retention is the difference between a table that grows forever and one that
// does not. These pin the two halves of it: old rows go, and nothing newer is
// touched — the second being the one that would actually destroy evidence.

func seedAudit(t *testing.T, db *gorm.DB, ages ...time.Duration) {
	t.Helper()
	base := time.Now()
	for _, age := range ages {
		l := model.AuditLog{
			Action:    "upload",
			Details:   "seeded",
			CreatedAt: base.Add(-age),
		}
		if err := db.Create(&l).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func countAudit(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.AuditLog{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPruneAuditLogsRemovesOnlyWhatIsPastTheWindow(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)

	seedAudit(t, db,
		200*24*time.Hour, // well past
		181*24*time.Hour, // just past
		179*24*time.Hour, // just inside
		2*time.Hour,      // today
		time.Duration(0), // now
	)
	if got := countAudit(t, db); got != 5 {
		t.Fatalf("seeded %d rows, want 5", got)
	}

	n, err := PruneAuditLogs(db, AuditRetention)
	if err != nil {
		t.Fatalf("PruneAuditLogs: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d, want 2 (the 200- and 181-day rows)", n)
	}

	var left []model.AuditLog
	db.Order("created_at ASC").Find(&left)
	if len(left) != 3 {
		t.Fatalf("%d rows left, want 3", len(left))
	}
	for _, l := range left {
		if time.Since(l.CreatedAt) > AuditRetention {
			t.Errorf("row from %v survived the window", time.Since(l.CreatedAt))
		}
	}
}

// The default is the number that makes the rest of this worth doing. If it
// drifts to something tiny the table starts eating history nobody agreed to
// lose; if it drifts to something enormous the window stops being a window.
func TestDefaultRetentionIsSixMonths(t *testing.T) {
	if got := AuditRetention.Hours() / 24; got != 180 {
		t.Errorf("default retention = %v days, want 180", got)
	}
}

// A zero or negative window disables pruning rather than deleting everything.
// "Keep nothing" is a reading of a misconfigured value, not a request.
func TestPruneIsANoOpWithoutAWindow(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)
	seedAudit(t, db, 1000*24*time.Hour)

	for _, retain := range []time.Duration{0, -time.Hour} {
		n, err := PruneAuditLogs(db, retain)
		if err != nil {
			t.Fatalf("retain=%v: %v", retain, err)
		}
		if n != 0 {
			t.Errorf("retain=%v pruned %d rows, want 0", retain, n)
		}
	}
	if got := countAudit(t, db); got != 1 {
		t.Errorf("row count = %d, want 1 — a disabled window deleted something", got)
	}
}

// Running it twice must not double-count or error. The daily tick means a
// server that is restarted often runs the immediate prune just as often.
func TestPruneIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)
	seedAudit(t, db, 400*24*time.Hour, 300*24*time.Hour)

	first, err := PruneAuditLogs(db, AuditRetention)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PruneAuditLogs(db, AuditRetention)
	if err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 0 {
		t.Errorf("first=%d second=%d, want 2 then 0", first, second)
	}
}

func TestPruneOnEmptyTable(t *testing.T) {
	dir := t.TempDir()
	db := walDB(t, dir)
	defer Shutdown(db)

	n, err := PruneAuditLogs(db, AuditRetention)
	if err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d from an empty table", n)
	}
}
