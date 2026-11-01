import { useEffect, useRef, useState, useCallback } from 'react'
import useSwipe from './useSwipe'

/**
 * Image preview driven entirely by gesture — no buttons on the image itself.
 *
 * Four gestures share one surface and must never be confused with each other:
 *
 *   - Wheel / pinch          — zoom
 *   - Drag, while zoomed in  — pan the image, because there is something
 *                              off-screen to reach
 *   - Drag, while zoomed out — page to the next image, because the whole
 *                              image is already visible and there is nothing
 *                              to pan to
 *   - Double-click / tap     — back to fit
 *
 * Pinch is the reason there are no zoom buttons. `.preview-image-stage`
 * carries `touch-action: none`, which switches off the browser's own
 * pinch-to-zoom; without a replacement, a phone or tablet could not zoom at
 * all once the buttons were gone.
 */

const MIN_ZOOM = 0.1
const MAX_ZOOM = 12

export default function ImageViewer({ src, alt, canPage, onPage }) {
  const [zoom, setZoom] = useState(1)
  const [pan, setPan] = useState({ x: 0, y: 0 })
  const [loaded, setLoaded] = useState(false)
  const [failed, setFailed] = useState(false)

  const panning = useRef(false)
  const last = useRef({ x: 0, y: 0 })

  // Every pointer currently down, and the pinch baseline once there are two.
  const pointers = useRef(new Map())
  const pinch = useRef(null)

  // A new file must not inherit the previous file's zoom and offset.
  useEffect(() => {
    setZoom(1)
    setPan({ x: 0, y: 0 })
    setLoaded(false)
    setFailed(false)
  }, [src])

  const clamp = useCallback((z) => Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, z)), [])

  // Double-click / double-tap: back to fit. With the toolbar gone this is the
  // only way home from a deep zoom.
  const reset = useCallback(() => {
    setZoom(1)
    setPan({ x: 0, y: 0 })
  }, [])

  // Wheel zoom. Browsers only send deltaY, so the anchor is approximated by
  // the pointer's offset from the centre — good enough to keep the spot under
  // the cursor on screen while zooming.
  const onWheel = useCallback((e) => {
    e.preventDefault()
    const next = clamp(zoom * (e.deltaY < 0 ? 1.15 : 1 / 1.15))
    setZoom(next)
    if (next <= 1) setPan({ x: 0, y: 0 })
  }, [zoom, clamp])

  const onPagePrev = useCallback(() => onPage?.(-1), [onPage])
  const onPageNext = useCallback(() => onPage?.(1), [onPage])

  const swipe = useSwipe({
    // A second finger means pinch, never a swipe.
    enabled: !!canPage && zoom <= 1 && !failed && pointers.current.size < 2,
    onPrev: onPagePrev,
    onNext: onPageNext,
  })

  const capture = (el, id, on) => {
    try {
      if (on) el.setPointerCapture?.(id)
      else el.releasePointerCapture?.(id)
    } catch {
      /* capture is an optimisation, not a requirement */
    }
  }

  const beginPinch = () => {
    const pts = [...pointers.current.values()]
    if (pts.length < 2) return
    const [a, b] = pts
    pinch.current = {
      // Guard against a zero baseline if both fingers land on the same pixel.
      dist: Math.max(1, Math.hypot(b.x - a.x, b.y - a.y)),
      zoom,
      panX: pan.x,
      panY: pan.y,
      midX: (a.x + b.x) / 2,
      midY: (a.y + b.y) / 2,
    }
  }

  const updatePinch = () => {
    const start = pinch.current
    const pts = [...pointers.current.values()]
    if (!start || pts.length < 2) return

    const [a, b] = pts
    const dist = Math.max(1, Math.hypot(b.x - a.x, b.y - a.y))
    const next = clamp(start.zoom * (dist / start.dist))
    setZoom(next)

    // The midpoint travels too, so the image follows it — otherwise a pinch
    // would zoom about the centre of the screen while your fingers sat off to
    // one side.
    const midX = (a.x + b.x) / 2
    const midY = (a.y + b.y) / 2
    let panX = start.panX + (midX - start.midX)
    let panY = start.panY + (midY - start.midY)

    // Back at fit, there is nothing to pan; snap rather than leave it adrift.
    if (next <= 1) { panX = 0; panY = 0 }
    setPan({ x: panX, y: panY })
  }

  const onPointerDown = (e) => {
    pointers.current.set(e.pointerId, { x: e.clientX, y: e.clientY })

    if (pointers.current.size >= 2) {
      beginPinch()
      // A two-finger gesture is never a page and never a pan.
      swipe.cancel()
      panning.current = false
      return
    }

    swipe.swipeProps.onPointerDown(e)

    if (zoom > 1) {
      panning.current = true
      last.current = { x: e.clientX - pan.x, y: e.clientY - pan.y }
      capture(e.currentTarget, e.pointerId, true)
    }
  }

  const onPointerMove = (e) => {
    const tracked = pointers.current.get(e.pointerId)
    if (tracked) {
      tracked.x = e.clientX
      tracked.y = e.clientY
    }

    // Pinch owns the gesture outright while two fingers are down, including
    // the motion of the finger that arrived first.
    if (pinch.current) {
      updatePinch()
      return
    }
    if (!tracked) return

    swipe.swipeProps.onPointerMove(e)

    if (panning.current) {
      setPan({ x: e.clientX - last.current.x, y: e.clientY - last.current.y })
    }
  }

  const endPointer = (e, cancelled) => {
    pointers.current.delete(e.pointerId)
    if (pointers.current.size < 2) pinch.current = null

    if (cancelled) swipe.swipeProps.onPointerCancel(e)
    else swipe.swipeProps.onPointerUp(e)

    capture(e.currentTarget, e.pointerId, false)
    panning.current = false
  }

  if (failed) {
    return <div className="preview-fallback">This image could not be decoded by the browser.</div>
  }

  return (
    <div
      className={`preview-image-stage ${swipe.active ? 'is-swiping' : ''} ${zoom > 1 ? 'is-zoomed' : ''}`}
      onWheel={onWheel}
      onDoubleClick={reset}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={(e) => endPointer(e, false)}
      onPointerCancel={(e) => endPointer(e, true)}
      onClickCapture={swipe.swipeProps.onClickCapture}
    >
      {!loaded && <div className="preview-loading">Loading…</div>}
      <img
        className="preview-image"
        src={src}
        alt={alt || ''}
        draggable={false}
        style={{
          transform: `translate3d(${pan.x + swipe.dx}px, ${pan.y}px, 0) scale(${zoom})`,
          transition: swipe.active ? 'none' : undefined,
        }}
        onLoad={() => setLoaded(true)}
        onError={() => setFailed(true)}
      />
    </div>
  )
}