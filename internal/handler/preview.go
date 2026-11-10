package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yuarcloud/yuarcloud/internal/auth"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// Preview streams a file's bytes inline instead of as an attachment, so the
// browser can render it instead of saving it.
//
// The difference from /download is not cosmetic. It answers Range requests,
// which is what makes seeking in a video or an audio file work and what keeps
// a 2 GB film from being pulled into memory before the first frame appears.

// maxPreviewSize caps a single inline response. Range requests still work
// above it — the cap only applies to a whole-file GET, so an unsupported
// binary blob cannot be used to push an unbounded payload through the API.
const maxPreviewSize = 256 << 20 // 256 MiB

// UploadLimits reports the ceilings an uploader needs to know about.
//
// This exists because the browser decides on its own whether to send a file in
// one request or split it into chunks, how big those chunks are, and how many
// files to send at once. All three decisions are only correct if they respect
// the server's configuration — a client that hard-codes a chunk size will start
// failing the moment an admin tunes max_chunk_size, which is exactly what the
// Chunked Upload settings page is for. Any signed-in user may read this; it is
// configuration, not secrets.
func (h *FileHandler) UploadLimits(c *gin.Context) {
	limits := h.storage.GetLimits()
	parallelChunks := h.maxParallelChunks(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"max_chunk_size":         limits.MaxChunkSize(),
		"max_file_size":          limits.MaxFileSize(),
		"max_concurrent_uploads": h.maxConcurrentUploads(c.Request.Context()),
		"max_parallel_chunks":    parallelChunks,
	})
}

func (h *FileHandler) maxParallelChunks(ctx context.Context) int {
	if h.db == nil {
		return defaultMaxParallelChunks
	}
	var setting model.Setting
	if err := h.db.WithContext(ctx).
		Select("value").
		First(&setting, "key = ?", settingKeyMaxParallelChunks).Error; err != nil {
		return defaultMaxParallelChunks
	}
	v, err := strconv.ParseInt(strings.TrimSpace(setting.Value), 10, 64)
	if err != nil {
		return defaultMaxParallelChunks
	}
	return normalizeParallelChunks(v)
}

// maxConcurrentUploads reads the admin's parallel-upload choice from settings.
//
// The limit is enforced by the browser, not by this process, so it cannot live
// in the storage limits struct that the upload path reads. Reading the row on
// each limits request keeps a saved change visible to the next uploader
// without a restart; the client caches the answer for the session.
func (h *FileHandler) maxConcurrentUploads(ctx context.Context) int {
	if h.db == nil {
		return defaultMaxConcurrentUploads
	}
	var setting model.Setting
	if err := h.db.WithContext(ctx).
		Select("value").
		First(&setting, "key = ?", settingKeyMaxConcurrentUploads).Error; err != nil {
		return defaultMaxConcurrentUploads
	}
	v, err := strconv.ParseInt(strings.TrimSpace(setting.Value), 10, 64)
	if err != nil {
		return defaultMaxConcurrentUploads
	}
	return normalizeConcurrentUploads(v)
}

// IssuePreviewToken mints the short-lived token the viewer puts in its URL.
//
// It sits behind the ordinary authenticated middleware, so the session access
// token never leaves a header. The token it returns is good for this one file
// for five minutes, and is refused by every other endpoint.
func (h *FileHandler) IssuePreviewToken(c *gin.Context) {
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	// Same scope check as the preview itself: a token is a capability, and
	// minting one for a file outside the caller's home would hand out access.
	file, ok := h.lookupFile(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}
	if file.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot preview a folder"})
		return
	}

	claims, ok := middleware.GetClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return
	}
	role, _ := middleware.GetUserRole(c)

	token, err := h.authService.GeneratePreviewToken(claims.UserID, claims.Username, role, file.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue preview token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_in": int64(auth.PreviewTokenTTL.Seconds()),
	})
}

