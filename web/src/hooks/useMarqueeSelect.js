import { useEffect, useRef } from 'react'
import { useFiles } from '../context/FileContext'

// A press that never moves further than this is a click, not a drag. Without a
// threshold, every click on the background would paint a 0x0 box for a frame
// and read as a flicker.
const DRAG_THRESHOLD = 4

// How close to the top/bottom edge the pointer has to get before the listing
// scrolls on its own, and the fastest it scrolls (px per frame).
const EDGE = 44
const MAX_SCROLL = 18

const INTERACTIVE = 'button, a, input, textarea, select, [role="button"], [data-file-id]'

/**
 * Rubber-band selection: drag a box across the empty space of a listing and
 * everything it touches gets selected, the way a desktop file manager works.
 *
 * The band only starts on the background. Starting it on an item would take
 * the press away from the item's own drag-to-move, and a file manager that
 * moves a file when you meant to draw a box is worse than one without a box.
 * Ctrl/Cmd/Shift latches at press time and adds to whatever was selected.
 *
 * Everything is measured in the container's scroll-content coordinates, so the
 * box stays anchored to the tiles it is covering while the listing auto-scrolls
 * under the pointer. Only the DOM is read here — no item list is passed in, so
 * the band keeps working across grid and list mode and never goes stale when a
 * folder reloads mid-drag.
 */
export function useMarqueeSelect(containerRef) {
  const { selectMany, clearSelection } = useFiles()

  // Latest callbacks, so the effect below can stay mounted for the life of the
  // page instead of re-binding listeners on every render.
  const api = useRef({ selectMany, clearSelection })
  api.current = { selectMany, clearSelection }

  useEffect(() => {
    const container = containerRef.current
    if (!container) return undefined

    let gesture = null
    let box = null
    let frame = 0
    let pointer = { x: 0, y: 0 }
    let signature = null

    const stop = () => {
      if (frame) cancelAnimationFrame(frame)
      frame = 0
      box?.remove()
      box = null
      container.classList.remove('is-marquee')
      gesture = null
      signature = null
      window.removeEventListener('pointermove', onMove)
      window.removeEventListener('pointerup', onEnd)
      window.removeEventListener('pointercancel', onEnd)
      window.removeEventListener('blur', onEnd)
    }

    // Items intersecting the band, in DOM order. Rects are read at paint time
    // rather than cached, so scrolling and re-layout are picked up for free.
    const paint = () => {
      if (!gesture || !box) return
      const rect = container.getBoundingClientRect()
      const scrollX = container.scrollLeft
      const scrollY = container.scrollTop

      // The pointer is clamped to the visible card: dragging out of the pane
      // must not stretch the box over the status bar or the page behind it.
      const px = Math.min(Math.max(pointer.x, rect.left), rect.right)
      const py = Math.min(Math.max(pointer.y, rect.top), rect.bottom)
      const x2 = px - rect.left + scrollX
      const y2 = py - rect.top + scrollY

      const x1 = Math.min(gesture.startX, x2)
      const y1 = Math.min(gesture.startY, y2)
      const w = Math.abs(x2 - gesture.startX)
      const h = Math.abs(y2 - gesture.startY)
      box.style.transform = `translate(${x1}px, ${y1}px)`
      box.style.width = `${w}px`
      box.style.height = `${h}px`

      const hits = []
      for (const node of container.querySelectorAll('[data-file-id]')) {
        const r = node.getBoundingClientRect()
        const left = r.left - rect.left + scrollX
        const top = r.top - rect.top + scrollY
        const right = left + r.width
        const bottom = top + r.height
        if (left < x1 + w && right > x1 && top < y1 + h && bottom > y1) {
          hits.push(node.dataset.fileId)
        }
      }

      // Every paint that changes the hit set is a re-render of the whole grid,
      // so identical sets are dropped instead of being committed again.
      const next = hits.join(' ')
      if (next === signature) return
      signature = next
      api.current.selectMany(hits, { additive: gesture.additive })
    }

    // A pointer parked near an edge stops firing pointermove, so the scroll has
    // to be driven by a frame loop once the band is live.
    const tick = () => {
      frame = 0
      if (!gesture) return
      const rect = container.getBoundingClientRect()
      const y = pointer.y
      let delta = 0
      if (y < rect.top + EDGE) {
        delta = -MAX_SCROLL * (1 - Math.max(0, y - rect.top) / EDGE)
      } else if (y > rect.bottom - EDGE) {
        delta = MAX_SCROLL * (1 - Math.max(0, rect.bottom - y) / EDGE)
      }
      if (delta) {
        const before = container.scrollTop
        container.scrollTop = before + delta
        if (container.scrollTop !== before) paint()
      }
      frame = requestAnimationFrame(tick)
    }

    function onMove(event) {
      if (!gesture || event.pointerId !== gesture.pointerId) return
      pointer = { x: event.clientX, y: event.clientY }

      if (!gesture.active) {
        const dx = Math.abs(event.clientX - gesture.originX)
        const dy = Math.abs(event.clientY - gesture.originY)
        if (dx < DRAG_THRESHOLD && dy < DRAG_THRESHOLD) return
        gesture.active = true
        // Built on the first real movement, not on every press: a plain click
        // on the background must not churn a node in and out of the listing.
        box = document.createElement('div')
        box.className = 'marquee-box'
        container.appendChild(box)
        frame = requestAnimationFrame(tick)
      }
      paint()
    }

    function onEnd(event) {
      if (event && event.pointerId !== undefined && gesture && event.pointerId !== gesture.pointerId) return
      stop()
    }

    function onDown(event) {
      // Mouse only. On a touch screen a drag on the background is a scroll, and
      // claiming it would make the listing feel stuck.
      if (gesture || event.button !== 0 || event.pointerType !== 'mouse') return
      if (event.target.closest?.(INTERACTIVE)) return
      if (!container.contains(event.target)) return
      // Nothing to band over — leave plain clicks on an empty folder alone.
      if (!container.querySelector('[data-file-id]')) return

      const rect = container.getBoundingClientRect()
      gesture = {
        pointerId: event.pointerId,
        additive: event.metaKey || event.ctrlKey || event.shiftKey,
        originX: event.clientX,
        originY: event.clientY,
        startX: event.clientX - rect.left + container.scrollLeft,
        startY: event.clientY - rect.top + container.scrollTop,
        active: false,
      }
      pointer = { x: event.clientX, y: event.clientY }

      // A plain press on the background drops the selection immediately, which
      // is the same thing the click handlers used to do — and it is what makes
      // the band start empty. With a modifier the base selection is kept.
      if (!gesture.additive) api.current.clearSelection()

      // Kills text selection over the tile labels for the length of the drag.
      container.classList.add('is-marquee')

      window.addEventListener('pointermove', onMove)
      window.addEventListener('pointerup', onEnd)
      window.addEventListener('pointercancel', onEnd)
      window.addEventListener('blur', onEnd)
    }

    container.addEventListener('pointerdown', onDown)
    return () => {
      container.removeEventListener('pointerdown', onDown)
      stop()
    }
  }, [containerRef])
}
