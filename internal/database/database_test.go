package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openPooled(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fk.db")
	db, err := gorm.Open(sqlite.Open(sqliteDSN(path)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)
	return db, db
}

// `PRAGMA foreign_keys = ON` issued through the pool configures exactly one
// connection. The pool is allowed 25 and grows under load, so every connection
// opened later starts with foreign keys off — and dangling rows get written
// there. Measured before the fix: 3 of 20 concurrently-held connections had
// enforcement disabled.
func TestForeignKeysHoldOnEveryPooledConnection(t *testing.T) {
	db, _ := openPooled(t)
	sqlDB, _ := db.DB()

	const workers = 20
	release := make(chan struct{})
	ready := make(chan struct{}, workers)

	var wg sync.WaitGroup
	var mu sync.Mutex
	off, checked := 0, 0

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := sqlDB.Conn(context.Background())
			if err != nil {
				return
			}
			defer c.Close()

			var on int
			if err := c.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&on); err != nil {
				return
			}
			mu.Lock()
			checked++
			if on == 0 {
				off++
			}
			mu.Unlock()

			ready <- struct{}{}
			<-release
		}()
	}

	for i := 0; i < workers; i++ {
		<-ready
	}
	close(release)
	wg.Wait()

	if checked < workers {
		t.Fatalf("only %d of %d connections were checked; the test did not actually grow the pool", checked, workers)
	}
	if off > 0 {
		t.Fatalf("%d of %d connections had foreign key enforcement OFF", off, checked)
	}
}

// And the consequence, stated as behaviour rather than as a pragma: a row that
// points at a user who does not exist must be refused.
func TestDanglingForeignKeyIsRejected(t *testing.T) {
	db, _ := openPooled(t)

	if err := db.Exec("CREATE TABLE users (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE files (id TEXT PRIMARY KEY, user_id TEXT REFERENCES users(id))").Error; err != nil {
		t.Fatal(err)
	}

	err := db.Exec("INSERT INTO files (id, user_id) VALUES ('f1', 'ghost')").Error
	if err == nil {
		t.Fatal("inserting a file whose user does not exist succeeded; foreign keys are not being enforced")
	}
}

// SQLite serialises writers. The app serves requests in parallel goroutines, so
// without a busy timeout the second concurrent write fails immediately instead
// of waiting for the first to finish.
func TestConcurrentWritesDoNotFailWithBusy(t *testing.T) {
	db, _ := openPooled(t)
	if err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)").Error; err != nil {
		t.Fatal(err)
	}

	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := db.Exec("INSERT INTO t (v) VALUES (?)", "x").Error; err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	failures := 0
	for err := range errs {
		failures++
		t.Errorf("concurrent write failed: %v", err)
	}
	if failures > 0 {
		t.Fatalf("%d concurrent writers failed", failures)
	}

	var count int64
	db.Raw("SELECT COUNT(*) FROM t").Scan(&count)
	if count != writers*10 {
		t.Fatalf("landed %d rows, want %d", count, writers*10)
	}
}

func TestSQLiteDSNKeepsAnExistingQueryString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/data/yuarcloud.db", "/data/yuarcloud.db?_foreign_keys=on&_busy_timeout=10000"},
		{"/data/yuarcloud.db?mode=rwc", "/data/yuarcloud.db?mode=rwc&_foreign_keys=on&_busy_timeout=10000"},
	}
	for _, tc := range cases {
		if got := sqliteDSN(tc.in); got != tc.want {
			t.Errorf("sqliteDSN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
