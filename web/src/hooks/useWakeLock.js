import { useEffect } from 'react'

/**
 * Keeps the screen awake while a transfer is running.
 *
 * A phone locks its screen after a short idle period, and a hidden or locked
 * tab has its timers and network requests suspended. An upload that outlives
 * the screen timeout — a few minutes for a 100 MB file on mobile data — then
 * freezes part-way through with nothing actually wrong with the connection.
 *
 * The browser releases the lock whenever the page is hidden, so it is
 * requested again when the page comes back. Failure is expected on browsers
 * without the API, on insecure origins, and when the battery is low; none of
 * those is worth surfacing, because the upload itself still works.
 */
export function useWakeLock(active) {
  useEffect(() => {
    if (!active) return
    if (!navigator.wakeLock?.request) return

    let sentinel = null
    let disposed = false

    const acquire = async () => {
      if (disposed || sentinel || document.visibilityState !== 'visible') return
      try {
        const lock = await navigator.wakeLock.request('screen')
        if (disposed) {
          lock.release().catch(() => {})
          return
        }
        // The system releases the lock on its own when the page hides; the
        // reference has to be dropped so a later acquire is not skipped.
        lock.addEventListener('release', () => {
          if (sentinel === lock) sentinel = null
        })
        sentinel = lock
      } catch {
        // Denied, unsupported, or low battery: not a reason to fail an upload.
      }
    }

    const onVisibility = () => {
      if (document.visibilityState === 'visible') acquire()
    }

    acquire()
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      disposed = true
      document.removeEventListener('visibilitychange', onVisibility)
      sentinel?.release().catch(() => {})
      sentinel = null
    }
  }, [active])
}
