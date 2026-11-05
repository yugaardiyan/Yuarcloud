import React, { createContext, useContext, useState, useCallback, useEffect, useLayoutEffect, useRef } from 'react'
import { useFiles, driveHref } from '../context/FileContext'
import { useNavigate } from 'react-router-dom'
import ShareModal from './ShareModal'
import InfoModal from './InfoModal'
import PromptModal from './PromptModal'
import ConfirmModal from './ConfirmModal'
import { claimKeyboard, keyboardIsOwned, isTypingTarget } from '../utils/keyboardOwner'
import { fireAndForget } from '../utils/fireAndForget'
import { stemLength } from '../utils/fileType'
import { useMediaQuery } from '../hooks/useMediaQuery'


const ContextMenuContext = createContext(null)

export function useContextMenu() {
  const ctx = useContext(ContextMenuContext)
  if (!ctx) throw new Error('useContextMenu must be used inside <ContextMenuProvider>')
  return ctx
}

export default function ContextMenuProvider({ children }) {
  const files = useFiles()
  const navigate = useNavigate()
  const {
    rawItems, view, selected, clipboard,
    downloadItem, downloadSelectedAsZip, renameItem, removeItems, restoreItems,
    cutItems, copyToClipboard, pasteInto,
    createFolder, uploadFiles, selectAll, selectMany, toggleSelectMode, selectMode, exitSelectMode, clearSelection,
  } = files

  // The mobile layout and touch screens have no reliable right-click on blank
  // space, so the page-level menu that carries "Select" is hard to reach there
  // — and often there is no blank space at all, only tiles. The per-item ⋯
  // menu is the reliable entry point, so it gains the same action in both
  // cases.
  const mobile = useMediaQuery('(hover: none), (max-width: 560px)')

  // `kind` is 'file' (right-clicked an item) or 'empty' (right-clicked
  // blank space). An empty menu carries no ids and offers file-level
  // actions such as New folder / Upload instead of item actions.
  const [menu, setMenu] = useState({ x: 0, y: 0, visible: false, kind: 'file', ids: [] })
  const [shareTarget, setShareTarget] = useState(null)
  // `isDir` travels with the target so the rename dialog can pre-select a file's
  // stem while leaving a folder's name whole. See `stemLength`.
  const [renameTarget, setRenameTarget] = useState(null) // { id, name, isDir }
  const [newFolderOpen, setNewFolderOpen] = useState(false)
  const [infoTarget, setInfoTarget] = useState(null)
  // confirm: { title, message, confirmLabel, danger, onConfirm }
  const [confirm, setConfirm] = useState(null)

  const closeMenu = useCallback(() => setMenu((p) => ({ ...p, visible: false })), [])

  // An open menu owns the keyboard too — otherwise Drive's own Delete shortcut
  // fires while the menu is on screen and moves the highlighted entries to the
  // trash behind it. Keyed on visibility rather than mounted, because the menu
  // element is rendered persistently and only shown.
  useEffect(() => {
    if (!menu.visible) return undefined
    const release = claimKeyboard('context-menu')
    return release
  }, [menu.visible])

  const menuRef = useRef(null)
  const fileInputRef = useRef(null)
  const folderInputRef = useRef(null)
  /* When the menu was last opened. A touch long press opens it without an
     event to stop, so the click iOS synthesises on finger lift arrives right
     after and would close it again; clicks this soon after opening are the
     gesture that opened the menu, not a dismissal. */
  const openedAt = useRef(0)

  /*
   * Flip/shift the menu so it always sits fully inside the viewport. This runs
   * after the menu is in the DOM and measures the real height — guessing it
   * clipped the tail whenever the list was taller than the guess.
   */
  useLayoutEffect(() => {
    if (!menu.visible || !menuRef.current) return

    const el = menuRef.current
    // offsetWidth/Height, not getBoundingClientRect: the menu animates in with
    // a scale transform, and the rect would report the scaled-down size and
    // then under-correct.
    const width = el.offsetWidth
    const height = el.offsetHeight
    const gap = 8

    let x = menu.x
    let y = menu.y

    // Horizontal: nudge left rather than flipping, menus read fine anywhere.
    if (x + width > window.innerWidth - gap) x = Math.max(gap, window.innerWidth - width - gap)
    if (x < gap) x = gap

    // Vertical: prefer below the cursor, but flip above when it will not fit.
    if (y + height > window.innerHeight - gap) {
      const flipped = Math.max(gap, y - height)
      y = flipped + height <= window.innerHeight - gap ? flipped : Math.max(gap, window.innerHeight - height - gap)
    }

    if (x !== menu.x || y !== menu.y) setMenu((p) => ({ ...p, x, y }))
    // `menuItems` is declared further down, so it cannot be read here (the
    // dependency array is evaluated during render, before that const exists).
    // Opening the menu always changes `visible`, which re-runs this.
  }, [menu.visible, menu.x, menu.y])

  useEffect(() => {
    const onClick = () => {
      // A long press opens the menu, and the finger lifting can still produce
      // a click. Treating that as "clicked elsewhere" would shut the menu
      // before it could be read.
      if (Date.now() - openedAt.current < 400) return
      closeMenu()
    }
    const onKey = (e) => { if (e.key === 'Escape') closeMenu() }
    const onReflow = () => closeMenu()

    document.addEventListener('click', onClick)
    document.addEventListener('contextmenu', onClick)
    document.addEventListener('keydown', onKey)
    window.addEventListener('resize', onReflow)
    window.addEventListener('scroll', onReflow, true)

    return () => {
      document.removeEventListener('click', onClick)
      document.removeEventListener('contextmenu', onClick)
      document.removeEventListener('keydown', onKey)
      window.removeEventListener('resize', onReflow)
      window.removeEventListener('scroll', onReflow, true)
    }
  }, [closeMenu])

  const openMenu = useCallback((e, fileId, multi = false) => {
    e.preventDefault()
    e.stopPropagation()

    const ids = multi && selected.size > 1 && selected.has(fileId)
      ? Array.from(selected)
      : [fileId]

    const items = rawItems.filter((f) => ids.includes(f.id))
    if (!items.length) return

    // Keyboard activation carries 0,0 rather than a pointer position. Anchor
    // those openers to their own control so the menu does not jump to the
    // top-left corner of the viewport. Pointer events already have coordinates.
    const rect = e.currentTarget?.getBoundingClientRect?.()
    const x = e.clientX || (rect ? rect.right - 8 : 8)
    const y = e.clientY || (rect ? rect.bottom : 8)

    // The menu repositions itself against the real viewport size once mounted.
    openedAt.current = Date.now()
    setMenu({ x, y, visible: true, kind: 'file', ids })
  }, [selected, rawItems])

  // Right-click on blank space: a menu of page-level actions.
  const openEmptyMenu = useCallback((e) => {
    e.preventDefault()
    e.stopPropagation()
    openedAt.current = Date.now()
    setMenu({ x: e.clientX, y: e.clientY, visible: true, kind: 'empty', ids: [] })
  }, [])

  const targetItems = rawItems.filter((f) => menu.ids.includes(f.id))
  const first = targetItems[0]
  const isMulti = targetItems.length > 1
  const inTrash = view.scope === 'trash'

  
  const run = async (action) => {
    const ids = menu.ids
    closeMenu()

    // Blank-space actions target no file, so they run before the id guard.
    if (menu.kind === 'empty') {
      switch (action) {
        case 'new-folder':
          setNewFolderOpen(true)
          break
        case 'upload-file':
          fileInputRef.current?.click()
          break
        case 'upload-folder':
          folderInputRef.current?.click()
          break
        case 'select-all':
          selectAll()
          break
        case 'select':
          // Tapping "Select" from the context menu starts pick-one-at-a-time
          // mode. A plain tap on any file then toggles it into the selection
          // instead of opening it; the file tiles themselves change hands
          // between "open" and "toggle" based on this flag.
          toggleSelectMode()
          break
        case 'cancel-select':
          // Exit select mode and clear selection
          exitSelectMode()
          clearSelection()
          break
        case 'download':
          // Blank-space "Download" targets the current selection, not a file:
          // when several are picked they go out as one zip.
          if (selected.size > 1) downloadSelectedAsZip(selected)
          else if (selected.size === 1) {
            const item = rawItems.find((f) => selected.has(f.id))
            if (item) downloadItem(item)
          }
          break
        case 'share': {
          // Same batch the capsule offers: one link for everything marked.
          const picked = rawItems.filter((f) => selected.has(f.id))
          if (picked.length > 1) {
            setShareTarget({
              ids: picked.map((item) => item.id),
              name: `${picked.length} items`,
              isDir: false,
            })
          }
          break
        }
        case 'paste':
          // Same destination the existing file-menu paste resolves to.
          pasteInto(view.scope === 'drive' ? view.folderId : null, view.scope === 'drive' ? view.path : '')
          break
        default:
          break
      }
      return
    }

    if (!ids.length) return

    switch (action) {
      case 'open':
        if (first?.is_dir) navigate(driveHref(first.path))
        else window.dispatchEvent(new CustomEvent('yuar:preview', { detail: { id: first.id } }))
        break

      case 'select':
        // Mark the item the ⋯ belongs to and switch to pick-one-at-a-time
        // mode. The trigger already clears any previous selection, so this
        // starts a fresh batch. `selectMany` keeps the anchor right for a
        // subsequent shift-click on a desktop with a touch screen.
        if (!selectMode) toggleSelectMode()
        selectMany(ids)
        break

      case 'select-all':
        // Same mode as "Select", but the whole listing starts marked. Without
        // entering the mode, a tap on a tile afterwards would replace the set
        // instead of toggling one entry out of it — not what "select all" then
        // "adjust" looks like on a phone.
        if (!selectMode) toggleSelectMode()
        selectAll()
        break

      case 'download':
        // Single item downloads itself; multiple items go out as one zip.
        if (targetItems.length > 1) downloadSelectedAsZip(ids)
        else downloadItem(targetItems[0])
        break

      case 'copy':
        copyToClipboard(ids)
        break

      case 'cut':
        cutItems(ids)
        break

      case 'paste-into':
        // The menu only offers this for a single folder, so `first` is it.
        pasteInto(first.id, first.path)
        break

      case 'rename': {
        if (isMulti || !first) return
        setRenameTarget({ id: ids[0], name: first.name, isDir: !!first.is_dir })
        break
      }

      case 'share':
        // Share covers the whole marked set. One link opens a page listing
        // every item, so a multi-selection is sent as ids rather than being
        // reduced to the item that was right-clicked.
        if (first) {
          setShareTarget({
            ids: targetItems.map((item) => item.id),
            name: isMulti ? `${targetItems.length} items` : first.name,
            isDir: !isMulti && !!first.is_dir,
          })
        }
        break

      case 'trash': {
        const n = ids.length
        setConfirm({
          simple: true,
          title: `Move ${n > 1 ? 'items' : 'item'} to trash?`,
          confirmLabel: 'Move to trash',
          danger: true,
          onConfirm: () => {
            fireAndForget(removeItems(ids))
            setConfirm(null)
          },
        })
        break
      }

      case 'delete-permanent': {
        const n = ids.length
        setConfirm({
          simple: true,
          title: `Delete ${n > 1 ? 'items' : 'item'} forever?`,
          confirmLabel: 'Delete',
          danger: true,
          onConfirm: () => {
            fireAndForget(removeItems(ids))
            setConfirm(null)
          },
        })
        break
      }

      case 'restore':
        fireAndForget(restoreItems(ids))
        break

      case 'info':
        if (first) setInfoTarget(first)
        break

      default:
        break
    }
  }

  // Clipboard keyboard shortcuts.
  useEffect(() => {
    const onKey = (e) => {
      if (isTypingTarget(e.target)) return
      // Copy, cut and paste are destructive to whatever they are aimed at, so
      // they must not fire at a dialog or a viewer. Before this guard a
      // Ctrl/Cmd-C with a preview open put the viewer's own selection on the
      // clipboard, and Cmd-V pasted files into the folder behind it.
      if (keyboardIsOwned()) return
      const mod = e.metaKey || e.ctrlKey
      if (!mod) return

      const key = e.key.toLowerCase()
      if (key === 'c' && selected.size) { e.preventDefault(); copyToClipboard(Array.from(selected)) }
      if (key === 'x' && selected.size) { e.preventDefault(); cutItems(Array.from(selected)) }
      if (key === 'v' && clipboard.action) {
        // Trash and search are listings, not locations. Resolving their null
        // folder to the root pasted the clipboard into My Cloud, a place the
        // user was not looking at.
        if (view.scope !== 'drive') return
        e.preventDefault()
        pasteInto(view.folderId, view.path)
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [selected, clipboard.action, copyToClipboard, cutItems, pasteInto, view, files])

  // Drive raises this event for the F2 shortcut so the rename dialog lives in
  // one place instead of being duplicated per view.
  useEffect(() => {
    const onRename = (e) => {
      if (e.detail?.id) setRenameTarget({ id: e.detail.id, name: e.detail.name, isDir: !!e.detail.isDir })
    }
    window.addEventListener('yuar:rename', onRename)
    return () => window.removeEventListener('yuar:rename', onRename)
  }, [])

  // The header capsule's Share button raises this. The modal lives here, where
  // every other file dialog does, so the capsule does not carry a second copy
  // of the share flow.
  useEffect(() => {
    const onShare = (e) => {
      const wanted = Array.isArray(e.detail?.ids) ? e.detail.ids : []
      const items = rawItems.filter((f) => wanted.includes(f.id))
      if (!items.length) return
      setShareTarget({
        ids: items.map((item) => item.id),
        name: items.length > 1 ? `${items.length} items` : items[0].name,
        isDir: items.length === 1 && !!items[0].is_dir,
      })
    }
    window.addEventListener('yuar:share', onShare)
    return () => window.removeEventListener('yuar:share', onShare)
  }, [rawItems])

  const handlePickFiles = (e) => {
    const picked = Array.from(e.target.files || [])
    if (picked.length) fireAndForget(uploadFiles(picked))
    e.target.value = ''
  }

  const handlePickFolder = (e) => {
    const picked = Array.from(e.target.files || [])
    if (picked.length) fireAndForget(uploadFiles(picked))
    e.target.value = ''
  }

  // The blank-space menu: page-level actions. Folder creation and upload only
  // make sense inside the real drive tree, so they are hidden elsewhere.
  // Build empty-space menu dynamically.
  // In select mode with files selected: show "Cancel" and "Download as zip"
  // In select mode without selection: show "Cancel" and "Select all"
  // Normal mode: show "Select" and "Select all"
  const buildEmptyMenuItems = () => {
    const items = []
    if (view.scope === 'drive' && !selectMode) {
      if (clipboard.action) items.push({ id: 'paste', label: 'Paste here' })
      items.push({ id: 'new-folder', label: 'New folder' })
      items.push({ id: 'upload-file', label: 'Upload file' })
      items.push({ id: 'upload-folder', label: 'Upload folder' })
      items.push({ type: 'divider' })
    }
    if (selectMode) {
      items.push({ id: 'cancel-select', label: 'Cancel' })
      items.push({ id: 'select-all', label: 'Select all' })
      if (selected.size > 1) {
        items.push({ type: 'divider' })
        items.push({ id: 'download', label: 'Download' })
        items.push({ id: 'share', label: 'Share' })
      }
    } else {
      items.push({ id: 'select', label: 'Select' })
      items.push({ id: 'select-all', label: 'Select all' })
    }
    return items
  }

  const emptyMenuItems = buildEmptyMenuItems()

  const trashMenuItems = [
    { id: 'info', label: 'Get info' },
    { type: 'divider' },
    { id: 'restore', label: 'Restore' },
    { id: 'delete-permanent', label: 'Delete forever', danger: true },
  ]

  // Build file menu dynamically based on selection state and select mode.
  // Items that cannot run on the current selection are marked `disabled` here
  // and then removed by compactMenuItems — the menu never shows a greyed-out
  // button, only actions that actually do something.
  const buildFileMenuItems = () => {
    const items = []
    // Open only makes sense for one item (or a set of folders); with a mixed
    // selection there is no single target to open.
    items.push({
      id: 'open',
      label: 'Open',
      disabled: isMulti && targetItems.some((f) => !f.is_dir),
    })
    // Mobile layout or touch screen only: see `mobile` above. Desktop already
    // has "Select" in the blank-space context menu, and right-clicking there
    // is natural.
    if (mobile) {
      items.push({ id: 'select', label: 'Select' })
      items.push({ id: 'select-all', label: 'Select all' })
    }
    items.push({ id: 'download', label: 'Download' })
    items.push({ type: 'divider' })
    // Copy/Cut — always active, shown in both modes.
    items.push({ id: 'copy', label: 'Copy' })
    items.push({ id: 'cut', label: 'Cut' })
    // Paste into folder: a single folder only.
    if (clipboard.action && !isMulti && first?.is_dir && view.scope === 'drive') {
      items.push({ id: 'paste-into', label: 'Paste into folder' })
    }
    // Rename acts on exactly one item. This is why the per-row ⋯ menu still
    // offers it in select mode: that menu always targets its own item, not the
    // batch.
    if (!isMulti) {
      items.push({ type: 'divider' })
      items.push({ id: 'rename', label: 'Rename' })
      items.push({ type: 'divider' })
      items.push({ id: 'share', label: 'Share' })
    } else {
      // Share is meaningful for a batch: one generated link opens a list of
      // everything that was marked, the way a shared folder would.
      items.push({ type: 'divider' })
      items.push({ id: 'share', label: 'Share' })
    }
    items.push({ type: 'divider' })
    items.push({ id: 'trash', label: 'Move to trash', danger: true })
    items.push({ type: 'divider' })
    items.push({ id: 'info', label: 'Get info', disabled: isMulti })
    return items
  }

  // Drop any entry that cannot run and collapse the dividers it leaves behind
  // (leading, trailing, or doubled). Applied to every menu, so the rule is the
  // same in select mode and in ordinary multi-selection.
  const compactMenuItems = (items) => {
    const kept = []
    for (const item of items) {
      if (item.type === 'divider') {
        if (!kept.length || kept[kept.length - 1].type === 'divider') continue
        kept.push(item)
        continue
      }
      if (item.disabled) continue
      kept.push(item)
    }
    while (kept.length && kept[kept.length - 1].type === 'divider') kept.pop()
    return kept
  }

  const fileMenuItems = buildFileMenuItems()

  const menuItems = compactMenuItems(
    menu.kind === 'empty'
      ? emptyMenuItems
      : inTrash
        ? trashMenuItems
        : fileMenuItems,
  )

  const value = { openMenu, openEmptyMenu, closeMenu }

  return (
    <ContextMenuContext.Provider value={value}>
      {children}

      {menu.visible && (
        <div
          className="context-menu"
          ref={menuRef}
          style={{ left: menu.x, top: menu.y }}
          onClick={(e) => e.stopPropagation()}
          onContextMenu={(e) => e.preventDefault()}
          role="menu"
          aria-label={menu.kind === 'empty' ? 'Page actions' : 'File actions'}
        >
          {menuItems.map((item, i) =>
            item.type === 'divider' ? (
              <div key={`d${i}`} className="context-menu-divider" role="separator" />
            ) : (
              <button
                key={item.id}
                className={`context-menu-item ${item.danger ? 'danger' : ''}`}
                onClick={() => run(item.id)}
                disabled={item.disabled}
                role="menuitem"
              >
                {item.label}
              </button>
            )
          )}
        </div>
      )}

      {infoTarget && (
        <InfoModal file={infoTarget} onClose={() => setInfoTarget(null)} />
      )}

      {shareTarget && (
        <ShareModal
          fileId={shareTarget.ids[0]}
          fileIds={shareTarget.ids}
          fileName={shareTarget.name}
          isDir={shareTarget.isDir}
          onClose={() => setShareTarget(null)}
        />
      )}

      {renameTarget && (
        <PromptModal
          title="Rename"
          label="New name"
          initialValue={renameTarget.name}
          confirmLabel="Rename"
          selectEnd={stemLength(renameTarget.name, renameTarget.isDir)}
          onConfirm={(name) => renameItem(renameTarget.id, name)}
          onCancel={() => setRenameTarget(null)}
        />
      )}

      {newFolderOpen && (
        <PromptModal
          title="New folder"
          label="Folder name"
          placeholder="Folder name"
          confirmLabel="Create"
          onConfirm={(name) => createFolder(name)}
          onCancel={() => setNewFolderOpen(false)}
        />
      )}

      {confirm && (
        <ConfirmModal
          simple={confirm.simple}
          title={confirm.title}
          message={confirm.message}
          confirmLabel={confirm.confirmLabel}
          danger={confirm.danger}
          onConfirm={confirm.onConfirm}
          onCancel={() => setConfirm(null)}
        />
      )}

      <input ref={fileInputRef} type="file" multiple style={{ display: 'none' }} onChange={handlePickFiles} />
      <input
        ref={folderInputRef}
        type="file"
        webkitdirectory=""
        directory=""
        multiple
        style={{ display: 'none' }}
        onChange={handlePickFolder}
      />
    </ContextMenuContext.Provider>
  )
}

