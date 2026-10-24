import React, { useEffect, useRef, useCallback } from 'react'
import { Check } from 'lucide-react'
import { useFiles } from '../context/FileContext'
import { useContextMenu } from './ContextMenuProvider'
import { formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from './FileIcon'
import FileMenuButton from './FileMenuButton'
import { useDragMove } from '../hooks/useDragMove'
import EmptyState from './EmptyState'

function FileGrid({ onOpen, onDownload, onFile }) {
  const {
    items, view, selected, select, selectAll, clearSelection,
    isSelected, openFolder, downloadItem,
  } = useFiles()
  const { openMenu } = useContextMenu()
  const gridRef = useRef(null)
  const { itemProps } = useDragMove()
  const longPressTimer = useRef(null)
  const longPressTriggered = useRef(false)

  // Folders navigate; files open in the viewer when one is wired up, and fall
  // back to downloading when the grid is rendered somewhere without it.
  const activate = (item) => {
    if (!item) return
    if (item.is_dir) (onOpen || openFolder)(item)
    else (onFile || onDownload || downloadItem)(item)
  }

  // Arrow-key navigation. Column count comes from the live layout so it stays
  // correct when the window resizes.
  useEffect(() => {
    const onKey = (e) => {
      if (!gridRef.current?.contains(document.activeElement)) return

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'a') {
        e.preventDefault()
        selectAll()
        return
      }
      if (e.key === 'Escape') {
        clearSelection()
        return
      }
      if (e.key !== 'Enter' && e.key !== ' ' && !e.key.startsWith('Arrow')) return

      const nodes = Array.from(gridRef.current.querySelectorAll('[data-file-id]'))
      if (!nodes.length) return

      const current = nodes.findIndex((n) => n.dataset.fileId === e.target?.dataset?.fileId)

      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault()
        activate(items[current])
        return
      }

      const first = nodes[0]
      const gap = 12
      const cols = first
        ? Math.max(1, Math.round((gridRef.current.clientWidth + gap) / (first.offsetWidth + gap)))
        : 1

      let next = current
      if (e.key === 'ArrowRight') next = Math.min(current + 1, nodes.length - 1)
      if (e.key === 'ArrowLeft') next = Math.max(current - 1, 0)
      if (e.key === 'ArrowDown') next = Math.min(current + cols, nodes.length - 1)
      if (e.key === 'ArrowUp') next = Math.max(current - cols, 0)
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

  if (!items.length) {
    return <EmptyState scope={view.scope} />
  }

  return (
    <div
      ref={gridRef}
      className="file-grid"
      role="grid"
      aria-label="Files"
      tabIndex={0}
      style={{ outline: 'none' }}
    >
      {items.map((item) => {
        const active = isSelected(item.id)
        const drag = itemProps(item)
        const handleTouchStart = (e) => {
          longPressTriggered.current = false
          longPressTimer.current = setTimeout(() => {
            longPressTriggered.current = true
            select(item.id, { additive: true })
            // Prevent context menu on mobile after long press selection
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
            className={`file-item file-card ${active ? 'selected' : ''} ${drag.className || ''}`}
            role="gridcell"
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
            <div className={`file-icon ${iconClassFor(item)}`}>
              <FileIcon item={item} />
            </div>

            {active && (
              <span className="file-check" aria-hidden="true">
                <Check size={11} />
              </span>
            )}

            <div className="file-info">
              <div className="file-name">{item.name}</div>
              <div className="file-meta">
                {/* One short fact per tile. A tile is 116-120px wide, which fits
                    a date but not a date and a size — trying to show both is
                    what made a card's text wider than the card. The size is in
                    the list view and in the Info panel. */}
                {item.updated_at ? formatDate(item.updated_at) : '—'}
              </div>
            </div>

            <FileMenuButton item={item} multi={active && selected.size > 1} />
          </div>
        )
      })}
    </div>
  )
}

export default FileGrid
