import { useCallback, useRef, useState } from 'react'

/**
 * Drag-to-page gesture for the visual media.
 *
 * Images and video are the two kinds where swiping sideways is the gesture
 * people already expect — the same motion a phone photo gallery uses — so the
 * viewer honours it on a trackpad or a touch screen as well as with the arrow
 * keys. Other kinds (a spreadsheet, a PDF) do not get it: dragging a table
 * sideways has no meaning, and intercepting the gesture there would fight the
 * horizontal scrollbar.
 *
 * Three details keep it from feeling wrong:
 *
 *   - Horizontal dominance. A drag only pages when it is clearly sideways;
 *     a vertical or diagonal drag is left alone, which matters on touch where
 *     the two are easy to confuse.
 *   - No click-through. A drag ends the gesture and suppresses the click, so
 *     releasing over the video does not also toggle play.
 *   - Pointer capture, so a fast drag that leaves the element still tracks.
 *
 * @param {object} options
 * @param {boolean} options.enabled   ignore the gesture entirely when false
 * @param {() => void} options.onPrev
 * @param {() => void} options.onNext
 * @param {number} [options.threshold]  px of travel that counts as a page
 * @returns {{dx: number, dragging: boolean, active: boolean, swipeProps: object}}
 */
export default function useSwipe({ enabled, onPrev, onNext, threshold = 70 }) {
  const [dx, setDx] = useState(0)
  const [dragging, setDragging] = useState(false)

  const start = useRef(null)

  const finish = useCallback(() => {
    const s = start.current
    start.current = null
    setDragging(false)

    // Always clear the offset, whichever way the gesture resolves.
    //
    // It was only cleared on pointercancel, so a drag that ended normally left
    // dx at whatever it last was: a 20px nudge that did not reach the paging
    // threshold left the image visibly off-centre until something else
    // re-rendered it, and a swipe that did page handed its residual offset to
    // the next item, which then opened already shifted. `dragging` going false
    // is what re-enables the CSS transition, so this settles back rather than
    // snapping.
    setDx(0)

    if (!s || !s.moved) return
    // One page per gesture, whatever the distance. A long swipe must not run
    // through five files.
    if (s.dx <= -threshold) onNext()
    else if (s.dx >= threshold) onPrev()
  }, [threshold, onPrev, onNext])

  // Pointer capture is best-effort. It throws for a pointer the element does
  // not own — after a lost capture, and for any synthetic event — and an
  // exception here would abort the handler before the gesture was ever
  // resolved, leaving the viewer stuck mid-drag with no page.
  const capture = (el, id, on) => {
    try {
      if (on) el.setPointerCapture?.(id)
      else el.releasePointerCapture?.(id)
    } catch {
      /* capture is an optimisation, not a requirement */
    }
  }

  const onPointerDown = useCallback((e) => {
    // Left button or touch only; a right-click drag is a context menu.
    if (!enabled || e.button !== 0) return
    start.current = { x: e.clientX, y: e.clientY, dx: 0, moved: false, id: e.pointerId }
  }, [enabled]);

  const onPointerMove = useCallback((e) => {
    const s = start.current
    if (!s || e.pointerId !== s.id) return

    const dx = e.clientX - s.x
    const dy = e.clientY - s.y

    if (!s.moved) {
      // Decide once, on the first real movement, so a later wobble cannot
      // turn a horizontal page into a vertical scroll.
      if (Math.abs(dx) < 6 && Math.abs(dy) < 6) return
      if (Math.abs(dx) <= Math.abs(dy)) {
        start.current = null
        return
      }
      s.moved = true
      setDragging(true)
      capture(e.currentTarget, e.pointerId, true)
    }

    // Recorded on the session, not just rendered: finish() reads this back to
    // decide which way to page, and a value kept only in state would be zero
    // by the time the pointer comes up.
    s.dx = dx

    // Rubber-band past the ends so the viewer acknowledges the gesture even
    // when there is nowhere to go.
    const over = Math.abs(dx) > threshold
    setDx(over ? dx * 0.25 : dx)
  }, [threshold]);

  const onPointerUp = useCallback((e) => {
    if (start.current && e.pointerId !== start.current.id) return
    capture(e.currentTarget, e.pointerId, false)
    // finish() clears the offset itself, and it must run even if releasing
    // capture misbehaved — otherwise the drag never resolves.
    finish()
  }, [finish]);

  const onPointerCancel = useCallback(() => {
    start.current = null
    setDragging(false)
    setDx(0)
  }, [])

  // Abandons a gesture in progress without paging. The image viewer calls
  // this when a second finger lands, because a two-finger gesture is a pinch
  // and must never be read as the start of a swipe.
  const cancel = useCallback(() => {
    onPointerCancel()
  }, [onPointerCancel])

  // A swipe must never also register as a click on whatever it started over.
  const onClickCapture = useCallback((e) => {
    if (!dragging) return
    e.preventDefault()
    e.stopPropagation()
  }, [dragging]);

  const active = enabled && dragging

  return {
    dx,
    dragging,
    active,
    cancel,
    swipeProps: {
      onPointerDown,
      onPointerMove,
      onPointerUp,
      onPointerCancel,
      onClickCapture,
    },
  }
}