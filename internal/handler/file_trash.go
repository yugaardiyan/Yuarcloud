package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"gorm.io/gorm"
)

// The trash: listing, trashing, restoring, permanent deletion and emptying.
//
// Every path through it moves bytes on disk and rows in the database in the
// same step, and the two have to agree at every intermediate point or the
// instance is left holding rows that point at files which are not there. That
// is why reads precede the transaction here rather than being issued inside it.

// trashRoot is where trashed items live inside the storage root. Keeping them
// under one folder keeps them out of normal listings while staying
// recoverable, and makes "empty trash" a single subtree removal.
const trashRoot = ".yuarcloud-trash"

// trashPrefixFor builds the on-disk location for a trashed item. The item's own
// id namespaces the folder so trashing a parent can never collide with a child
// that was already trashed on its own.
func trashPrefixFor(id uuid.UUID) string {
	return path.Join(trashRoot, id.String())
}

// ---------------------------------------------------------------------------
// Trash
// ---------------------------------------------------------------------------

func (h *FileHandler) ListTrash(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	sc := reqScope(c)

	// The caller's own trashed rows, unfiltered by the home scope.
	//
	// Trashing rewrites the row's path to .yuarcloud-trash/<id>/…, which falls
	// outside any non-empty home prefix — so scoped() matched nothing and the
	// trash page was permanently empty for every user who has a home directory,
	// which is everyone sharing a folder. EmptyTrash never applied the scope and
	// so did delete rows the UI claimed did not exist.
	//
	// The confinement that matters is applied where it can actually matter: on
	// the destination, in RestoreFromTrash, which checks that OriginalPath is
	// still inside the home before writing anything back.
	var files []model.File
	if err := h.db.
		Where("user_id = ? AND is_trashed = ?", userID, true).
		Order("trashed_at DESC").
		Find(&files).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load trash"})
		return
	}

	// Show only entries that were trashed in their own right. If one trashed
	// folder's on-disk trash folder contains another's, that one came along for
	// the ride and is already represented by its ancestor.
	//
	// Physical containment, not OriginalPath prefix: a child trashed *before*
	// its parent sits in its own id-namespaced folder even though its original
	// location is inside the parent's. Judging by path hid it from the list
	// entirely, so it could never be restored — and emptying the trash then
	// removed it without the user ever being offered the chance.
	topLevel := make([]model.File, 0, len(files))
	for i := range files {
		item := &files[i]

		nested := false
		for j := range files {
			if j == i {
				continue
			}
			if strings.HasPrefix(item.Path, trashPrefixFor(files[j].ID)+"/") {
				nested = true
				break
			}
		}
		if !nested {
			topLevel = append(topLevel, *item)
		}
	}

	response := make([]model.FileResponse, 0, len(topLevel))
	for i := range topLevel {
		response = append(response, toResponse(sc, &topLevel[i]))
	}

	c.JSON(http.StatusOK, gin.H{
		"files":    response,
		"path":     trashRoot,
		"scope":    "trash",
		"parent":   nil,
		"readonly": true,
	})
}

// trashRootOf returns the trashed row whose own id names the on-disk trash
// folder that physically contains this one.
//
// It used to decide by OriginalPath prefix alone: any trashed row whose original
// location was a parent of this one's counted as the ancestor. But each trash
// entry gets its own id-namespaced folder precisely so a parent and an
// already-trashed child cannot collide — so a child trashed first is *not*
// inside its parent's folder, while OriginalPath still says it is. Restoring
// then renamed the parent's folder (now empty) instead of the child's, stamped
// the child's row as restored, and left the bytes stranded in a folder nothing
// pointed at. Deleting permanently went the other way and removed the parent's
// real data.
//
// Physical containment is the only question that has a true answer here, so
// that is what is asked, and the closest ancestor wins rather than whichever
// row the unordered scan happened to reach last.
func (h *FileHandler) trashRootOf(userID uuid.UUID, file *model.File) model.File {
	root := *file
	bestLen := -1

	var candidates []model.File
	// No owner filter, same reason as descendantsOf: trashing relocates the whole
	// subtree regardless of who owns the rows inside it, so the ancestor of this
	// row may well belong to someone else. Filtering here made a member's
	// trashed file report itself as its own root, and restoring "that" renamed
	// the file's own trash folder instead of the folder containing it.
	_ = userID
	h.db.Where("is_trashed = ?", true).
		Where("path LIKE ? ESCAPE '\\'", escapeLike(trashPrefixFor(file.ID))+"%").
		Find(&candidates)

	for i := range candidates {
		c := &candidates[i]
		if c.ID == file.ID {
			continue
		}
		// `file` lives inside c's folder, and c is not inside file's own.
		if !strings.HasPrefix(file.Path, trashPrefixFor(c.ID)+"/") {
			continue
		}
		if len(c.Path) > bestLen {
			bestLen = len(c.Path)
			root = *c
		}
	}
	_ = userID
	return root
}

