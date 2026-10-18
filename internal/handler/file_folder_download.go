package handler

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
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

	var descendants []model.File
	whereSubtree(h.db.Where("is_trashed = ?", false), "path", folder.Path).
		Find(&descendants)

	c.Header("Content-Disposition", contentDisposition("attachment", folder.Name+".zip"))
	c.Header("Content-Type", "application/zip")

	// Stream straight into the response so a large folder never sits in memory.
	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	seen := make(map[string]bool, len(descendants))
	for i := range descendants {
		f := &descendants[i]
		if f.IsDir {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Path, folder.Path), "/")
		if rel == "" || seen[rel] {
			continue
		}

		reader, err := h.storage.Read(context.Background(), f.Path)
		if err != nil {
			continue // skip unreadable entries, keep the archive valid
		}
		entry, err := zw.Create(rel)
		if err != nil {
			reader.Close()
			return
		}
		if _, err := io.Copy(entry, reader); err != nil {
			reader.Close()
			return
		}
		reader.Close()
		seen[rel] = true
	}

	logAudit(h.db, c, userID, "download_folder", "file", &fileID, "Downloaded as zip: "+folder.Name)
}
