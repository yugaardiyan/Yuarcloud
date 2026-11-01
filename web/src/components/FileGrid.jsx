import React, { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useFiles } from '../context/FileContext'
import { useContextMenu } from './ContextMenuProvider'
import { formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from './FileIcon'
import FileMenuButton from './FileMenuButton'
import { useDragMove } from '../hooks/useDragMove'
import EmptyState from './EmptyState'

// One canvas shared by every card. Canvas text metrics are the only way to ask
// "how wide would this string be at this font" without laying out a DOM node
// for each candidate prefix.
let measureCanvas = null

function measureWidth(text, font) {
  if (!measureCanvas) measureCanvas = document.createElement('canvas').getContext('2d')
  if (!measureCanvas) return text.length * 7
  measureCanvas.font = font
  return measureCanvas.measureText(text).width
}

// Cut a name with "..", not the typographic ellipsis. CSS can only draw its
// fixed three-dot glyph, so the cut is measured here instead. A ResizeObserver
// reruns the fit when a column changes width, and the full name stays available
// through the card's `title`.
function TruncatedName({ text }) {
  const ref = useRef(null)
  const [label, setLabel] = useState(text)

  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return undefined

    const fit = () => {
      const width = el.getBoundingClientRect().width
      if (!width) return

      const style = getComputedStyle(el)
      const font = `${style.fontStyle} ${style.fontWeight} ${style.fontSize} ${style.fontFamily}`
      const suffix = '..'
      // 2px of slack: canvas and DOM kerning can disagree by a fraction, and a
      // string that overflows would be clipped without its marker.
      const slack = 2

      if (measureWidth(text, font) + slack <= width) {
        setLabel((prev) => (prev === text ? prev : text))
        return
      }

      const suffixWidth = measureWidth(suffix, font)
      let lo = 0
      let hi = text.length
      while (lo < hi) {
        const mid = Math.ceil((lo + hi) / 2)
        if (measureWidth(text.slice(0, mid), font) + suffixWidth + slack <= width) lo = mid
        else hi = mid - 1
      }

      // Cut on a clean boundary: a prefix that already ends in the file's own
      // dot would otherwise read as three dots once the marker is added.
      const next = `${text.slice(0, lo).replace(/[.\s]+$/, '')}${suffix}`
      setLabel((prev) => (prev === next ? prev : next))
    }

    fit()
    const observer = new ResizeObserver(fit)
    observer.observe(el)
    if (document.fonts?.ready) document.fonts.ready.then(fit).catch(() => {})
    return () => observer.disconnect()
  }, [text])

  return (
    <div ref={ref} className="file-name">
      {label}
    </div>
  )
}

function FileGrid({ onOpen, onDownload, onFile }) {
  const {
    items, view, selected, select, selectAll, clearSelection,
    isSelected, openFolder, downloadItem, selectMode,
  } = useFiles()
  const { openMenu } = useContextMenu()
  const gridRef = useRef(null)
  const { itemProps } = useDragMove()

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
            onClick={(e) => {
              if (e.detail === 2) return
              // In select mode a plain tap toggles selection instead of opening
              // the item — that is the point of "Select" from the context menu.
              if (selectMode) {
                select(item.id, { additive: true })
                return
              }
              select(item.id, { additive: e.metaKey || e.ctrlKey, range: e.shiftKey })
            }}
            onDoubleClick={(e) => {
              e.preventDefault()
              if (selectMode) select(item.id, { additive: true }); else activate(item)
            }}
            onContextMenu={(e) => {
              openMenu(e, item.id, active && selected.size > 1)
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                if (selectMode) select(item.id, { additive: true }); else activate(item)
              }
            }}
          >
            <div className={`file-icon ${iconClassFor(item)}`}>
              <FileIcon item={item} />
            </div>

            <div className="file-info">
              <TruncatedName text={item.name} />
              <div className="file-meta">
                {/* The date sits under the name without a "Modified" prefix;
                    the line below a file name is already understood as its
                    modified time. */}
                {item.updated_at ? formatDate(item.updated_at, { hour12: false }) : '—'}
              </div>
            </div>

            <FileMenuButton item={item} />
          </div>
        )
      })}
    </div>
  )
}

export default FileGrid
