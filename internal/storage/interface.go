package storage

import (
	"context"
	"io"
	"time"
)

type FileInfo struct {
	Name     string
	Path     string
	Size     int64
	MimeType string
	IsDir    bool
	// IsSymlink marks an entry that is a symbolic link. List and Stat report
	// the link itself rather than following it; the adoption path uses this
	// to skip links, because a link can point outside the storage root.
	IsSymlink bool
	ModTime   time.Time
	// Checksum is the SHA-256 of the file's contents, and it is only ever
	// populated by paths that already have the whole file in hand. Stat and
	// List deliberately leave it empty.
	//
	// They used to fill it by reading every file end to end, on every call.
	// That made browsing a directory cost the sum of its contents in disk
	// reads — opening a folder of 4GB of video re-read 4GB to produce a
	// number nothing looked at. Every consumer reads the checksum from the
	// files table, where it is stored once at upload; the listing built from
	// these structs is intersected with those rows and takes their value.
	Checksum string
}

type StorageBackend interface {
	// Basic operations
	Stat(ctx context.Context, path string) (*FileInfo, error)
	List(ctx context.Context, path string) ([]*FileInfo, error)
	Mkdir(ctx context.Context, path string) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	Copy(ctx context.Context, srcPath, dstPath string) error

	// File operations
	Read(ctx context.Context, path string) (io.ReadCloser, error)

	// ReadRange returns a reader over at most length bytes starting at offset.
	// Preview uses it to answer HTTP Range requests so that seeking in a video
	// or an audio file does not have to pull the whole body down first.
	ReadRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error)
	Write(ctx context.Context, path string, reader io.Reader, size int64) error
	Append(ctx context.Context, path string, reader io.Reader, size int64) error

	// Chunked upload
	// WriteChunk stores one chunk and reports whether the index was newly
	// recorded, so a retried chunk is not counted twice.
	WriteChunk(ctx context.Context, uploadID string, chunkIndex int, reader io.Reader, size int64) (bool, error)
	AssembleChunks(ctx context.Context, uploadID, finalPath string, totalChunks int, checksum string) (int64, error)
	CleanupChunks(ctx context.Context, uploadID string) error
	CreateUploadSession(uploadID, filename, targetPath string, totalSize, chunkSize int64, totalChunks int, ttl time.Duration) *UploadSession
	GetUploadSession(uploadID string) (*UploadSession, bool)
	DeleteUploadSession(uploadID string)

	// Utility
	GetTempDir() string
	// GetLimits exposes the runtime upload ceilings. They are read through the
	// accessor rather than copied, because an admin can change them without a
	// restart; a handler holding its own int64 copy would enforce the value that
	// was current when it was constructed.
	GetLimits() *Limits
	GetMaxFileSize() int64
	GetMaxChunkSize() int64
	Exists(ctx context.Context, path string) (bool, error)
	GetDiskUsage(ctx context.Context, path string) (used, free int64, err error)
}

type UploadSession struct {
	ID             string
	Filename       string
	TargetPath     string
	TotalSize      int64
	ChunkSize      int64
	TotalChunks    int
	UploadedChunks int
	// ReceivedBytes counts what has actually landed on disk for this session.
	//
	// TotalSize is what the client claimed in the init request and cannot be
	// trusted: nothing compared it to the bytes that arrived, so a client could
	// declare 1 byte, send a thousand chunks, and have max_file_size, the user's
	// quota and the recorded size all sailed past on the strength of the claim.
	// This is the measured number, maintained under the storage lock.
	ReceivedBytes int64
	// received tracks which chunk indices are actually on disk. A counter alone
	// cannot tell a completed upload from one that received the same chunk twice
	// and is still missing another, so the index set is the source of truth and
	// UploadedChunks is just its size.
	received  map[int]struct{}
	TempDir   string
	Checksum  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// MarkReceived records a chunk index. Callers must hold the storage lock.
func (u *UploadSession) MarkReceived(index int) {
	if u.received == nil {
		u.received = make(map[int]struct{})
	}
	u.received[index] = struct{}{}
	u.UploadedChunks = len(u.received)
}
