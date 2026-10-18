package handler

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/scope"
	"gorm.io/gorm"
)

// Rename, move and copy: three operations that differ mainly in where the result
// lands and how the subtree is rewritten, and that share resolveDestination and
// availableName to decide on that.
//
// Grouped because the rules holding them together are shared — one destination,
// never overwriting what is already there, and the whole subtree following
// along.

func (h *FileHandler) RenameFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	name := strings.TrimSpace(req.Name)
	if !validEntryName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
		return
	}

	sc := reqScope(c)
	file, ok := h.writable(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	oldPath := file.Path
	newPath := cleanPath(filepath.Join(parentDir(oldPath), name))

	// Renaming never crosses a boundary: the parent is unchanged by definition,
	// so this is a belt-and-braces check against a home that moved meanwhile.
	if !sc.Contains(newPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "outside your folder"})
		return
	}

	if newPath == oldPath {
		c.JSON(http.StatusOK, toResponse(sc, file)) // nothing to do
		return
	}
	if newPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
		return
	}
	if reservedPath(newPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot rename into a reserved location"})
		return
	}

	var existing int64
	scoped(sc, h.db.Model(&model.File{})).
		Where("path = ? AND is_trashed = ?", newPath, false).
		Count(&existing)
	if existing > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "an item with that name already exists here"})
		return
	}

	if err := h.storage.Rename(context.Background(), oldPath, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "rename")
		return
	}

	// One transaction with every row checked, for the same reason as MoveFile: the
	// rewrites were individually committed and unchecked, so a failure left the
	// disk rolled back and the descendant rows pointing at a path that was no
	// longer there.
	// Read before the transaction, for the same reason as MoveFile: a read on
	// h.db while the tx holds SQLite's write lock returns SQLITE_BUSY, Find
	// ignores it, and the subtree's rows silently keep their old paths.
	descendants := h.descendantsOf(file.UserID, oldPath)

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// The on-disk rename covered the subtree, so descendants only need DB
		// paths rewritten.
		for _, d := range descendants {
			if e := tx.Model(&model.File{}).
				Where("id = ?", d.ID).
				Update("path", newPath+strings.TrimPrefix(d.Path, oldPath)).Error; e != nil {
				return e
			}
		}

		prevName, prevPath := file.Name, file.Path
		file.Name = name
		file.Path = newPath
		file.UpdatedAt = time.Now()
		if e := tx.Save(file).Error; e != nil {
			file.Name, file.Path = prevName, prevPath
			return e
		}
		return nil
	}); err != nil {
		h.storage.Rename(context.Background(), newPath, oldPath) // roll back disk
		c.Error(err)                                             //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update file record"})
		return
	}

	logAudit(h.db, c, userID, "rename", "file", &file.ID, "Renamed to: "+name)

	c.JSON(http.StatusOK, toResponse(sc, file))
}

func (h *FileHandler) MoveFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		TargetParentID *uuid.UUID `json:"target_parent_id"`
		TargetPath     string     `json:"target_path"` // optional alternative to the id
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sc := reqScope(c)
	file, ok := h.writable(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	targetParentPath, targetParentID, ok := h.resolveDestination(c, userID, req.TargetParentID, req.TargetPath)
	if !ok {
		return
	}

	// Refuse to move a folder into itself or one of its own descendants.
	if file.IsDir && (targetParentPath == file.Path || strings.HasPrefix(targetParentPath, file.Path+"/")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot move a folder into itself"})
		return
	}

	newPath := cleanPath(path.Join(targetParentPath, file.Name))
	if newPath == "" || newPath == file.Path {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source and destination are the same"})
		return
	}

	// RenameFile and CreateFolder both check this; MoveFile did not, and
	// os.Rename replaces the destination atomically. So a file dropped into a
	// folder that already had something of that name destroyed it on disk while
	// leaving the victim's row behind — two live rows on one path, one of them
	// now describing bytes that no longer exist. The victim could then trash it
	// and take the mover's file with it.
	if h.rejectPathTaken(c, sc, newPath) || h.rejectPathOnDisk(c, newPath) {
		return
	}

	if err := h.storage.Mkdir(context.Background(), parentDir(newPath)); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "prepare destination")
		return
	}

	if err := h.storage.Rename(context.Background(), file.Path, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "move")
		return
	}

	oldPath := file.Path

	// The DB half is one transaction, and every row's error is checked.
	//
	// The descendant rewrites were separate committed writes whose errors were
	// thrown away, and the rollback moved the bytes back without undoing them. A
	// failure on the root Save therefore produced: bytes back at oldPath, root
	// row back at oldPath, and every descendant row still pointing at
	// newPath/… — a location that no longer existed. The whole subtree below the
	// root vanished from every listing and could not be selected to repair.
	// Read before the transaction. descendantsOf goes through h.db, which inside
	// a transaction is a different pooled connection — and SQLite serialises
	// writers, so the read returns SQLITE_BUSY, GORM swallows it, and the loop
	// iterates over nothing. Every descendant row keeps its old path while the
	// bytes moved, which is a subtree that cannot be listed, opened, moved or
	// deleted because none of those can find it any more.
	descendants := h.descendantsOf(file.UserID, oldPath)

	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// A single on-disk rename relocated the whole subtree, so descendants only
		// need their DB paths rewritten.
		for _, d := range descendants {
			if e := tx.Model(&model.File{}).
				Where("id = ?", d.ID).
				Update("path", newPath+strings.TrimPrefix(d.Path, oldPath)).Error; e != nil {
				return e
			}
		}

		previous, parent := file.Path, file.ParentID
		file.Path = newPath
		file.ParentID = targetParentID
		file.UpdatedAt = time.Now()
		if e := tx.Save(file).Error; e != nil {
			file.Path, file.ParentID = previous, parent
			return e
		}
		return nil
	}); err != nil {
		h.storage.Rename(context.Background(), newPath, oldPath)
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update file record"})
		return
	}

	logAudit(h.db, c, userID, "move", "file", &file.ID, "Moved to: "+newPath)

	c.JSON(http.StatusOK, toResponse(sc, file))
}