// PreviewFile serves a file to its owner, or to anyone whose home contains it.
// Reachable with a preview token in the query as well as an Authorization
// header, because <img>/<video>/<audio> cannot send headers.
func (h *FileHandler) PreviewFile(c *gin.Context) {
	// Video and audio previews stream for as long as the viewer watches; the
	// response deadline must not end them.
	liftTransferDeadlines(c)

	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	// Scoped lookup: the id alone must never be enough to read outside the
	// caller's home, which is the same rule the download endpoint follows.
	file, ok := h.lookupFile(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	// A ?w= hint asks for a screen-sized copy instead of the original, which is
	// what keeps a multi-megabyte photo from crossing a slow link in full. It is
	// best-effort: anything ServeThumb cannot render falls through to the
	// original below.
	if h.thumbs != nil && h.thumbs.ServeThumb(c, file, previewWidth(c.Query("w")), true) {
		return
	}

	serveInline(c, h.storage, file, true)
}

// serveInline writes file to c with inline disposition and Range handling.
//
// cacheable asks for a Cache-Control the browser may reuse. It is the caller's
// decision because the URL carries the credential: the authenticated preview
// is backed by a five-minute token, so a cached copy cannot outlive the access
// it was granted for. A public share link can be revoked at any moment, so the
// share route does not opt in.
func serveInline(c *gin.Context, store storage.StorageBackend, file *model.File, cacheable bool) {
	if file.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot preview a folder"})
		return
	}

	mime := file.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}

	c.Header("Content-Type", mime)
	// inline, and the filename is quoted so spaces and non-ASCII survive.
	// `filename*` additionally gives browsers the real UTF-8 name for a
	// "save as", which the plain ASCII parameter cannot always carry.
	c.Header("Content-Disposition", contentDisposition("inline", file.Name))
	// Active content gets a sandboxed origin with no script execution.
	//
	// The MIME type is whatever the uploader declared, so a file named .pdf
	// could arrive as text/html and the PDF viewer's iframe would render it as
	// a page on this origin - with localStorage access, where the session
	// tokens live. Sandbox (without allow-scripts) keeps the document
	// displayable, which is why HTML and SVG previews still work, but nothing
	// in it can run.
	if activeContentType(mime) {
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; media-src data: blob:")
	}
	// Tells the player it may seek. Without this a media element still plays,
	// but the scrubber is dead and some browsers refuse to start at all.
	c.Header("Accept-Ranges", "bytes")
	c.Header("X-Content-Type-Options", "nosniff")

	// The size on the row can be stale — an adopted file, an old migration, a
	// copy that predates the size fix. Declaring that number as Content-Length
	// makes the server abort the moment the body runs longer ("wrote more than
	// the declared Content-Length"), which the browser sees as a truncated or
	// empty preview. The bytes on disk are the authority, exactly as in the
	// download path.
	size := file.Size
	if info, statErr := store.Stat(context.Background(), file.Path); statErr == nil && !info.IsDir {
		size = info.Size
	}

	// A Range header means the player only wants a slice. Honour it and reply
	// 206; a range we cannot parse falls through to the full body, and one that
	// points outside the file is a 416.
	//
	// The three outcomes are gated on `rangeOK` rather than handled as a switch
	// with early returns, because the fall-through case has to reach the full-
	// body code *below the whole block*. A `break` leaves a Go switch and lands
	// on the next statement, which here is the 206 path — so a header we did
	// not understand was answered 206 with `Content-Range: bytes 0-0/size` and
	// a single byte of content. That looks like a successful partial response,
	// so the client stops asking for the rest of the file.
	if rangeHeader := c.GetHeader("Range"); rangeHeader != "" && size > 0 {
		start, end, status := parseRange(rangeHeader, size)

		if status == rangeUnsatisfiable {
			c.Header("Content-Range", fmt.Sprintf("bytes */%d", size))
			c.Status(http.StatusRequestedRangeNotSatisfiable)
			return
		}

		if status == rangeOK {
			reader, err := store.ReadRange(context.Background(), file.Path, start, end-start+1)
			if err != nil {
				writeStorageError(c, err)
				return
			}
			defer reader.Close() //nolint:errcheck

			length := end - start + 1
			c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			c.Header("Content-Length", strconv.FormatInt(length, 10))
			allowClientCache(c, cacheable)
			c.Status(http.StatusPartialContent)
			if c.Request.Method == http.MethodHead {
				return
			}
			// Copy through io.Copy so a client that disconnects mid-stream does
			// not leave us writing into a dead connection.
			io.Copy(c.Writer, reader) //nolint:errcheck
			return
		}
		// rangeMalformed: ignored, per RFC 9110 §14.2. Fall through to the full
		// body.
	}

	if size > maxPreviewSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": "file is too large to preview inline",
			"size":  size,
			"hint":  "download the file instead",
		})
		return
	}

	reader, err := store.Read(context.Background(), file.Path)
	if err != nil {
		writeStorageError(c, err)
		return
	}
	defer reader.Close() //nolint:errcheck

	c.Header("Content-Length", strconv.FormatInt(size, 10))
	allowClientCache(c, cacheable)
	c.Status(http.StatusOK)
	if c.Request.Method == http.MethodHead {
		return
	}
	io.Copy(c.Writer, reader) //nolint:errcheck
}

// allowClientCache marks a successful preview as reusable by the browser.
//
// Without a Cache-Control (and with no Last-Modified or ETag to fall back on)
// a browser has nothing to hold on to, so closing and reopening an image
// downloaded the whole original again and re-decoded it. The URL is stable
// while its preview token lives, so the copy is safe for exactly that long;
// `private` keeps it out of any shared cache on the way. The header is added
// only on the success paths, so a 416, a 413 or a storage error is never
// remembered as the answer for that file.
func allowClientCache(c *gin.Context, enabled bool) {
	if !enabled {
		return
	}
	c.Header("Cache-Control", "private, max-age=300")
	// The preview endpoint is reachable both with a query token and with an
	// Authorization header (the parsed viewers use the latter). Keep the two
	// apart in any cache that keys by URL alone.
	c.Header("Vary", "Authorization")
}

