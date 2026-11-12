package handler

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
)

// Folder download as a ZIP, streamed straight into the response so a large
// folder never has to be assembled in memory first.

// ---------------------------------------------------------------------------
// Folder download as ZIP
// ---------------------------------------------------------------------------

func (h *FileHandler) DownloadFolder(c *gin.Context) {
	// The archive is streamed as it is built and can take much longer than the
	// server's response deadline.
	liftTransferDeadlines(c)

	userID, _ := middleware.GetUserID(c)
	sc := reqScope(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var folder model.File
	if err := scoped(sc, h.db).First(&folder, "id = ? AND is_trashed = ?", fileID, false).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
		return
	}
	if !folder.IsDir {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a folder"})
		return
	}

	c.Header("Content-Disposition", contentDisposition("attachment", folder.Name+".zip"))
	c.Header("Content-Type", "application/zip")

	// Walk the disk rather than the files table. A library adopted lazily may
	// have descendants without rows, and a FileBrowser-style folder download
	// must include them. The storage layer already hides its own bookkeeping
	// directories; symlinks are skipped so the archive cannot escape the tree.
	zw := zip.NewWriter(c.Writer)
	defer zw.Close()
	writeFolderZip(context.Background(), h.storage, zw, folder.Path)

	logAudit(h.db, c, userID, "download_folder", "file", &fileID, "Downloaded as zip: "+folder.Name)
}

// ---------------------------------------------------------------------------
// Batch download: several picked files/folders into one zip
// ---------------------------------------------------------------------------

// downloadZipBody is the request body of DownloadFilesZip: a list of ids.
// A separate POST route (not a comma-joined GET param) keeps the URL encoding
// simple and lets the size cap live in one place.
type downloadZipBody struct {
	IDs []uuid.UUID `json:"ids" binding:"required,min=1,max=100"`
}

// DownloadFilesZip streams one zip containing every selected item. Files are
// stored at their own root with a sibling-name collision handled by a numeric
// suffix; folders contribute their contents under their own name. Items that
// are not visible to the caller (outside the home subtree, trashed) are
// skipped so the archive is honest about what the user can actually see.
func (h *FileHandler) DownloadFilesZip(c *gin.Context) {
	// Same as DownloadFolder: the zip is written while it streams.
	liftTransferDeadlines(c)

	var body downloadZipBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids are required"})
		return
	}

	files, ok := h.resolveZipItems(c, body.IDs)
	if !ok {
		return
	}
	h.streamZip(c, files)
}

// DownloadFilesZipToken is the same archive, selected by the download token
// instead of a JSON body. A browser cannot POST when it follows a link, so the
// id set rides in the token minted by IssueBatchDownloadToken.
func (h *FileHandler) DownloadFilesZipToken(c *gin.Context) {
	liftTransferDeadlines(c)

	claims, ok := middleware.GetClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return
	}

	files, ok := h.resolveZipItems(c, claims.FileIDs)
	if !ok {
		return
	}
	h.streamZip(c, files)
}

// resolveZipItems dedupes the requested ids and returns the rows the caller is
// allowed to download. It writes the error response itself and reports false.
//
// Resolving through the scoped query is what makes an id from another home
// simply absent rather than an error.
func (h *FileHandler) resolveZipItems(c *gin.Context, requested []uuid.UUID) ([]model.File, bool) {
	ids := make([]uuid.UUID, 0, len(requested))
	seen := make(map[uuid.UUID]struct{}, len(requested))
	for _, id := range requested {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids are required"})
		return nil, false
	}

	var files []model.File
	if err := scoped(reqScope(c), h.db).Where("id IN ? AND is_trashed = ?", ids, false).Find(&files).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load files"})
		return nil, false
	}
	if len(files) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no downloadable items found"})
		return nil, false
	}
	return files, true
}