// resolveDestination turns the request into a validated destination folder
// path and parent id. Accepts either target_parent_id or target_path.
// It writes the error response itself and reports ok=false on failure.
func (h *FileHandler) resolveDestination(c *gin.Context, userID uuid.UUID, parentID *uuid.UUID, parentPath string) (string, *uuid.UUID, bool) {
	sc := reqScope(c)

	// The destination must land inside the caller's home. An unrestricted user
	// keeps the storage root; a scoped one keeps their own folder.
	if parentID != nil {
		var target model.File
		if err := scoped(sc, h.db).First(&target, "id = ? AND is_dir = ? AND is_trashed = ?",
			*parentID, true, false).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target folder not found"})
			return "", nil, false
		}
		// is_trashed already excludes the trash itself, but not the chunk
		// scratch directory or any future internal directory. Those have no
		// rows at all, so this is belt-and-braces — the point is that one
		// check covers them rather than each needing its own clause.
		if reservedPath(target.Path) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot target a reserved location"})
			return "", nil, false
		}
		return target.Path, &target.ID, true
	}

	if parentPath != "" {
		cleaned := absOf(sc, parentPath)
		if cleaned == sc.Home {
			return sc.Home, nil, true // the home itself
		}
		if reservedPath(cleaned) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot target a reserved location"})
			return "", nil, false
		}
		var target model.File
		if err := scoped(sc, h.db).First(&target, "path = ? AND is_dir = ? AND is_trashed = ?",
			cleaned, true, false).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target folder not found"})
			return "", nil, false
		}
		return target.Path, &target.ID, true
	}

	// Neither supplied — the destination is the caller's root.
	return sc.Home, nil, true
}

