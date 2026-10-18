package handler

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/model"
)

// Quota was stored, displayed and editable in the admin panel, and consulted
// nowhere. These pin the part that matters: a user cannot put more bytes on
// disk than they are allowed, on any of the three doors in.
func quotaUser(t *testing.T, db *gorm.DB, used, quota int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	u := model.User{
		ID: id, Username: "q" + id.String()[:8], Email: id.String() + "@test",
		PasswordHash: "x", UsedBytes: used, QuotaBytes: quota, IsActive: true,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func openSession(t *testing.T, db *gorm.DB, userID uuid.UUID, size int64) {
	t.Helper()
	s := model.UploadSession{
		ID: uuid.New(), UserID: userID, Filename: "f", TargetPath: "f",
		TotalSize: size, ChunkSize: 1, TotalChunks: 1,
		Status: "uploading", ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func quotaTestHandler(t *testing.T) (*FileHandler, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(freshDSN(t)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.AuditLog{}, &model.UploadSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewFileHandler(db, nil, nil), db
}

func TestQuotaExceeded(t *testing.T) {
	h, db := quotaTestHandler(t)
	_ = h

	cases := []struct {
		name                       string
		used, quota, add, inflight int64
		wantOver                   bool
		wantShort                  int64
	}{
		{"well under", 100, 1000, 100, 0, false, 0},
		{"exactly at the limit is allowed", 900, 1000, 100, 0, false, 0},
		{"one byte over", 900, 1000, 101, 0, true, 1},
		{"quota of zero means unlimited", 9_999_999, 0, 1, 0, false, 0},
		{"negative quota also means unlimited", 1, -1, 1, 0, false, 0},
		{"in-flight bytes count against the limit", 100, 1000, 100, 800, false, 0},
		{"in-flight bytes can push it over", 100, 1000, 100, 801, true, 1},
		{"in-flight alone can exceed it", 0, 1000, 0, 1001, true, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := quotaUser(t, db, tc.used, tc.quota)
			over, short := quotaExceeded(db, user, tc.add, tc.inflight)
			if over != tc.wantOver {
				t.Fatalf("over = %v, want %v", over, tc.wantOver)
			}
			if over && short != tc.wantShort {
				t.Fatalf("short_by = %d, want %d", short, tc.wantShort)
			}
		})
	}
}

// A user must not be able to sit at exactly their quota and then start a
// hundred uploads that each individually fit. The declared sizes of live
// sessions are what closes that, which is why this exists instead of a cap on
// how many sessions may be open.
func TestManySessionsCannotTogetherExceedQuota(t *testing.T) {
	h, db := quotaTestHandler(t)
	_ = h
	user := quotaUser(t, db, 0, 1000)

	// Check-then-create, the order the handler uses: the session being
	// considered is not yet in flight when the question is asked.
	opened, refusedAt := 0, 0
	for i := 0; i < 100; i++ {
		if over, _ := quotaExceeded(db, user, 900, inflightUploadBytes(db, user)); over {
			refusedAt = i + 1
			break
		}
		openSession(t, db, user, 900)
		opened++
	}

	if refusedAt == 0 {
		t.Fatal("100 sessions of 900 bytes against a 1000 byte quota were all accepted")
	}
	if refusedAt != 2 || opened != 1 {
		t.Fatalf("accepted %d session(s) and refused at #%d; want 1 accepted, refused at #2", opened, refusedAt)
	}
}

// Sessions the sweeper has not reclaimed yet must not keep holding quota, or a
// single abandoned upload locks the user out of their own remaining space until
// the next cleanup tick.
func TestExpiredSessionsDoNotHoldQuota(t *testing.T) {
	h, db := quotaTestHandler(t)
	_ = h
	user := quotaUser(t, db, 0, 1000)

	s := model.UploadSession{
		ID: uuid.New(), UserID: user, Filename: "f", TargetPath: "f",
		TotalSize: 5000, ChunkSize: 1, TotalChunks: 1,
		Status: "uploading", ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("seed expired session: %v", err)
	}

	if got := inflightUploadBytes(db, user); got != 0 {
		t.Fatalf("inflightUploadBytes = %d, want 0: an expired session holds no quota", got)
	}
	if over, _ := quotaExceeded(db, user, 900, inflightUploadBytes(db, user)); over {
		t.Fatal("a stale session pushed the user over quota")
	}
}

// Finished sessions are not reservations. Their bytes are now in used_bytes.
func TestCompletedSessionsDoNotCountAsInFlight(t *testing.T) {
	h, db := quotaTestHandler(t)
	_ = h
	user := quotaUser(t, db, 900, 1000)

	for _, status := range []string{"completed", "failed", "expired"} {
		s := model.UploadSession{
			ID: uuid.New(), UserID: user, Filename: "f", TargetPath: "f",
			TotalSize: 9999, ChunkSize: 1, TotalChunks: 1,
			Status: status, ExpiresAt: time.Now().Add(time.Hour),
		}
		if err := db.Create(&s).Error; err != nil {
			t.Fatalf("seed %s session: %v", status, err)
		}
	}

	if got := inflightUploadBytes(db, user); got != 0 {
		t.Fatalf("inflightUploadBytes = %d, want 0 for finished sessions", got)
	}
}

// Refusing when the quota cannot be read is the safe direction. Letting the
// upload through would turn a transient database error into an unlimited one.
func TestUnreadableQuotaRefuses(t *testing.T) {
	h, db := quotaTestHandler(t)
	_ = h
	over, _ := quotaExceeded(db, uuid.New(), 1, 0)
	if !over {
		t.Fatal("an unknown user was allowed to upload")
	}
}
