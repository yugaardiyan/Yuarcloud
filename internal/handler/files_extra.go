package handler

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yuarcloud/yuarcloud/internal/middleware"
	"github.com/yuarcloud/yuarcloud/internal/model"
)

// trashRoot is where trashed items live inside the storage root. Keeping them
// under one folder keeps them out of normal listings while staying
// recoverable, and makes "empty trash" a single subtree removal.
const trashRoot = ".yuarcloud-trash"

// escapeLike neutralises LIKE metacharacters so user input matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s)
}

// subtreeMatch builds the WHERE fragment and arguments matching `root` itself
// plus everything below it, in one call.
//
// The ESCAPE clause is not decoration. escapeLike inserts backslashes precisely
// so a name containing `_`, `%` or `\` is compared literally, and SQLite's LIKE
// has *no default escape character* — without the clause the backslash is
// matched literally as well and the pattern matches nothing at all. Seven call
// sites had that omission, and the effect was invisible: a folder called
// `my_files` returned zero descendants from every move, rename and delete, so
// its files kept paths that no longer existed and vanished from listings while
// still occupying quota. Postgres happens to default LIKE's escape to
// backslash, which is why this only ever bit on the shipped database.
//
// Both arms are required. `path = ?` catches the folder row itself; the LIKE
// catches the contents. Either alone is a different bug.
// subtreeMatch matches a row and everything nested below it.
//
// The root itself is included. That is right for "delete this subtree" and
// "relabel this subtree", and wrong for "what is inside this" — so descendantsOf
// excludes it explicitly rather than inheriting the choice from here. It used to
// get that exclusion for free from a user_id filter: the root is owned by
// whoever created it, and the filter happened to drop it along with any
// descendant belonging to someone else. Dropping the filter to make shared
// folders work exposed the assumption underneath, and a copy of a folder then
// tried to clone the folder onto itself.
func subtreeMatch(column, root string) (string, []interface{}) {
	return fmt.Sprintf("(%s = ? OR %s LIKE ? ESCAPE '\\')", column, column),
		[]interface{}{root, escapeLike(root) + "/%"}
}

// whereDescendants applies the same match as whereSubtree without the root row.
//
// Order is deepest-first, so a caller rewriting paths can process a child before
// its parent without either order mattering for correctness.
func (h *FileHandler) whereDescendants(root string) *gorm.DB {
	if root == "" {
		return h.db
	}
	return h.db.Where("path LIKE ? ESCAPE '\\'", escapeLike(root)+"/%").
		Order("LENGTH(path) DESC")
}

// whereSubtree applies subtreeMatch to a query.
//
// This exists as its own function rather than letting callers write
// `q.Where(subtreeMatch(...))`: Go expands a two-value call into two
// arguments, so the slice lands in the variadic `args ...interface{}` as a
// single element and gets bound to the first placeholder, leaving the second
// unbound. The query still compiles and matches nothing — the same silent
// failure this whole change is about.
func whereSubtree(q *gorm.DB, column, root string) *gorm.DB {
	clause, args := subtreeMatch(column, root)
	return q.Where(clause, args...)
}

// trashPrefixFor builds the on-disk location for a trashed item. The item's own
// id namespaces the folder so trashing a parent can never collide with a child
// that was already trashed on its own.
func trashPrefixFor(id uuid.UUID) string {
	return path.Join(trashRoot, id.String())
}

func parentDir(p string) string {
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
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
	whereSubtree(h.db.Model(&model.File{}), "path", root).
		Select("user_id, COALESCE(SUM(size), 0) AS bytes").
		Where("is_dir = ?", false).
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

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func (h *FileHandler) Search(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		c.JSON(http.StatusOK, gin.H{"files": []model.FileResponse{}, "query": query, "count": 0})
		return
	}

	limit := 100
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}

	pattern := "%" + escapeLike(query) + "%"

	var files []model.File
	sc := reqScope(c)
	if err := scoped(sc, h.db).
		Where("is_trashed = ? AND name LIKE ? ESCAPE '\\'", false, pattern).
		Order("is_dir DESC, name ASC").
		Limit(limit).
		Find(&files).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})
		return
	}

	response := make([]model.FileResponse, 0, len(files))
	for i := range files {
		response = append(response, toResponse(sc, &files[i]))
	}

	c.JSON(http.StatusOK, gin.H{
		"files":     response,
		"query":     query,
		"limit":     limit,
		"count":     len(response),
		"truncated": len(response) == limit,
	})
}

// ---------------------------------------------------------------------------
// Folder lists (move / copy destination picker)
// ---------------------------------------------------------------------------

