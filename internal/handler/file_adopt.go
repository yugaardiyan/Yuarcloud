package handler

// Adopting files that are already on disk.
//
// The rest of the application treats the files table as the index: a listing
// intersects the directory with rows, shares resolve rows, and an id is how a
// client names something. A volume that was populated outside Yuarcloud has
// bytes but no rows, so it looks empty. This file creates the missing rows
// lazily, one directory level at a time, as the user browses. That keeps the
// first open of a large library from walking the whole drive, while still
// making everything reachable once it is on screen.
//
// Adopted rows are not charged to used_bytes. The files predate the account's
// quota, and charging them retroactively could leave an operator unable to
// upload anything after simply opening a folder. QuotaCharged=false is what
// makes trashing or deleting those rows a no-op for the counter; copies are
// new bytes and are charged normally.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuarcloud/yuarcloud/internal/model"
	"github.com/yuarcloud/yuarcloud/internal/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// adoptDirectoryEntries inserts a row for every direct child of dir that does
// not already have a live row. Symlinks are skipped: following one can leave
// the storage root, and an importer must not turn that into a browsable id.
//
// The insert is deliberately conflict-tolerant. Two requests can list the same
// folder at the same time, and the partial unique index on files(path) is the
// authority on who wins; ON CONFLICT DO NOTHING makes the loser harmless.
func adoptDirectoryEntries(db *gorm.DB, userID uuid.UUID, dir string, parent *model.File, entries []*storage.FileInfo) error {
	if db == nil || len(entries) == 0 {
		return nil
	}

	dir = cleanPath(dir)
	pattern, nested := childPathPatterns(dir)

	var existing []model.File
	query := db.Model(&model.File{}).Where("is_trashed = ?", false)
	if pattern != "" {
		query = query.Where("path LIKE ? ESCAPE '\\'", pattern)
		if nested != "" {
			query = query.Where("path NOT LIKE ? ESCAPE '\\'", nested)
		}
	} else {
		// The storage root has no prefix to anchor a LIKE. Without this the
		// check loads the whole files table just to find its top-level rows.
		query = query.Where("path NOT LIKE ?", "%/%")
	}
	if err := query.Find(&existing).Error; err != nil {
		return err
	}

	known := make(map[string]*model.File, len(existing))
	for i := range existing {
		known[existing[i].Path] = &existing[i]
	}

	var parentID *uuid.UUID
	if parent != nil {
		id := parent.ID
		parentID = &id
	}

	now := time.Now()
	seen := make(map[string]struct{}, len(entries))
	rows := make([]model.File, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.IsSymlink {
			continue
		}
		name := entry.Name
		if name == "" {
			continue
		}
		abs := cleanPath(path.Join(dir, name))
		if abs == "" || reservedPath(abs) {
			continue
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}

		size := entry.Size
		if entry.IsDir {
			size = 0
		}
		mimeType := entry.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		mod := entry.ModTime
		if mod.IsZero() {
			mod = now
		}

		if row, ok := known[abs]; ok {
			// An adopted row is a cache of the disk, which FileBrowser also
			// treats as live. If the file was edited or replaced outside the
			// app, update the metadata the download and preview handlers read.
			// Rows the app charged are left alone: changing their size would
			// silently desynchronise used_bytes.
			if !row.CountsAgainstQuota() &&
				(row.Size != size || row.MimeType != mimeType || row.IsDir != entry.IsDir) {
				updates := map[string]interface{}{
					"size":      size,
					"mime_type": mimeType,
					"is_dir":    entry.IsDir,
				}
				if !entry.ModTime.IsZero() {
					updates["updated_at"] = entry.ModTime
				}
				_ = db.Model(&model.File{}).Where("id = ?", row.ID).Updates(updates).Error
			}
			continue
		}

		uncharged := false
		rows = append(rows, model.File{
			UserID:       userID,
			ParentID:     parentID,
			Name:         name,
			Path:         abs,
			Size:         size,
			QuotaCharged: &uncharged,
			MimeType:     mimeType,
			IsDir:        entry.IsDir,
			StorageType:  "local",
			StoragePath:  abs,
			CreatedAt:    mod,
			UpdatedAt:    mod,
		})
	}

	if len(rows) == 0 {
		return nil
	}

	return db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 200).Error
}

