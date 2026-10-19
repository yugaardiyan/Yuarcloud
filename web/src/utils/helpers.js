export function formatBytes(bytes, decimals = 1) {
  if (bytes === 0) return '0 B'
  const k = 1024
  const dm = decimals < 0 ? 0 : decimals
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i]
}

export function formatDate(dateString) {
  const date = new Date(dateString)
  if (Number.isNaN(date.getTime())) return '—'

  const now = new Date()
  const diff = now - date

  // Dates in the future (share expiry, for one) must always carry the date.
  // Folding them into a bare clock time made "Expires: 05:37 PM" ambiguous —
  // that reads as a time today, not a date a week out.
  if (diff < 0) {
    return -diff < 24 * 60 * 60 * 1000
      ? date.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
      : date.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' })
  }

  // One shape for everything in the past: the day it happened and the time it
  // happened. It used to change with age — a clock alone inside a day, then a
  // weekday and a clock inside a week, then a date with no clock after that —
  // so two folders side by side could read "03:51 AM" and "Sun 05:16 AM" and
  // look like they were reporting different things. The weekday is also the
  // least useful part of it: it only helps while the date is still this week,
  // and by then the column has already changed shape from row to row.
  //
  // The year stays only when it is not the current one. Dropping it from a file
  // last touched in 2024 would make it look like it was touched this month.
  const options = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }
  if (date.getFullYear() !== now.getFullYear()) options.year = 'numeric'
  return date.toLocaleString([], options)
}

/**
 * Copy text to the clipboard, falling back to execCommand when the async
 * Clipboard API is not usable.
 *
 * navigator.clipboard is gated behind a secure context. Served over plain HTTP
 * on anything other than localhost — a LAN address, a reverse proxy that does
 * not terminate TLS — the object is missing entirely, and calling into it
 * throws rather than returning false. The execCommand path still works there,
 * so try it before giving up.
 *
 * @returns {Promise<boolean>} whether the text made it to the clipboard.
 */
export async function copyText(text) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // Permission denied, or the document was not focused. The legacy path
    // does not care about either, so fall through to it.
  }

  try {
    const ta = document.createElement('textarea')
    ta.value = text
    // Off-screen but still selectable — display:none would make the copy a
    // no-op in some browsers.
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '-1000px'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    ta.setSelectionRange(0, ta.value.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}