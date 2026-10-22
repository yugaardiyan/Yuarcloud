package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func (ls *LocalStorage) Rename(ctx context.Context, oldPath, newPath string) error {
	oldFull := ls.resolvePath(oldPath)
	newFull := ls.resolvePath(newPath)

	if err := os.MkdirAll(filepath.Dir(newFull), 0755); err != nil {
		return err
	}

	return os.Rename(oldFull, newFull)
}

func (ls *LocalStorage) Copy(ctx context.Context, srcPath, dstPath string) error {
	srcFull := ls.resolvePath(srcPath)
	dstFull := ls.resolvePath(dstPath)

	if err := os.MkdirAll(filepath.Dir(dstFull), 0755); err != nil {
		return err
	}

	return ls.copyFileOrDir(srcFull, dstFull)
}

func (ls *LocalStorage) copyFileOrDir(src, dst string) error {
	// Lstat, not Stat: a symlink in an adopted library must not turn a folder
	// copy into a copy of whatever it points at. The importer skips links for
	// the same reason; copying has to agree or it re-admits their bytes as
	// ordinary files with no row and no charge.
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copy: refusing to follow symlink %s", src)
	}

	if info.IsDir() {
		return ls.copyDir(src, dst)
	}
	return ls.copyFile(src, dst)
}

func (ls *LocalStorage) copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// The server's own directories are not part of a user's tree. List
		// hides them, so copying them would create hidden bytes that no
		// listing can show and no row can charge.
		if isInternal(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if err := ls.copyFileOrDir(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func (ls *LocalStorage) copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func (ls *LocalStorage) Read(ctx context.Context, path string) (io.ReadCloser, error) {
	fullPath := ls.resolvePath(path)
	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return file, nil
}

// ReadRange seeks to offset and hands back a reader bounded to length.
//
// The underlying *os.File is already a ReaderAt, so this is a seek and a
// limited reader rather than a copy — a Range request costs one syscall plus
// the bytes actually asked for, no matter how large the file is.
func (ls *LocalStorage) ReadRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 0 {
		return nil, ErrInvalidRange
	}

	fullPath := ls.resolvePath(path)
	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close() //nolint:errcheck
		return nil, err
	}

	// Closing the file has to close it even after the limited reader is
	// exhausted, so wrap rather than returning the raw handle.
	return &boundedReader{Reader: io.LimitReader(file, length), closer: file}, nil
}

type boundedReader struct {
	io.Reader
	closer io.Closer
}

func (b *boundedReader) Close() error { return b.closer.Close() }

// Write and Append fsync before returning.
//
// Close alone flushes the page cache into the OS, not the data onto the
// device: after a power cut or a kernel panic the file can be there at the
// right length and full of zeroes, while the handler that created the row has
// already answered 201. The user's file is gone and the database says it
// exists. fsync is the point at which the bytes are genuinely durable, and it
// has to happen before the success response, not after.
//
// The directory entry is synced as well when a file is newly created. Without
// it the rename-into-place or create can be lost even though the contents
// survived, which is the same failure with more steps.
//
// The cost is one device flush per write, on a path that already copied the
// whole file — amortised over that copy it is nothing.
func (ls *LocalStorage) Write(ctx context.Context, path string, reader io.Reader, size int64) error {
	if max := ls.limits.MaxFileSize(); max > 0 && size > max {
		return ErrFileTooLarge
	}

	fullPath := ls.resolvePath(path)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}

	file, err := os.Create(fullPath)
	if err != nil {
		return err
	}

	if _, err = io.Copy(file, reader); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(fullPath))
}

func (ls *LocalStorage) Append(ctx context.Context, path string, reader io.Reader, size int64) error {
	fullPath := ls.resolvePath(path)

	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	if _, err = io.Copy(file, reader); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	return file.Close()
}

// syncDir flushes a directory's entries.
//
// A directory opened for reading and synced persists the names inside it. It is
// unsupported on some filesystems and on Windows; those are not the platforms
// this runs on, and a filesystem that does not implement it is not a reason to
// skip the one that is.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck
	return d.Sync()
}