func (h *FileHandler) FolderTree(c *gin.Context) {
	sc := reqScope(c)

	var folders []model.File
	if err := scoped(sc, h.db).
		Where("is_dir = ? AND is_trashed = ?", true, false).
		Order("path ASC").
		Find(&folders).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load folders"})
		return
	}

	// Nodes are held by pointer so appending to a parent's Children slice is
	// visible through the map. "path ASC" guarantees parents come first.
	nodes := make(map[string]*model.FolderNode, len(folders))
	order := make([]string, 0, len(folders))
	for i := range folders {
		f := &folders[i]
		nodes[f.Path] = &model.FolderNode{
			ID:       f.ID,
			Name:     f.Name,
			Path:     relOf(sc, f.Path),
			ParentID: f.ParentID,
		}
		order = append(order, f.Path)
	}

	var roots []*model.FolderNode
	for _, p := range order {
		node := nodes[p]
		if parent, ok := nodes[parentDir(p)]; ok && parent != node {
			parent.Children = append(parent.Children, *node)
			continue
		}
		// No parent (true root, or orphaned) — expose at the top level.
		roots = append(roots, node)
	}

	// Children were appended as value copies taken before their own children
	// were filled in, so refresh each subtree from the map.
	var refresh func(dst *model.FolderNode, src *model.FolderNode)
	refresh = func(dst, src *model.FolderNode) {
		if dst == nil || src == nil {
			return
		}
		dst.Children = nil
		for i := range src.Children {
			childPath := src.Children[i].Path
			child, ok := nodes[childPath]
			if !ok {
				continue
			}
			dst.Children = append(dst.Children, *child)
			refresh(&dst.Children[len(dst.Children)-1], child)
		}
	}

	out := make([]model.FolderNode, 0, len(roots))
	for _, root := range roots {
		node := *root
		refresh(&node, root)
		out = append(out, node)
	}

	c.JSON(http.StatusOK, gin.H{"folders": out})
}

func (h *FileHandler) FlatFolderList(c *gin.Context) {
	sc := reqScope(c)

	var folders []model.File
	scoped(sc, h.db).Where("is_dir = ? AND is_trashed = ?", true, false).
		Order("path ASC").Find(&folders)

	// The browser draws its listings from storage, not from here, so a database
	// row without a directory behind it is a folder the user cannot open, cannot
	// see and cannot account for — yet it would still be offered as somebody's
	// home directory, and picked from this list the assignment would fail later
	// with "home folder not found". Rows outlive their directories when a user
	// is deleted with a folder still on disk, or when a create is interrupted
	// between the two writes, so the two sources drift apart on their own.
	//
	// Checking each candidate against storage keeps the two in agreement: this
	// list can only name a directory that My Cloud would actually show.
	response := make([]model.FolderNode, 0, len(folders))
	for i := range folders {
		exists, err := h.storage.Exists(context.Background(), folders[i].Path)
		if err != nil || !exists {
			continue
		}
		response = append(response, model.FolderNode{
			ID:       folders[i].ID,
			Name:     folders[i].Name,
			Path:     relOf(sc, folders[i].Path),
			ParentID: folders[i].ParentID,
		})
	}

	c.JSON(http.StatusOK, gin.H{"folders": response})
}

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

// ---------------------------------------------------------------------------
// Shared with me
// ---------------------------------------------------------------------------

// ListShared returns shares the caller owns, annotated with the file so the
// client can render a "Shared" view without a second round-trip.
func (h *FileHandler) ListShared(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)

	var shares []model.ShareLink
	if err := h.db.
		Preload("File").
		Where("user_id = ? AND is_active = ? AND file_id IN (?)",
			userID, true,
			h.db.Model(&model.File{}).Select("id").Where("user_id = ? AND is_trashed = ?", userID, false)).
		Order("created_at DESC").
		Find(&shares).Error; err != nil {
		c.Error(err) //nolint:errcheck
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load shared items"})
		return
	}

	type entry struct {
		model.FileResponse
		Token   string     `json:"token"`
		URL     string     `json:"url"`
		Views   int        `json:"views"`
		Expires *time.Time `json:"expires_at"`
		HasPass bool       `json:"has_password"`
		ShareID uuid.UUID  `json:"share_id"`
	}

	out := make([]entry, 0, len(shares))
	for i := range shares {
		s := &shares[i]
		out = append(out, entry{
			FileResponse: model.ToFileResponse(&s.File),
			Token:        s.Token,
			URL:          "/yc/" + s.Token,
			Views:        s.ViewCount,
			Expires:      s.ExpiresAt,
			HasPass:      s.PasswordHash != "",
			ShareID:      s.ID,
		})
	}

	c.JSON(http.StatusOK, gin.H{"shares": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// Storage usage
// ---------------------------------------------------------------------------

func (h *FileHandler) StorageStats(c *gin.Context) {
	userID, _ := middleware.GetUserID(c)
	sc := reqScope(c)

	// Counts describe what this user can actually reach inside the home, so
	// the numbers match the file list rather than the whole disk.
	var stats model.StorageStats
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", false, false).
		Count(&stats.TotalFiles)
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", true, false).
		Count(&stats.TotalFolders)
	scoped(sc, h.db.Model(&model.File{})).
		Where("is_dir = ? AND is_trashed = ?", false, false).
		Select("COALESCE(SUM(size), 0)").Scan(&stats.TotalSize)

	var user model.User
	if err := h.db.First(&user, "id = ?", userID).Error; err == nil {
		stats.TotalQuota = user.QuotaBytes
	}

	c.JSON(http.StatusOK, stats)
}

// unusedImportGuard keeps gorm imported for the future; referenced here so the
// compiler does not drop it when the file is edited.
var _ = gorm.ErrRecordNotFound
