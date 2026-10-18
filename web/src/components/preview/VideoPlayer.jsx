import { useRef } from 'react'
import useSwipe from './useSwipe'

/**
 * Video player that can also be dragged sideways to page.
 *
 * The awkward part is that <video controls> owns its own gestures — click to
 * play, drag on the progress bar to seek — and a wrapper-level drag handler
 * would fight all of them. So the gesture is only claimed when the pointer is
 * somewhere that is not a control:
 *
 *   - not the bottom strip, where the native control bar lives;
 *   - not a click on the video element itself while the pointer is down
 *     without moving, which is the native play/pause toggle.
 *
 * Anything else — the black letterbox area, or a drag that starts on the
 * picture and travels sideways — pages.
 */
export default function VideoPlayer({ src, title, canPage, onPage }) {
  const videoRef = useRef(null)

  const onPagePrev = () => onPage?.(-1)
  const onPageNext = () => onPage?.(1)

  const swipe = useSwipe({
    enabled: !!canPage,
    onPrev: onPagePrev,
    onNext: onPageNext,
  })

  // The native control bar is roughly 40px tall in every engine. Anything in
  // that band belongs to the video, not to us.
  const inControlBar = (e) => {
    const video = videoRef.current
    if (!video) return false
    const rect = video.getBoundingClientRect()
    return e.clientY > rect.bottom - 44
  }

  const inVideoControls = (e) => {
    const video = videoRef.current
    if (!video) return false
    // A press on the picture that has not moved is the play/pause toggle, and
    // the video element swallows the event before it reaches the wrapper.
    return e.target === video && e.type === 'pointerdown'
  }

  const guard = (handler) => (e) => {
    if (inControlBar(e) || inVideoControls(e)) return
    handler(e)
  }

  const handlers = {
    onPointerDown: guard(swipe.swipeProps.onPointerDown),
    onPointerMove: swipe.swipeProps.onPointerMove,
    onPointerUp: swipe.swipeProps.onPointerUp,
    onPointerCancel: swipe.swipeProps.onPointerCancel,
    onClickCapture: swipe.swipeProps.onClickCapture,
  }

  return (
    <div className={`preview-video-stage ${swipe.active ? 'is-swiping' : ''}`} {...handlers}>
      <video
        ref={videoRef}
        className="preview-video"
        src={src}
        title={title}
        controls
        preload="metadata"
        style={{ transform: `translateX(${swipe.dx}px)`, transition: swipe.active ? 'none' : undefined }}
      />
    </div>
  )
}