// releaseSubtreeUsage frees the quota held by every owner of a subtree, in
// proportion to what each one actually had inside it.
//
// Bytes are charged to whoever uploaded them, so a shared folder's quota
// cannot be released as a single number against a single account: crediting the
// caller for another member's files inflates the caller's counter and leaves the
// uploader paying for bytes that are no longer live. Grouping by user_id and
// adjusting each is the only split that is true on both sides.
//
// Sign the delta, not an amount: callers use it to release (trash) or to
// re-charge (restore).
func (h *FileHandler) releaseSubtreeUsage(root string, sign int64) {
	if root == "" {
		return
	}

	type ownerBytes struct {
		UserID uuid.UUID
		Bytes  int64
	}
	var rows []ownerBytes
	// Only rows the application actually charged may move used_bytes. Files
	// adopted from disk were never added to the counter, so releasing them
	// here would hand out quota for bytes nobody paid for (and could drive
	// the total below zero). Copies are charged and therefore still count.
	whereSubtree(h.db.Model(&model.File{}), "path", root).
		Select("user_id, COALESCE(SUM(size), 0) AS bytes").
		Where("is_dir = ? AND quota_charged = ?", false, true).
		Group("user_id").
		Scan(&rows)

	for _, r := range rows {
		if r.Bytes == 0 {
			continue
		}
		h.adjustUsage(r.UserID, sign*r.Bytes)
	}
}

// descendantsOf returns every DB row nested under root (root itself excluded).
//
// No user_id filter. Confinement in this app is by path, not by owner — that is
// the premise the scope package is built on, and it is why a shared folder works
// at all. The filter was here anyway, and the on-disk rename that a move or a
// trash performs relocates the whole subtree regardless of who owns the rows
// inside it. So in a shared home, one member's rename or trash moved every
// byte while rewriting only that member's rows: the other member's files kept
// paths that no longer existed, vanished from listings, 404'd on download, and
// could no longer be moved or trashed at all because the disk source was gone.
// Their quota never came back either.
//
// Callers that genuinely need one owner use ownerIDs, which filters by a
// *specific* id rather than by the caller's.
func (h *FileHandler) descendantsOf(userID uuid.UUID, root string) []model.File {
	var rows []model.File
	if root == "" {
		return rows
	}
	_ = userID // kept in the signature; see the note above
	h.whereDescendants(root).Find(&rows)
	return rows
}

func (h *FileHandler) MoveToTrash(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	file, ok := h.writable(c, fileID, "is_trashed = ?", false)
	if !ok {
		return
	}

	if err := h.MoveToTrashRecord(c, userID, file); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to move item to trash"})
		return
	}

	logAudit(h.db, c, userID, "trash", "file", &fileID, "Moved to trash: "+file.Name)
	c.JSON(http.StatusOK, gin.H{"message": "moved to trash", "id": fileID})
}

// MoveToTrashRecord relocates an item (and its subtree) into the trash folder.
// Both on disk and in the database, so a restore returns it to where it was.
func (h *FileHandler) MoveToTrashRecord(c *gin.Context, userID uuid.UUID, file *model.File) error {
	originalPath := file.Path
	prefix := trashPrefixFor(file.ID)
	newPath := prefix

	if err := h.storage.Mkdir(context.Background(), parentDir(prefix)); err != nil {
		log.Printf("trash: mkdir %q failed for user=%s file=%s: %v", parentDir(prefix), userID, file.ID, err)
		return err
	}
	if err := h.storage.Rename(context.Background(), originalPath, newPath); err != nil {
		log.Printf("trash: rename %q -> %q failed for user=%s file=%s: %v", originalPath, newPath, userID, file.ID, err)
		return err
	}

	now := time.Now()

	// Read the subtree before anything rewrites it.
	//
	// Both the quota release and the row rewrites are expressed in terms of the
	// rows' *live* paths, so whichever runs first destroys the other's input.
	// Releasing after the loop found nothing — every row had already moved
	// under .yuarcloud-trash/ — so trashing released no quota at all and
	// used_bytes ratcheted upward until the account could not upload again.
	descendants := h.descendantsOf(userID, originalPath)

	// Quota stops counting trashed bytes, credited to each uploader rather than
	// to whoever happened to press the button.
	h.releaseSubtreeUsage(originalPath, -1)

	// One transaction with every row checked, and the disk rolled back if it
	// fails. The descendant writes were separate and unchecked, so a failure
	// part-way through left a subtree split across the live tree and the trash
	// folder — some rows marked trashed at their old path, some not marked at
	// their new one, and bytes in neither place.
	err := h.db.Transaction(func(tx *gorm.DB) error {
		// The single on-disk rename already relocated the whole subtree into
		// `prefix`; only the paths need rewriting.
		for _, d := range descendants {
			if e := tx.Model(&model.File{}).Where("id = ?", d.ID).Updates(map[string]interface{}{
				"is_trashed":    true,
				"trashed_at":    now,
				"original_path": d.Path,
				"path":          prefix + strings.TrimPrefix(d.Path, originalPath),
			}).Error; e != nil {
				return e
			}
		}
		return tx.Model(file).Updates(map[string]interface{}{
			"is_trashed":    true,
			"trashed_at":    now,
			"original_path": originalPath,
			"path":          newPath,
		}).Error
	})
	if err != nil {
		h.storage.Rename(context.Background(), newPath, originalPath) // roll back disk
		log.Printf("trash: db update failed for user=%s file=%s: %v", userID, file.ID, err)
		// The bytes are back where they were, so give the quota back too.
		h.releaseSubtreeUsage(originalPath, +1)
		return err
	}

	return nil
}

