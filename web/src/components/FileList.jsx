import React, { useCallback, useEffect, useRef, useState } from 'react'
import { Check, ArrowUpDown } from 'lucide-react'
import { useFiles } from '../context/FileContext'
import { useContextMenu } from './ContextMenuProvider'
import { formatBytes, formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from './FileIcon'
import FileMenuButton from './FileMenuButton'
import { useDragMove } from '../hooks/useDragMove'
import EmptyState from './EmptyState'

/** Where a dragged name column is remembered between visits. */
const NAME_WIDTH_KEY = 'list.nameWidth'
/* Below this the filenames are unreadable; past this the size and modified
   columns get squeezed off a narrow window. The row's own fixed columns take
   about 280px, so the cap is a share of what is left rather than a raw number. */
const NAME_WIDTH_MIN = 160

function readStoredWidth() {
  const raw = localStorage.getItem(NAME_WIDTH_KEY)
  if (raw === null) return null // null means "let the layout decide"
  const n = Number(raw)
  return Number.isFinite(n) && n >= NAME_WIDTH_MIN ? n : null
}

function FileList({ onOpen, onDownload, onFile }) {
  const {
    items, view, selected, select, selectAll, clearSelection,
    isSelected, openFolder, downloadItem, sort, toggleSort,
  } = useFiles()
  const { openMenu } = useContextMenu()
  const listRef = useRef(null)
  const { itemProps } = useDragMove()
  const longPressTimer = useRef(null)
  const longPressTriggered = useRef(false)

  // null until the user drags, so the column starts on the responsive behaviour
  // and only pins itself once there is a width of theirs to honour.
  const [nameWidth, setNameWidth] = useState(readStoredWidth)
  const [resizing, setResizing] = useState(false)
  const resizerRef = useRef(null)

  const applyWidth = useCallback((next) => {
    setNameWidth(next)
    if (next === null) localStorage.removeItem(NAME_WIDTH_KEY)
    else localStorage.setItem(NAME_WIDTH_KEY, String(Math.round(next)))
  }, [])

  // Pointer drag. Measured from the column's current width at pointer-down rather
  // than from the grab point, so a press in the middle of the handle moves the
  // edge by the same distance the pointer travelled instead of jumping.
  const startResize = useCallback((e) => {
    if (e.button !== undefined && e.button !== 0) return
    e.preventDefault()
    e.stopPropagation()

    const cell = resizerRef.current?.parentElement
    if (!cell) return

    const startX = e.clientX
    const startW = cell.getBoundingClientRect().width
    setResizing(true)

    const onMove = (ev) => {
      const max = Math.max(NAME_WIDTH_MIN, window.innerWidth - 380)
      applyWidth(Math.min(max, Math.max(NAME_WIDTH_MIN, startW + (ev.clientX - startX))))
    }
    const onUp = () => {
      setResizing(false)
      window.removeEventListener('pointermove', onMove)
      window.removeEventListener('pointerup', onUp)
      // The press that started the drag also lands as a click on the sort button
      // underneath; without this, letting go of the handle re-sorts the list.
      document.body.classList.remove('is-resizing-column')
    }

    window.addEventListener('pointermove', onMove)
    window.addEventListener('pointerup', onUp)
    document.body.classList.add('is-resizing-column')
  }, [applyWidth])

  // Arrow keys move the edge too. A drag-only control is unreachable without a
  // pointer, and the arrow keys are how a column divider is expected to behave.
  const nudgeWidth = useCallback((delta) => {
    const cell = resizerRef.current?.parentElement
    const current = cell ? cell.getBoundingClientRect().width : nameWidth || 0
    const max = Math.max(NAME_WIDTH_MIN, window.innerWidth - 380)
    applyWidth(Math.min(max, Math.max(NAME_WIDTH_MIN, current + delta)))
  }, [applyWidth, nameWidth])

  // While dragging, stop the row beneath from taking the pointer as a selection
  // and let the browser keep sending move events even at the window edge.
  useEffect(() => {
    if (!resizing) return
    const prevent = (e) => e.preventDefault()
    document.addEventListener('selectstart', prevent)
    return () => document.removeEventListener('selectstart', prevent)
  }, [resizing])

  // Folders navigate; files open in the viewer when one is wired up, and fall
  // back to downloading when the list is rendered somewhere without it.
  const activate = (item) => {
    if (!item) return
    if (item.is_dir) (onOpen || openFolder)(item)
    else (onFile || onDownload || downloadItem)(item)
  }

  useEffect(() => {
    const onKey = (e) => {
      if (!listRef.current?.contains(document.activeElement)) return

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'a') {
        e.preventDefault()
        selectAll()
        return
      }
      if (e.key === 'Escape') { clearSelection(); return }
      if (!['Enter', ' ', 'ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return

      const nodes = Array.from(listRef.current.querySelectorAll('[data-file-id]'))
      if (!nodes.length) return

      const current = nodes.findIndex((n) => n.dataset.fileId === e.target?.dataset?.fileId)

      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        activate(items[current])
        return
      }

      let next = current
      if (e.key === 'ArrowDown') next = Math.min(current + 1, nodes.length - 1)
      if (e.key === 'ArrowUp') next = Math.max(current - 1, 0)
      if (e.key === 'Home') next = 0
      if (e.key === 'End') next = nodes.length - 1

      if (next === current) return
      e.preventDefault()
      nodes[next]?.focus()
      select(nodes[next].dataset.fileId, { additive: e.shiftKey, range: e.shiftKey })
    }

    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [items, select, selectAll, clearSelection])

  const SortHeader = ({ label, sortKey, className }) => (
    <button
      className={`sort-header ${className || ''}`}
      onClick={() => toggleSort(sortKey)}
      aria-label={`Sort by ${label}`}
    >
      {label}
      <ArrowUpDown
        size={12}
        style={{
          opacity: sort.key === sortKey ? 1 : 0.35,
          transform: sort.key === sortKey && sort.direction === 'desc' ? 'rotate(180deg)' : undefined,
        }}
      />
      {sortKey === 'name' && (
        <span
          ref={resizerRef}
          className={`file-col-resizer${resizing ? ' is-active' : ''}`}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize name column"
          aria-valuenow={nameWidth ? Math.round(nameWidth) : undefined}
          tabIndex={0}
          title="Drag to resize · double-click to reset"
          onPointerDown={startResize}
          onClick={(e) => e.stopPropagation()}
          onDoubleClick={(e) => { e.stopPropagation(); applyWidth(null) }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowLeft') { e.preventDefault(); nudgeWidth(-24) }
            else if (e.key === 'ArrowRight') { e.preventDefault(); nudgeWidth(24) }
            else if (e.key === 'Home') { e.preventDefault(); applyWidth(null) }
          }}
        />
      )}
    </button>
  )

  if (!items.length) {
    return <EmptyState scope={view.scope} />
  }

  return (
    <div
      ref={listRef}
      className="file-list"
      role="grid"
      aria-label="Files"
      tabIndex={0}
      style={{ outline: 'none' }}
    >
      <div className="file-list-header" role="row">
        <span style={{ width: 32, flexShrink: 0 }} />
        <SortHeader label="Name" sortKey="name" className="flex-1" />
        <SortHeader label="Size" sortKey="size" className="file-col-size" />
        <SortHeader label="Modified" sortKey="updated_at" className="file-col-date" />
        {/* Keeps Modified flush with the dates once the per-row menu button
            takes its 28px at the end of every row. */}
        <span className="file-menu-head" aria-hidden="true" />
      </div>

      {items.map((item) => {
        const active = isSelected(item.id)
        const drag = itemProps(item)
        const handleTouchStart = (e) => {
          longPressTriggered.current = false
          longPressTimer.current = setTimeout(() => {
            longPressTriggered.current = true
            select(item.id, { additive: true })
            e.preventDefault()
          }, 500)
        }
        const handleTouchMove = () => {
          if (longPressTimer.current) {
            clearTimeout(longPressTimer.current)
            longPressTimer.current = null
          }
        }
        const handleTouchEnd = (e) => {
          if (longPressTimer.current) {
            clearTimeout(longPressTimer.current)
            longPressTimer.current = null
          }
          if (longPressTriggered.current) {
            e.preventDefault()
            longPressTriggered.current = false
            return
          }
        }
        const handleTouchCancel = () => {
          if (longPressTimer.current) {
            clearTimeout(longPressTimer.current)
            longPressTimer.current = null
          }
          longPressTriggered.current = false
        }
        return (
          <div
            key={item.id}
            data-file-id={item.id}
            data-name={item.name}
            className={`file-item file-row ${active ? 'selected' : ''} ${drag.className || ''}`}
            role="row"
            tabIndex={-1}
            title={item.path}
            draggable={drag.draggable}
            onDragStart={drag.onDragStart}
            onDragEnd={drag.onDragEnd}
            onDragOver={drag.onDragOver}
            onDragEnter={drag.onDragEnter}
            onDragLeave={drag.onDragLeave}
            onDrop={drag.onDrop}
            onTouchStart={handleTouchStart}
            onTouchMove={handleTouchMove}
            onTouchEnd={handleTouchEnd}
            onTouchCancel={handleTouchCancel}
            onClick={(e) => {
              if (e.detail === 2) return
              if (longPressTriggered.current) {
                longPressTriggered.current = false
                return
              }
              select(item.id, { additive: e.metaKey || e.ctrlKey, range: e.shiftKey })
            }}
            onDoubleClick={(e) => { e.preventDefault(); activate(item) }}
            onContextMenu={(e) => {
              if (longPressTriggered.current) {
                e.preventDefault()
                longPressTriggered.current = false
                return
              }
              openMenu(e, item.id, active && selected.size > 1)
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activate(item) }
            }}
          >
            <div className={`file-icon file-icon-sm ${iconClassFor(item)}`}>
              <FileIcon item={item} />
              {active && (
                <span className="file-check file-check-sm" aria-hidden="true">
                  <Check size={10} />
                </span>
              )}
            </div>

            <span className="file-name flex-1" title={item.name}>{item.name}</span>
            <span className="file-col-size">{item.is_dir ? '—' : formatBytes(item.size)}</span>
            <span className="file-col-date">{item.updated_at ? formatDate(item.updated_at) : '—'}</span>

            <FileMenuButton item={item} multi={active && selected.size > 1} />
          </div>
        )
      })}
    </div>
  )
}

export default FileList
