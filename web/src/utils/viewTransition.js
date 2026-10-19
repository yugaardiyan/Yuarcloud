import { useCallback } from 'react'
import { flushSync } from 'react-dom'
import { useLocation, useNavigate } from 'react-router-dom'

/**
 * Whether a view transition can run at all: the browser has to be able to keep
 * the old view on screen, and the visitor must not have asked for less motion.
 */
function canTransition() {
  return (
    typeof document.startViewTransition === 'function' &&
    !window.matchMedia('(prefers-reduced-motion: reduce)').matches
  )
}

/**
 * Commits an update as a browser view transition.
 *
 * CSS can animate an element in, but never out: React removes the node from
 * the document, and an animation on a removed node has nothing left to run on.
 * The View Transitions API takes a picture of the page before the update and
 * after it, keeps both alive for the length of the animation and lets CSS move
 * them — the only way the outgoing view can be seen leaving.
 *
 * flushSync is not optional. The browser takes its "after" picture as soon as
 * the callback returns, and React would otherwise commit on the next tick, so
 * the picture would still be the old view and there would be nothing to reveal.
 * Without a transition (or with motion turned down) the update runs plainly.
 */
export function withViewTransition(update) {
  if (!canTransition()) {
    update()
    return
  }

  const transition = document.startViewTransition(() => flushSync(update))
  // A transition started while another is still running is skipped, and a
  // skipped transition rejects `ready`. Nothing here depends on it, so the
  // rejection is swallowed rather than left to surface as a console error.
  transition.ready.catch(() => {})
}

/**
 * `navigate` for moves between views. Drilling into a folder is deliberately
 * not one: the new listing has not arrived yet, so what the browser would
 * photograph is an empty card, and the animation would play over a load
 * instead of over the result.
 */
export function useViewNavigate() {
  const navigate = useNavigate()
  const { pathname, search } = useLocation()

  return useCallback(
    (to, options) => {
      // History moves (a number) are handled by the router, not by a URL.
      if (typeof to === 'number') {
        navigate(to)
        return
      }
      // Asking for the view that is already on screen would animate it out and
      // straight back in, which reads as a flicker rather than as a move. The
      // nav buttons stay enabled wherever you are, so this is a real click.
      if (to === pathname + search) {
        navigate(to, options)
        return
      }
      withViewTransition(() => navigate(to, options))
    },
    [navigate, pathname, search]
  )
}
