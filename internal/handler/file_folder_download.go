package handler

import (
	"archive/zip"
	"context"
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
