import React, { useEffect, useLayoutEffect, useCallback, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import FileGrid from '../components/FileGrid'
import FileList from '../components/FileList'
import ConfirmModal from '../components/ConfirmModal'
import PreviewModal from '../components/PreviewModal'
import { previewGroupOf } from '../utils/fileType'
import { hasInternalDrag } from '../utils/dragItems'
import { keyboardIsOwned, isTypingTarget } from '../utils/keyboardOwner'
import { fireAndForget } from '../utils/fireAndForget'
import { useFiles, driveHref } from '../context/FileContext'
import { useMarqueeSelect } from '../hooks/useMarqueeSelect'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { useAuth } from '../context/AuthContext'
import { useContextMenu } from '../components/ContextMenuProvider'
import { Folder, RefreshCw, ArrowLeft, ArrowRight, ChevronRight } from 'lucide-react'

function Drive() {
  const navigate = useNavigate()
  const params = useParams()
  const { user } = useAuth()
  const files = useFiles()
  const { openEmptyMenu } = useContextMenu()
  const phone = useMediaQuery('(max-width: 560px)')

  const {
    items, view, loading, error, viewMode, setViewMode,
    refresh, removeItems, renameItem, downloadItem, clearSelection, selected
  } = files

  const folderInputRef = useRef(null)
  const contentRef = useRef(null)
  const breadcrumbRef = useRef(null)

  // Rubber-band selection over the listing (grid and list). It owns what a
  // press on the background means, so the click-to-clear handlers that used to
  // live in the grid and the list are gone.
  useMarqueeSelect(contentRef)

  // Clicking outside the listing still drops the selection, as it always did.
  // One owner for the whole pane: the two components each decided this for
  // themselves, and neither could tell a Ctrl-drag from a plain press.
  useEffect(() => {
    const onDown = (e) => {
      if (contentRef.current && !contentRef.current.contains(e.target)) clearSelection()
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [clearSelection])

  // ---------------------------------------------------------------- routing
  //
  // The URL is the source of truth, and FileContext turns each route into the
  // load it stands for:
  //   /cloud            -> root
  //   /cloud/Documents  -> the "Documents" folder
  //   /trash            -> the trash
  //   /search?q=…       -> search results
  // This page therefore only has to navigate; it never decides what is loaded.

  const urlPath = params['*'] || ''

  // On a phone the breadcrumb is a horizontal strip (see the 560px status-bar
  // block in main.css). Keep the newest segment — the folder you are actually
  // in — in view if an unusually long name makes the strip overflow. On wider
  // screens the full path is meant to stay left-aligned, so reset any scroll
  // left over from a rotation.
  useLayoutEffect(() => {
    const el = breadcrumbRef.current
    if (!el) return
    el.scrollLeft = phone ? el.scrollWidth : 0
  }, [phone, view.path, view.scope])

  // ---------------------------------------------------------------- history
  //
  // A folder stack with an index, mirroring a file manager's
  // Back/Forward. `navLock` marks a change that came from the
  // buttons themselves so the URL effect does not re-push it.
  const histRef = useRef({ stack: [''], index: 0 })
  const [, bumpHist] = useState(0)
  const navLock = useRef(false)

  useEffect(() => {
    if (view.scope !== 'drive') return
    const path = urlPath || ''
    const h = histRef.current
    if (navLock.current) {
      navLock.current = false
      return
    }
    if (path !== h.stack[h.index]) {
      h.stack = h.stack.slice(0, h.index + 1)
      h.stack.push(path)
      h.index = h.stack.length - 1
      bumpHist((n) => n + 1)
    }
  }, [urlPath, view.scope])

  const goBack = useCallback(() => {
    const h = histRef.current
    if (h.index <= 0) return
    h.index -= 1
    navLock.current = true
    bumpHist((n) => n + 1)
    navigate(driveHref(h.stack[h.index]))
  }, [navigate])

  const goForward = useCallback(() => {
    const h = histRef.current
    if (h.index >= h.stack.length - 1) return
    h.index += 1
    navLock.current = true
    bumpHist((n) => n + 1)
    navigate(driveHref(h.stack[h.index]))
  }, [navigate])

  const canGoBack = histRef.current.index > 0
  const canGoForward = histRef.current.index < histRef.current.stack.length - 1

  // ---------------------------------------------------------------- entering folders

  const enterFolder = useCallback((item) => {
    if (!item?.is_dir) return
    // Update the URL, which drives the effect above and loads the folder. The
      // you move between folders — dropping it here would silently turn it off the
    // moment you opened something.
    navigate(driveHref(item.path))
  }, [navigate])

  // ---------------------------------------------------------------- previewing
  //
  // Clicking a file used to download it. Now it opens the viewer, and only
  // the viewer offers a download.
  //
  // Paging is scoped to a *series*: the arrow keys step photo → photo, film →
  // film, track → track, and within documents whether each is a PDF, a sheet
  // or plain text. One pair of arrows across a mixed folder meant most presses
  // swapped the content entirely.
  //
  // The open file is held by id rather than by index, because the series it
  // belongs to is derived from it — an index would be invalidated the moment
  // the list behind it got filtered.
  const [previewId, setPreviewId] = useState(null)

  const previewItem = previewId ? items.find((f) => f.id === previewId && !f.is_dir) : null

  const previewGroup = useMemo(() => previewGroupOf(previewItem), [previewItem])

  const previewFiles = useMemo(() => {
    // No series means no paging: an archive gets a panel of its own.
    if (!previewGroup) return previewItem ? [previewItem] : []
    return items.filter((f) => !f.is_dir && previewGroupOf(f) === previewGroup)
  }, [items, previewGroup, previewItem])

  const previewIndex = previewId ? previewFiles.findIndex((f) => f.id === previewId) : -1

  const openPreview = useCallback((item) => {
    if (!item || item.is_dir) return
    setPreviewId(item.id)
  }, [])

  const closePreview = useCallback(() => setPreviewId(null), [])

  // The context menu's "Open" lives in a provider above this page and cannot
  // call openPreview itself, so it asks for it the same way F2 asks for the
  // rename dialog. Opening a file meant downloading it, while Enter and a
  // double click both previewed — the same word did two different things.
  useEffect(() => {
    const onPreview = (e) => {
      const item = items.find((f) => f.id === e.detail?.id)
      if (item && !item.is_dir) openPreview(item)
    }
    window.addEventListener('yuar:preview', onPreview)
    return () => window.removeEventListener('yuar:preview', onPreview)
  }, [items, openPreview])

  const stepPreview = useCallback((delta) => {
    const next = previewFiles[previewIndex + delta]
    // Stop at the ends rather than wrapping: bouncing back to the first file
    // after the last reads as a glitch, not as navigation.
    if (next) setPreviewId(next.id)
  }, [previewFiles, previewIndex])

  // Breadcrumb navigation: clicking a path segment jumps to it.
  const goToBreadcrumb = useCallback((path) => {
    navigate(driveHref(path))
  }, [navigate])

  // ---------------------------------------------------------------- delete confirm
  //
  // `deleteConfirm` is { permanent, ids }. A single dialog covers both
  // "move to trash" (in My Cloud) and "delete forever" (in Trash),
  // plus the Delete/Backspace keyboard shortcut.
  const [deleteConfirm, setDeleteConfirm] = useState(null)

  const askDelete = useCallback((ids, permanent) => {
    if (!ids.length) return
    setDeleteConfirm({ ids: Array.from(ids), permanent })
  }, [])

  // ---------------------------------------------------------------- keyboard

  useEffect(() => {
    const onKey = (e) => {
      if (isTypingTarget(e.target)) return
      // Anything open owns the keyboard: a dialog, a menu, the preview viewer.
      //
      // This used to query a hardcoded list of overlay class names. The viewer
      // is its own class at a higher z-index than .modal-overlay, so it was
      // never matched — selecting two files, opening a preview of one and
      // pressing Delete offered "move 2 items to trash?" on top of the viewer.
      // Surfaces now register themselves instead, so a new overlay cannot be
      // missed by forgetting to add a class name to a list somewhere else.
      if (keyboardIsOwned()) return

      const ids = Array.from(selected)

      if (view.scope === 'trash') {
        if ((e.key === 'Delete' || e.key === 'Backspace') && ids.length) {
          e.preventDefault()
          askDelete(ids, true)
        }
        return
      }

      if ((e.key === 'Delete' || e.key === 'Backspace') && ids.length) {
        e.preventDefault()
        askDelete(ids, false)
        return
      }

      if (e.key === 'Enter' && ids.length === 1) {
        const item = items.find((f) => f.id === ids[0])
        if (item?.is_dir) enterFolder(item)
        else openPreview(item)
        return
      }

      if (e.key === 'F2' && ids.length === 1) {
        e.preventDefault()
        const item = items.find((f) => f.id === ids[0])
        if (item) {
          // The context menu owns the rename dialog; ask it to open.
          // `is_dir` rides along so the dialog knows whether to pre-select a
          // stem or the whole name.
          window.dispatchEvent(new CustomEvent('yuar:rename', {
            detail: { id: item.id, name: item.name, is_dir: item.is_dir },
          }))
        }
        return
      }

      if (e.key === 'Escape') clearSelection()

      if (e.metaKey || e.ctrlKey) {
        if (e.key === '1') { e.preventDefault(); setViewMode('grid') }
        if (e.key === '2') { e.preventDefault(); setViewMode('list') }
      }
    }

    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [selected, items, view.scope, removeItems, renameItem, openPreview, enterFolder, clearSelection, setViewMode, askDelete])

  // ---------------------------------------------------------------- drag & drop

  // A view that is not a location cannot receive a drop. Trash is a
  // listings over folders that live somewhere else, so an OS file dropped on
  // either had nowhere to mean — it went to the root, because that is what the
  // null folder id resolved to. Refusing the drop is honest; accepting it and
  // filing the file somewhere the user did not choose is how files get lost.
  const acceptsDrop = view.scope === 'drive'

  // The drop tint is written straight to style.background so a dragover does
  // not re-render the listing sixty times a second. That is also what makes it
  // stick: dragleave does not fire when a drag is cancelled with Escape or
  // pulled back out of the window, so the pane kept its blue background for
  // good with nothing left to clear it. dragend fires for every way a drag can
  // end, including the ones no element under the cursor will ever hear about.
  const clearDropHighlight = useCallback(() => {
    if (contentRef.current) contentRef.current.style.background = ''
  }, [])

  useEffect(() => {
    window.addEventListener('dragend', clearDropHighlight)
    window.addEventListener('drop', clearDropHighlight)
    window.addEventListener('blur', clearDropHighlight)
    return () => {
      window.removeEventListener('dragend', clearDropHighlight)
      window.removeEventListener('drop', clearDropHighlight)
      window.removeEventListener('blur', clearDropHighlight)
    }
  }, [clearDropHighlight])

  const handleDragOver = (e) => {
    // An in-app move drag carries our own payload and no files. Letting this
    // run would tint the whole page as if files were coming in, and set
    // dropEffect to copy — wrong for a move, and the folder tiles under the
    // pointer would never get to claim it.
    if (hasInternalDrag(e.dataTransfer)) return
    if (!acceptsDrop) return
    e.preventDefault()
    e.stopPropagation()
    e.dataTransfer.dropEffect = 'copy'
    if (contentRef.current) contentRef.current.style.background = 'var(--bg-selected)'
  }

  const handleDragLeave = (e) => {
    if (hasInternalDrag(e.dataTransfer)) return
    if (contentRef.current && !contentRef.current.contains(e.relatedTarget)) {
      clearDropHighlight()
    }
  }

  const handleDrop = (e) => {
    // An in-app move that landed on bare space has nowhere to go.
    if (hasInternalDrag(e.dataTransfer)) return
    if (!acceptsDrop) return
    e.preventDefault()
    e.stopPropagation()
    clearDropHighlight()
    const dropped = Array.from(e.dataTransfer.files || [])
    if (dropped.length) fireAndForget(files.uploadFiles(dropped))
  }

  // Right-click on blank space opens the page menu. Items open their
  // own menu (openMenu stops propagation), so this only fires for
  // gaps, the empty state, or the container itself.
  const handleContentContextMenu = (e) => {
    if (e.target.closest('[data-file-id]')) return
    openEmptyMenu(e)
  }

  // ---------------------------------------------------------------- status text

  /*
   * There is no loading spinner by design — it only ever flashed for a
   * few milliseconds and read as a glitch. While a switch is in flight the pane
   * just dims (see the content wrapper below).
   *
   * `loadedOnce` distinguishes "nothing fetched yet" from "genuinely empty".
   * Without it a cold load would briefly render the "folder is empty" state
   * before the real files arrived.
   */
  const sawLoading = useRef(false)
  const loadedOnce = useRef(false)
  useEffect(() => {
    if (loading) {
      sawLoading.current = true
      return
    }
    if (sawLoading.current) {
      loadedOnce.current = true
      sawLoading.current = false
    }
  }, [loading])

  const awaitingFirstLoad = loading && !loadedOnce.current

  const scopeLabel = {
    drive: 'My Cloud',
    trash: 'Trash',
    search: `Results for "${view.query || ''}"`,
  }[view.scope]

  // Breadcrumb segments for the current folder path.
  const driveSegments = view.path ? view.path.split('/').filter(Boolean) : []

  // A phone cannot hold the whole path beside the arrows. Drop the middle
  // rather than letting every segment shrink to a letter: "… › Proyek 2026 ›
  // Foto Liburan" is a path you can read, and the leading ellipsis still goes
  // home. One-level paths keep "My Cloud", because they fit.
  const compactCrumbs = phone && driveSegments.length > 1
  const firstCrumbIndex = compactCrumbs ? driveSegments.length - 2 : 0
  const shownSegments = driveSegments.slice(firstCrumbIndex)

  return (
    <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0, height: '100%' }}>
      {/* Navigation lives in the status bar at the bottom, so there is no scope
          bar here — it held nothing but Back/Forward and the breadcrumb, and an
          empty band across the top of the drive would be worse than none. */}

      {/* Content. The rounded box is its own element rather than the scroll
          container itself: a scroll container paints its own overflow square,
          so rounded corners on it get scrolled content bleeding past them. */}
      <div className="drive-shell">
        <div
          ref={contentRef}
          className={`drive-content${viewMode === 'list' ? ' is-list' : ''}`}
          style={{
            transition: 'opacity 140ms ease',
            // Dim rather than blank out, so a quick switch reads as a refresh
            // instead of the whole list vanishing for a few milliseconds.
            opacity: loading ? 0.55 : 1,
          }}
          onDragOver={handleDragOver}
          onDragLeave={handleDragLeave}
          onDrop={handleDrop}
          onContextMenu={handleContentContextMenu}
        >
        {awaitingFirstLoad ? null : error ? (
            <div className="empty-state">
              <Folder className="empty-state-icon" size={64} />
              <h3 className="empty-state-title">Could not load this folder</h3>
              <p className="empty-state-text">{error}</p>
              <button className="btn btn-secondary" onClick={refresh} style={{ marginTop: 16 }}>
                <RefreshCw size={16} /> Try again
              </button>
            </div>
          ) : viewMode === 'grid' ? (
            <FileGrid onOpen={enterFolder} onDownload={downloadItem} onFile={openPreview} />
          ) : (
            <FileList onOpen={enterFolder} onDownload={downloadItem} onFile={openPreview} />
          )}
        </div>
      </div>

      {/* File viewer. Rendered outside the toolbar/content stack so it covers
          the whole viewport; it portals to the body on its own. */}
      {previewItem && (
        <PreviewModal
          item={previewItem}
          items={previewFiles}
          onNavigate={stepPreview}
          onClose={closePreview}
          onDownload={downloadItem}
        />
      )}

      {/* Status bar. Navigation moved down here: arrows and breadcrumb on the left,
          the counts and the storage figure pushed to the right, sitting beside
          each other rather than at opposite ends. */}
      <div className="status-bar">
        <div className="status-bar-nav">
          {view.scope === 'drive' && (
            <>
              {/* Both arrows are built from the same numbers on purpose. They
                  sit at the foot of the pane in much smaller type than
                  anything up in the capsule, and at lucide's default 2px
                  stroke they read as hairlines next to the bold wordmark. */}
              <button
                className="btn btn-ghost status-nav-btn"
                onClick={goBack}
                disabled={!canGoBack}
                title="Back"
                aria-label="Back"
              >
                <ArrowLeft size={17} strokeWidth={2.5} />
              </button>
              <button
                className="btn btn-ghost status-nav-btn"
                onClick={goForward}
                disabled={!canGoForward}
                title="Forward"
                aria-label="Forward"
              >
                <ArrowRight size={17} strokeWidth={2.5} />
              </button>
            </>
          )}

          {view.scope === 'drive' ? (
            <nav ref={breadcrumbRef} className="breadcrumb" aria-label="Breadcrumb">
              <button
                className={`breadcrumb-item${compactCrumbs ? ' breadcrumb-item-compact' : ''}`}
                onClick={() => goToBreadcrumb('')}
                title={compactCrumbs ? 'My Cloud' : undefined}
                aria-label={compactCrumbs ? 'My Cloud' : undefined}
              >
                {compactCrumbs ? '…' : 'My Cloud'}
              </button>
              {shownSegments.map((seg, i) => {
                const segPath = driveSegments.slice(0, firstCrumbIndex + i + 1).join('/')
                return (
                  <React.Fragment key={segPath}>
                    <span className="breadcrumb-sep" aria-hidden="true">
                      <ChevronRight size={12} />
                    </span>
                    <button className="breadcrumb-item" onClick={() => goToBreadcrumb(segPath)}>
                      {seg}
                    </button>
                  </React.Fragment>
                )
              })}
            </nav>
          ) : (
            <span className="truncate status-bar-scope">{scopeLabel}</span>
          )}
        </div>

        <div className="status-bar-meta">
          <span>
            {items.length} item{items.length === 1 ? '' : 's'}
            {selected.size > 0 && ` · ${selected.size} selected`}
          </span>
          <span>
            {user?.quota_bytes > 0 && (
              <span>
                {formatBytes(user.used_bytes || 0)} / {formatBytes(user.quota_bytes)} used
              </span>
            )}
          </span>
        </div>
      </div>

      <input ref={folderInputRef} type="file" style={{ display: 'none' }} />

      {/* Delete / move-to-trash confirmation (menu, button, and keyboard). */}
      {deleteConfirm && (
        <ConfirmModal
          simple
          title={deleteConfirm.permanent ? 'Delete item forever?' : 'Move item to trash?'}
          confirmLabel={deleteConfirm.permanent ? 'Delete' : 'Move to trash'}
          danger
          onConfirm={() => { fireAndForget(removeItems(deleteConfirm.ids)); setDeleteConfirm(null) }}
          onCancel={() => setDeleteConfirm(null)}
        />
      )}
    </div>
  )
}

function formatBytes(bytes) {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** i).toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export default Drive