// parseRange understands the single-range forms a browser media element
// actually sends: "bytes=0-", "bytes=500-999", "bytes=-500" (the last 500
// bytes).
//
// It distinguishes two failures, because they are not the same thing:
//   - malformed — RFC 9110 §14.2 says an unparseable Range is ignored and the
//     full representation served with 200
//   - unsatisfiable — syntactically fine but outside the file, which is 416
//
// The two used to collapse into a single `ok` that answered 416 either way,
// so a preview that played in Chrome failed for anything emitting a form this
// function does not model: a unit range, a multi-range request, a stray
// `Range: bytes=0-1,3-4`. All of those are legal to receive and all of them
// are meant to be served in full.
func parseRange(header string, size int64) (start, end int64, status parseRangeStatus) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, rangeMalformed
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if strings.Contains(spec, ",") {
		// Multi-range is legal and this does not serve it. Per RFC 9110 §14.2
		// the correct response is the whole thing, not a refusal.
		return 0, 0, rangeMalformed
	}

	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, rangeMalformed
	}
	fromStr := strings.TrimSpace(spec[:dash])
	toStr := strings.TrimSpace(spec[dash+1:])

	// Suffix form: the last N bytes.
	if fromStr == "" {
		n, err := strconv.ParseInt(toStr, 10, 64)
		if err != nil {
			return 0, 0, rangeMalformed
		}
		if n <= 0 {
			// "bytes=-0" asks for nothing at all. Syntactically valid, and
			// there is no byte range that satisfies it.
			return 0, 0, rangeUnsatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, rangeOK
	}

	start, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, rangeMalformed
	}
	if start >= size {
		return 0, 0, rangeUnsatisfiable
	}

	// Open form: "bytes=500-" runs to the end of the file.
	if toStr == "" {
		return start, size - 1, rangeOK
	}

	end, err = strconv.ParseInt(toStr, 10, 64)
	if err != nil || end < start {
		return 0, 0, rangeMalformed
	}
	if end >= size {
		end = size - 1
	}
	return start, end, rangeOK
}

type parseRangeStatus int

const (
	rangeMalformed parseRangeStatus = iota
	rangeUnsatisfiable
	rangeOK
)

func writeStorageError(c *gin.Context, err error) {
	if err == storage.ErrNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found in storage"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read file"})
}

// writeWriteError answers a failure from a write path.
//
// A 409 for the collision cases is not cosmetic. os.Create truncates, so
// "something is already at that path" is a conflict the caller can act on, not
// a server fault — and a 500 sent them looking in the wrong place.
func writeWriteError(c *gin.Context, err error, what string) {
	switch err {
	case storage.ErrNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": what + ": destination folder not found"})
		return
	case storage.ErrNotADirectory:
		c.JSON(http.StatusConflict, gin.H{"error": what + ": a file already exists at that path"})
		return
	case storage.ErrFileTooLarge:
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds the maximum upload size"})
		return
	case storage.ErrSizeMismatch:
		c.JSON(http.StatusBadRequest, gin.H{"error": "written size does not match what was sent"})
		return
	case storage.ErrChecksumMismatch:
		c.JSON(http.StatusBadRequest, gin.H{"error": "the file did not match the checksum it was uploaded with"})
		return
	case storage.ErrIncompleteUpload:
		c.JSON(http.StatusConflict, gin.H{"error": "some chunks never arrived; restart the upload"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": what + ": failed in storage"})
}

// asciiFallback strips a filename down to characters that are legal in the
// quoted-string form of Content-Disposition. The real name still rides along
// in the filename* parameter, so nothing is actually lost.
func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127:
			b.WriteByte('_') // control characters, including the quote
		case r > 126:
			b.WriteByte('_')
		case r == '"' || r == '\\':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// contentDisposition builds the header for a download or an inline preview.
//
// Four call sites formatted it by hand and three of them interpolated the
// stored name straight into a quoted-string:
//
//	Content-Disposition: attachment; filename="a".txt"
//
// A name containing a quote closes the string early, so the value after it is
// no longer part of the filename parameter at all — the header stops being
// what it claims and the browser's suggested save name becomes attacker-
// chosen. Control characters are simply illegal inside a quoted-string.
// (Response splitting is not the risk it looks like: net/http strips CR and LF
// from header values, verified. It is the grammar, not the newline.)
//
// asciiFallback + urlEscape is the RFC 6266 pair — a safe ASCII form plus the
// real UTF-8 name — and it already existed here for previews. One function, all
// sites, so the next download route cannot reintroduce it.
func contentDisposition(disposition, name string) string {
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s",
		disposition, asciiFallback(name), urlEscape(name))
}

// activeContentType reports whether a browser will treat the decoded bytes as a
// document rather than as media that cannot execute. The comparison is on the
// type only: parameters such as charset do not change the answer.
func activeContentType(mime string) bool {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "text/html", "application/xhtml+xml", "image/svg+xml",
		"application/xml", "text/xml", "application/javascript", "text/javascript":
		return true
	}
	return false
}