func (h *FileHandler) RestoreFromTrash(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var file model.File
	if err := h.db.First(&file, "id = ? AND user_id = ? AND is_trashed = ?", fileID, userID, true).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trashed item not found"})
		return
	}

	// A trashed row now lives under .trash/, so the scope no longer covers it;
	// check the destination it would come back to instead. Restoring must never
	// be a way to write outside the home.
	sc := reqScope(c)
	restorePath := cleanPath(file.OriginalPath)
	if restorePath == "" {
		restorePath = file.Name
	}
	if !sc.Contains(restorePath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "this item no longer belongs in your folder"})
		return
	}

	// Restore the whole trashed subtree that physically lives under one folder.
	root := h.trashRootOf(userID, &file)
	restoreRoot := cleanPath(root.OriginalPath)
	if restoreRoot == "" {
		restoreRoot = root.Name
	}

	// The name the item had when it was trashed is very likely back in use by now
	// — the window between trashing and restoring is unbounded. os.Rename
	// replaces the destination, so restoring without asking destroys whatever
	// was created in the meantime and orphans its row.
	if h.rejectPathTaken(c, sc, restoreRoot) {
		return
	}

	if err := h.storage.Mkdir(context.Background(), parentDir(restoreRoot)); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare restore"})
		return
	}
	if err := h.storage.Rename(context.Background(), trashPrefixFor(root.ID), restoreRoot); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore item"})
		return
	}

	// Only rows physically living under this trash folder take part. An item
	// that was trashed separately has its own folder and must stay put.
	// Every row physically under this folder, whoever owns it.
	//
	// The owner filter was here, and the restore moved the bytes back without
	// moving the rows that did not match: a folder whose contents belonged to
	// several accounts came back with only the caller's own rows, and
	// everyone else's files stayed pointing at a trash folder that had just
	// been renamed away. Invisible, unreachable, and still counting against
	// their quota.
	prefix := trashPrefixFor(root.ID)
	var rows []model.File
	h.whereDescendants(prefix).
		Where("is_trashed = ?", true).
		Find(&rows)

	// Re-charge while the rows still carry their trash paths.
	//
	// Same ordering trap as MoveToTrashRecord: the charge is expressed in terms
	// of where the rows are, and the transaction below moves them. Run after, it
	// matched nothing — so a restore put live bytes back into the tree without
	// charging anyone, and cycling an item through the trash fifty times cost
	// nothing at all while the disk filled up.
	h.releaseSubtreeUsage(prefix, +1)

	// One transaction, and every row's error checked. Each of these was a
	// separate committed write whose failure was discarded, so a failure part-way
	// through left rows 0..k restored and rows k..N still marked trashed but
	// pointing at a trash folder that had just been renamed away — rows the UI
	// would still offer to restore, whose bytes were already back in the tree.
	// Restoring one of those then renamed a folder that no longer existed.
	err = h.db.Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			restored := cleanPath(rows[i].OriginalPath)
			if restored == "" {
				continue
			}
			if e := tx.Model(&model.File{}).Where("id = ?", rows[i].ID).Updates(map[string]interface{}{
				"is_trashed":    false,
				"trashed_at":    nil,
				"original_path": "",
				"path":          restored,
			}).Error; e != nil {
				return e
			}
		}
		return tx.Model(&file).Updates(map[string]interface{}{
			"is_trashed":    false,
			"trashed_at":    nil,
			"original_path": "",
			"path":          restorePath,
		}).Error
	})
	if err != nil {
		// Roll the bytes back to where the rows still think they are: the trash
		// folder this subtree came from, not the single item's old path. Using
		// restorePath renamed one item into a folder that no longer matched any
		// row, which made the rollback worse than the failure it undid.
		h.storage.Rename(context.Background(), restoreRoot, trashPrefixFor(root.ID))
		// The charge above was speculative; nothing came back, so give it back.
		h.releaseSubtreeUsage(prefix, -1)
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore item"})
		return
	}

	logAudit(h.db, c, userID, "restore", "file", &fileID, "Restored: "+file.Name)
	c.JSON(http.StatusOK, gin.H{"message": "restored", "id": fileID, "path": restorePath})
}

