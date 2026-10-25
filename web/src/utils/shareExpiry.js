/**
 * When the share dialog should read its list again.
 *
 * A link with a duration stops existing the moment its date passes, and the
 * server already leaves expired links out of the response — which is why the
 * settings page shows them dropping away on its own. The dialog fetches once
 * when it opens, so it needs to wake up at the soonest deadline instead of
 * polling, and go quiet again as soon as the list holds nothing due.
 *
 * Returns the delay in milliseconds, or null when nothing in the list can
 * expire (every link is "no expiry").
 */

// A second past the deadline: the server compares the date against its own
// clock, so waking exactly on the boundary can still read as "not yet".
const SETTLE_MS = 1000

// setTimeout clamps anything past this and fires at once, which would turn a
// year-long link into a tight loop.
const MAX_TIMEOUT = 2147483647

// A date behind us that the server still returns means the two clocks disagree.
// Ask again shortly rather than trusting this device and hiding a link that may
// still open for everyone else.
const RETRY_MS = 30000

export function nextExpiryDelay(shares, now = Date.now()) {
  const dates = (shares || [])
    .map((share) => (share?.expires_at ? Date.parse(share.expires_at) : NaN))
    .filter((at) => Number.isFinite(at))

  const soonest = dates.filter((at) => at > now).sort((a, b) => a - b)[0]
  if (soonest) return Math.min(soonest - now + SETTLE_MS, MAX_TIMEOUT)

  return dates.some((at) => at <= now) ? RETRY_MS : null
}
