package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yuarcloud/yuarcloud/internal/imageproc"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// maxThumbSourceBytes caps the original this endpoint will decode. Past it the
// compressed bytes alone are a large share of the host's memory and the decoded
// bitmap is several times that; the fallback is to stream the original, which
// is what the preview endpoint did before any of this existed.
const maxThumbSourceBytes = 80 << 20 // 80 MiB

// maxConcurrentRenders bounds how many previews decode at once. Every other
// self-hosted front end caps this (Nextcloud's preview_concurrency_new, oCIS's
// THUMBNAILS_MAX_CONCURRENT_REQUESTS) for the same reason: one decode is cheap,
// a folder of them at once is not.
const maxConcurrentRenders = 4

// ThumbService renders and caches downscaled image previews.
//
// A nil *ThumbService is valid and means "unavailable": ServeThumb returns
// false and the caller streams the original instead. That keeps the feature
// off — as it is in every test that does not opt in — without a second code
// path through the handler.
type ThumbService struct {
	store    storage.StorageBackend
	dir      string
	renderer *imageproc.Renderer
}

// DefaultPreviewCacheDir is where generated previews live: a sibling of the
// trash and chunk directories under the storage root, carrying the same
// InternalPrefix so no listing and no client path can reach it. Keeping it on
// the storage volume means it travels with the files, and is excluded from any
// backup that already excludes the other server directories.
func DefaultPreviewCacheDir(storageRoot string) string {
	return filepath.Join(storageRoot, storage.InternalPrefix+"-previews")
}

// NewThumbService prepares the cache directory. An empty cacheDir or a nil
// store disables the service and returns (nil, nil).
func NewThumbService(store storage.StorageBackend, cacheDir string) (*ThumbService, error) {
	if cacheDir == "" || store == nil {
		return nil, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create preview cache: %w", err)
	}
	concurrency := runtime.GOMAXPROCS(0)
	if concurrency > maxConcurrentRenders {
		concurrency = maxConcurrentRenders
	}
	return &ThumbService{
		store:    store,
		dir:      cacheDir,
		renderer: imageproc.NewRenderer(concurrency),
	}, nil
}

// ServeThumb answers with a downscaled preview when it can, and reports whether
// it handled the request. A false return leaves the response untouched, so the
// caller falls back to serveInline — this is best-effort, and any decode
// failure degrades to the original rather than to an error.
func (s *ThumbService) ServeThumb(c *gin.Context, file *model.File, width int, cacheable bool) bool {
	if s == nil || s.renderer == nil || file == nil || file.IsDir {
		return false
	}
	if !imageproc.Supported(file.MimeType) {
		return false
	}
	width = imageproc.NormalizeWidth(width)
	if width == 0 || file.Size > maxThumbSourceBytes {
		return false
	}

	outMime := imageproc.OutputMime(file.MimeType)
	data, err := os.ReadFile(s.cachePath(file, width, outMime))
	if err != nil {
		rendered, renderedMime, rerr := s.render(c.Request.Context(), file, width)
		if rerr != nil {
			return false
		}
		data, outMime = rendered, renderedMime
		s.writeCache(file, width, outMime, data)
	}

	c.Header("Content-Type", outMime)
	c.Header("Content-Disposition", contentDisposition("inline", file.Name))
	c.Header("X-Content-Type-Options", "nosniff")
	allowClientCache(c, cacheable)
	// ServeContent derives Last-Modified from modtime and answers
	// If-Modified-Since and Range on its own — which is what keeps a repeat
	// open cheap even past the browser's max-age.
	http.ServeContent(c.Writer, c.Request, file.Name, file.UpdatedAt, bytes.NewReader(data))
	return true
}

func (s *ThumbService) render(ctx context.Context, file *model.File, width int) ([]byte, string, error) {
	reader, err := s.store.Read(ctx, file.Path)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close() //nolint:errcheck
	return s.renderer.Render(reader, file.MimeType, width)
}

// cachePath keys a preview on the file, the requested width, and the source's
// size and mtime. A re-upload changes those last two, so a preview of the old
// bytes stops being found and is regenerated rather than served.
func (s *ThumbService) cachePath(file *model.File, width int, mime string) string {
	ext := "jpg"
	if mime == "image/png" {
		ext = "png"
	}
	name := fmt.Sprintf("%d_%d_%d.%s", width, file.Size, file.UpdatedAt.Unix(), ext)
	return filepath.Join(s.dir, file.ID.String(), name)
}

// writeCache stores a freshly rendered preview and clears this width's older
// entries for the same file, so editing a file does not accumulate one preview
// per version.
func (s *ThumbService) writeCache(file *model.File, width int, mime string, data []byte) {
	path := s.cachePath(file, width, mime)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()        //nolint:errcheck
		os.Remove(tmpName) //nolint:errcheck
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) //nolint:errcheck
		return
	}

	base := filepath.Base(path)
	prefix := strconv.Itoa(width) + "_"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == base || !strings.HasPrefix(name, prefix) {
			continue
		}
		os.Remove(filepath.Join(dir, name)) //nolint:errcheck
	}
}

// previewWidth reads the ?w= hint, returning 0 when it is absent or outside the
// supported range. A 0 leaves the request to serveInline.
func previewWidth(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return imageproc.NormalizeWidth(n)
}