// adoptTree walks root and gives every file and directory below it a row.
//
// Copy is the one operation that must know every byte before it writes: it
// charges the destination up front, clones the source rows, and then copies the
// disk subtree. If the source still has unimported descendants, a lazy listing
// would make the copy free for those bytes and leave the destination without
// rows for them. Copy is already O(size of the tree), so the extra walk is not
// a new order of cost.
func adoptTree(ctx context.Context, db *gorm.DB, store storage.StorageBackend, userID uuid.UUID, root string, rootRow *model.File) error {
	entries, err := store.List(ctx, root)
	if err != nil {
		return err
	}
	if err := adoptDirectoryEntries(db, userID, root, rootRow, entries); err != nil {
		return err
	}

	pattern, nested := childPathPatterns(root)
	var dirs []model.File
	query := db.Where("is_dir = ? AND is_trashed = ?", true, false)
	if pattern != "" {
		query = query.Where("path LIKE ? ESCAPE '\\'", pattern)
		if nested != "" {
			query = query.Where("path NOT LIKE ? ESCAPE '\\'", nested)
		}
	} else {
		query = query.Where("path NOT LIKE ?", "%/%")
	}
	if err := query.Find(&dirs).Error; err != nil {
		return err
	}
	for i := range dirs {
		if err := adoptTree(ctx, db, store, userID, dirs[i].Path, &dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// adoptPathFrom makes sure every component of target below base has a row.
//
// ListFiles normally resolves the current folder from parent_id, and every
// folder reached that way was adopted when its parent was listed. A direct
// URL, a refreshed share page, or a deep link can arrive without that chain,
// so this walks the path from base and fills in what is missing. baseParent is
// the row for base when one exists; for a user's home root it is nil, matching
// the way home listings use NULL parent_id.
//
// Only paths under base are accepted. That, plus Stat's use of Lstat, keeps a
// crafted path or a symlink from importing data outside the caller's home.
func adoptPathFrom(db *gorm.DB, store storage.StorageBackend, userID uuid.UUID, base string, baseParent *model.File, target string) (*model.File, error) {
	if db == nil || store == nil {
		return nil, nil
	}

	base = cleanPath(base)
	target = cleanPath(target)
	if target == "" {
		return nil, nil
	}
	if reservedPath(target) {
		return nil, storage.ErrNotFound
	}

	rel := target
	if base != "" {
		if target == base {
			return baseParent, nil
		}
		if !strings.HasPrefix(target, base+"/") {
			return nil, storage.ErrNotFound
		}
		rel = strings.TrimPrefix(target, base+"/")
	}
	if rel == "" {
		return baseParent, nil
	}

	parts := strings.Split(rel, "/")
	parent := baseParent
	current := base

	for i, part := range parts {
		current = cleanPath(path.Join(current, part))
		last := i == len(parts)-1
		if current == "" {
			return nil, storage.ErrNotFound
		}

		var row model.File
		err := db.Where("path = ? AND is_trashed = ?", current, false).First(&row).Error
		if err == nil {
			if !last && !row.IsDir {
				return nil, storage.ErrNotFound
			}
			parent = &row
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}

		info, err := store.Stat(context.Background(), current)
		if err != nil {
			return nil, err
		}
		if info.IsSymlink {
			return nil, storage.ErrNotFound
		}
		if !last && !info.IsDir {
			return nil, storage.ErrNotFound
		}

		var parentID *uuid.UUID
		if parent != nil {
			id := parent.ID
			parentID = &id
		}
		size := info.Size
		if info.IsDir {
			size = 0
		}
		mimeType := info.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		mod := info.ModTime
		if mod.IsZero() {
			mod = time.Now()
		}

		uncharged := false
		row = model.File{
			UserID:       userID,
			ParentID:     parentID,
			Name:         info.Name,
			Path:         current,
			Size:         size,
			QuotaCharged: &uncharged,
			MimeType:     mimeType,
			IsDir:        info.IsDir,
			StorageType:  "local",
			StoragePath:  current,
			CreatedAt:    mod,
			UpdatedAt:    mod,
		}
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return nil, err
		}

		// A concurrent request may have inserted the row between the lookup
		// and the insert. Read it back so the next component gets the real id
		// rather than the one BeforeCreate minted for a discarded insert.
		var saved model.File
		if err := db.Where("path = ? AND is_trashed = ?", current, false).First(&saved).Error; err != nil {
			return nil, fmt.Errorf("read adopted path %q: %w", current, err)
		}
		row = saved
		parent = &row
	}

	return parent, nil
}