func (h *FileHandler) CopyFile(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var req struct {
		TargetParentID *uuid.UUID `json:"target_parent_id"`
		TargetPath     string     `json:"target_path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sc := reqScope(c)
	file, ok := h.lookupFile(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	targetParentPath, targetParentID, ok := h.resolveDestination(c, userID, req.TargetParentID, req.TargetPath)
	if !ok {
		return
	}

	if file.IsDir && (targetParentPath == file.Path || strings.HasPrefix(targetParentPath, file.Path+"/")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot copy a folder into itself"})
		return
	}

	// Copying is additive, so a sibling's file may be copied — the copy is
	// owned by the caller, not by whoever holds the original.
	newName, err := h.availableName(sc, targetParentPath, file.Name)
	if err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve destination name"})
		return
	}

	newPath := cleanPath(path.Join(targetParentPath, newName))

	// How much this copy will add, decided before any of it exists.
	//
	// The root's own bytes count: descendantsOf only matches rows strictly
	// below the root, so copying a plain file summed to zero and the charge was
	// skipped entirely. That made copy the one write path with no quota check
	// and no charge — a user could copy the same file a thousand times and
	// used_bytes never moved, so nothing downstream could see it either.
	subtreeBytes := int64(0)
	if !file.IsDir {
		subtreeBytes = file.Size
	}
	for _, d := range h.descendantsOf(file.UserID, file.Path) {
		if !d.IsDir {
			subtreeBytes += d.Size
		}
	}

	if rejectOverQuota(c, h.db, userID, subtreeBytes, inflightUploadBytes(h.db, userID)) {
		return
	}

	// Check the disk, not just the database.
	//
	// availableName only asks the files table, so it is blind to anything sitting
	// at the path with no row behind it — a leftover from a crashed copy, a file
	// dropped into the volume by hand, a partial upload. The copy would then be
	// written over it. Not a theoretical concern: the copy handler's own failure
	// paths used to leave exactly that state, since Copy truncated the destination
	// in place before the rows were inserted.
	if h.rejectPathOnDisk(c, newPath) {
		return
	}

	if err := h.storage.Mkdir(context.Background(), parentDir(newPath)); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "prepare destination")
		return
	}

	if err := h.storage.Copy(context.Background(), file.Path, newPath); err != nil {
		c.Error(err) //nolint:errcheck
		writeWriteError(c, err, "copy")
		return
	}

	// Create the copied root record.
	newFile := &model.File{
		UserID:      userID,
		ParentID:    targetParentID,
		Name:        newName,
		Path:        newPath,
		Size:        file.Size,
		MimeType:    file.MimeType,
		IsDir:       file.IsDir,
		Checksum:    file.Checksum,
		StorageType: file.StorageType,
		StoragePath: newPath,
	}
	// The root's own bytes were counted above, before anything was written.
	// Without that the loop below never ran for a plain file, the total stayed
	// at zero, and the guard skipped the charge.
	addedBytes := subtreeBytes

	// Read the subtree before opening the transaction, not inside it.
	//
	// descendantsOf goes through h.db. Inside a transaction that is a
	// *different* pooled connection than the tx handle, and SQLite serialises
	// writers: the read comes back SQLITE_BUSY, GORM's Find swallows the error,
	// and the loop iterates over nothing. The copy then answers 201 with the
	// root row only — a folder on disk holding the copied bytes, with no rows
	// for any of its contents. They are invisible in every listing, charged to
	// nobody, and cannot be deleted or moved because there is no id for them.
	//
	// Reading first is also simply correct: the source rows are not modified by
	// the copy, so there is nothing to see fresh.
	descendants := h.descendantsOf(file.UserID, file.Path)

	// One transaction, and a failure aborts it. The bytes are already on disk by
	// this point, so a clone that failed to insert left copied files with no row:
	// invisible in every listing, charged to nobody, and unremovable through the
	// API. `continue` swallowed that and still answered 201.
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Create(newFile).Error; e != nil {
			return e
		}
		for _, d := range descendants {
			clone := d
			clone.ID = uuid.UUID{}
			clone.UserID = userID // the copy belongs to whoever made it
			clone.Path = newPath + strings.TrimPrefix(d.Path, file.Path)
			clone.StoragePath = clone.Path
			clone.OriginalPath = ""
			clone.IsTrashed = false
			clone.TrashedAt = nil
			// parent_id is re-derived from paths by reparentCopiedSubtree below.
			clone.ParentID = nil
			if e := tx.Create(&clone).Error; e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		h.storage.Remove(context.Background(), newPath)
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create file records"})
		return
	}
	h.reparentCopiedSubtree(userID, newPath)

	if addedBytes > 0 {
		h.db.Model(&model.User{}).Where("id = ?", userID).
			UpdateColumn("used_bytes", gorm.Expr("used_bytes + ?", addedBytes))
	}

	logAudit(h.db, c, userID, "copy", "file", &newFile.ID, "Copied to: "+newPath)

	c.JSON(http.StatusCreated, toResponse(sc, newFile))
}

// reparentCopiedSubtree fixes parent_id across a freshly copied subtree by
// matching each cloned row's path to its parent path. Scoped to the copy's new
// root, since that is the only thing being rebuilt.
func (h *FileHandler) reparentCopiedSubtree(userID uuid.UUID, dstRoot string) {
	var clones []model.File
	whereSubtree(h.db.Where("user_id = ?", userID), "path", dstRoot).Find(&clones)

	byPath := make(map[string]uuid.UUID, len(clones)+1)
	var rootRow model.File
	if err := h.db.First(&rootRow, "user_id = ? AND path = ?", userID, dstRoot).Error; err == nil {
		byPath[dstRoot] = rootRow.ID
	}
	for i := range clones {
		byPath[clones[i].Path] = clones[i].ID
	}

	for i := range clones {
		cl := &clones[i]
		parentID, ok := byPath[parentDir(cl.Path)]
		if !ok {
			continue
		}
		h.db.Model(&model.File{}).Where("id = ?", cl.ID).Update("parent_id", parentID)
	}
}

// availableName returns name, or "name (2)" / "name (3)" ... when something with
// that name already exists in dir.
func (h *FileHandler) availableName(sc scope.Scope, dir, name string) (string, error) {
	exists := func(candidate string) (bool, error) {
		candidatePath := cleanPath(path.Join(dir, candidate))
		if candidatePath == "" {
			return false, nil
		}
		var count int64
		// Any row at this path counts: in a shared folder a sibling's file
		// must not be silently overwritten by a copy.
		err := scoped(sc, h.db.Model(&model.File{})).
			Where("path = ? AND is_trashed = ?", candidatePath, false).
			Count(&count).Error
		return count > 0, err
	}

	ok, err := exists(name)
	if err != nil || !ok {
		return name, err
	}

	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		ok, err := exists(candidate)
		if err != nil {
			return "", err
		}
		if !ok {
			return candidate, nil
		}
	}

	return name, nil
}