// streamZip writes the archive for an already-resolved item set.
func (h *FileHandler) streamZip(c *gin.Context, files []model.File) {
	userID, _ := middleware.GetUserID(c)

	name := "selection"
	if len(files) == 1 {
		name = files[0].Name
	}
	c.Header("Content-Disposition", contentDisposition("attachment", name+".zip"))
	c.Header("Content-Type", "application/zip")

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	// Collision handling: two sibling files of the same name in different
	// folders would overwrite each other in a flat zip, so the later one
	// gets a numeric suffix.
	taken := make(map[string]struct{})

	for _, file := range files {
		if file.IsDir {
			// Folders keep their internal structure under a uniquely named
			// top-level entry.
			rootName := uniqueZipName(taken, file.Name, true)
			entries, err := h.storage.List(c, file.Path)
			if err != nil {
				continue
			}
			writeZipTree(c, h.storage, zw, entries, rootName)
		} else {
			reader, err := h.storage.Read(c, file.Path)
			if err != nil {
				continue
			}
			outName := uniqueZipName(taken, file.Name, false)
			out, err := zw.Create(outName)
			if err != nil {
				reader.Close()
				continue
			}
			if _, err := io.Copy(out, reader); err != nil {
				reader.Close()
				continue
			}
			reader.Close()
		}
	}

	if len(files) > 0 {
		logAudit(h.db, c, userID, "download_batch", "file", &files[0].ID, fmt.Sprintf("Downloaded %d items as zip", len(files)))
	}
}

// writeZipTree streams a pre-listed entry set into zw. `prefix` is the
// archive-relative path already accumulated above this level (the folder's
// own name for the first call, "" for nothing). Recursion must keep carrying
// the full prefix: a file two levels down under docs/a.txt is written as
// <prefix>/a/b/a.txt, not just a/b/a.txt under a lost root.
func writeZipTree(ctx context.Context, store storage.StorageBackend, zw *zip.Writer, entries []*storage.FileInfo, prefix string) {
	for _, entry := range entries {
		if entry == nil || entry.IsSymlink {
			continue
		}
		rel := entry.Name
		if prefix != "" {
			rel = prefix + "/" + entry.Name
		}
		if entry.IsDir {
			writeZipTree(ctx, store, zw, mustList(ctx, store, entry.Path), rel)
			continue
		}
		reader, err := store.Read(ctx, entry.Path)
		if err != nil {
			continue
		}
		out, err := zw.Create(rel)
		if err != nil {
			reader.Close()
			return
		}
		if _, err := io.Copy(out, reader); err != nil {
			reader.Close()
			return
		}
		reader.Close()
	}
}

func mustList(ctx context.Context, store storage.StorageBackend, dir string) []*storage.FileInfo {
	entries, _ := store.List(ctx, dir)
	return entries
}

// uniqueZipName keeps a flat archive's top-level entries from colliding.
// Folders keep their whole name; a file's extension is preserved around the
// numeric suffix. Shared by the authenticated batch download and the public
// multi-item share so both produce the same names.
func uniqueZipName(taken map[string]struct{}, raw string, isDir bool) string {
	base, ext := raw, ""
	if !isDir {
		if idx := lastDot(raw); idx > 0 {
			base, ext = raw[:idx], raw[idx:]
		}
	}
	candidate := raw
	for n := 1; ; n++ {
		if _, used := taken[candidate]; !used {
			break
		}
		candidate = fmt.Sprintf("%s (%d)%s", base, n, ext)
	}
	taken[candidate] = struct{}{}
	return candidate
}

func lastDot(s string) int {
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// writeFolderZip streams every regular file below root into zw, using paths
// relative to root. It is shared by the authenticated and public folder
// download handlers so the two archives cannot drift apart.
//
// Unreadable entries are skipped to keep the archive valid; listing metadata
// (size, mime) is not needed here because the bytes come from storage.
func writeFolderZip(ctx context.Context, store storage.StorageBackend, zw *zip.Writer, root string) {
	if store == nil {
		return
	}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := store.List(ctx, dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry == nil || entry.IsSymlink {
				continue
			}
			childRel := path.Join(rel, entry.Name)
			if entry.IsDir {
				walk(entry.Path, childRel)
				continue
			}
			reader, err := store.Read(ctx, entry.Path)
			if err != nil {
				continue
			}
			out, err := zw.Create(childRel)
			if err != nil {
				reader.Close()
				return
			}
			if _, err := io.Copy(out, reader); err != nil {
				reader.Close()
				return
			}
			reader.Close()
		}
	}
	walk(root, "")
}