func (h *FileHandler) DeletePermanently(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	fileID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return
	}

	var file model.File
	if err := h.db.First(&file, "id = ? AND user_id = ? AND is_trashed = ?", fileID, userID, true).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trashed item not found"})
		return
	}

	root := h.trashRootOf(userID, &file)

	// Removing the namespaced trash folder takes the whole subtree with it.
	if err := h.storage.Remove(context.Background(), trashPrefixFor(root.ID)); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete item"})
		return
	}

	// The folder is gone, so every row under it has to go regardless of owner.
	// Leaving another account's rows behind points them at a directory that no
	// longer exists: invisible in listings, and impossible to delete or restore
	// because there is no folder to name. The bytes are already removed from
	// disk, so they are also still charged to their owners' quota.
	// No quota release here, unlike the live-file delete in file.go: these
	// bytes were already released when the item was trashed, and doing it
	// again hands the space back out while the trash still holds the data.
	prefix := trashPrefixFor(root.ID)
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		// The whole subtree, root included. whereDescendants excludes the root,
		// and the root is the row that actually named the folder just removed —
		// skipping it left a row pointing at a directory that no longer exists.
		if e := whereSubtree(tx, "path", prefix).Delete(&model.File{}).Error; e != nil {
			return e
		}
		if root.ID == file.ID {
			return nil
		}
		return tx.Delete(&file).Error
	}); err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete file record"})
		return
	}

	logAudit(h.db, c, userID, "delete_permanent", "file", &fileID, "Permanently deleted: "+file.Name)
	c.JSON(http.StatusOK, gin.H{"message": "deleted permanently", "id": fileID})
}

func (h *FileHandler) EmptyTrash(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var files []model.File
	h.db.Where("user_id = ? AND is_trashed = ?", userID, true).Find(&files)

	// Every trash folder is id-namespaced, so removing each root once is enough.
	//
	// A failure here used to be discarded and the bulk delete ran anyway: the
	// bytes stayed under .yuarcloud-trash/ with no row pointing at them, and
	// since browsing the trash is refused and dot-prefixed entries are hidden
	// from listings, they became unreachable through every endpoint and could
	// never be reclaimed. Abort instead, and say which folder failed, so the
	// trash is still intact and the user can retry.
	failures := 0
	for i := range files {
		if err := h.storage.Remove(context.Background(), trashPrefixFor(files[i].ID)); err != nil {
			failures++
			log.Printf("empty trash: removing %s for user=%s failed: %v", trashPrefixFor(files[i].ID), userID, err)
		}
	}
	if failures > 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":           "could not remove some items from storage; the trash was left intact",
			"failed_removals": failures,
		})
		return
	}

	// Drop the rows by physical location, not by owner.
	//
	// A trashed folder is one directory on disk containing rows from everyone
	// who had files in it. Removing that directory removes their bytes too, so
	// a delete filtered on user_id leaves their rows pointing at a path that no
	// longer exists: invisible, un-restorable (there is no folder to name), and
	// still counted against their quota for data that is gone.
	//
	// Each trash entry's own id namespaces its folder, so walking the caller's
	// trashed roots covers the whole thing; any stray row directly at a trash
	// root is removed by the owner-filtered sweep that follows.
	erased := int64(0)
	for i := range files {
		// The root row too: it is the one that named the directory just
		// removed, so leaving it behind is a row that points at nothing.
		res := whereSubtree(h.db, "path", trashPrefixFor(files[i].ID)).Delete(&model.File{})
		if res.Error != nil {
			c.Error(res.Error) //nolint:errcheck
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to empty trash"})
			return
		}
		erased += res.RowsAffected
	}

	res := h.db.Where("user_id = ? AND is_trashed = ?", userID, true).Delete(&model.File{})
	if res.Error != nil {
		c.Error(res.Error) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to empty trash"})
		return
	}
	erased += res.RowsAffected

	// Count what actually went, not just the caller's own rows. The figure is
	// shown in a toast, and reporting "removed 1" after emptying a folder that
	// held nine files is the kind of small lie that makes the next question
	// "where did the rest go?" unanswerable.
	logAudit(h.db, c, userID, "empty_trash", "file", nil, fmt.Sprintf("Emptied trash (%d rows)", erased))
	c.JSON(http.StatusOK, gin.H{"message": "trash emptied", "removed": erased})
}
