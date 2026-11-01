/**
 * Upload speed and time remaining.
 *
 * The bytes were always known — `onUploadProgress` hands over `e.loaded`, and
 * the resumable uploader knows the server's acknowledged offset — but both were
 * collapsed to a percentage before reaching the tray. This turns those bytes
 * back into a rate.
 *
 * Two shapes of upload need different treatment, which is the whole reason
 * this is not a one-liner over elapsed time:
 *
 * A plain upload reports continuously *and* has nowhere else for the bytes to
 * go, so a short recent window is both available and the honest answer — the
 * last few seconds say how fast the connection is *now*.
 *
 * A resumable (tus) upload reports the same way within a chunk, but those bytes
 * are only in the socket buffer. Counting them makes the rate spike at the start
 * of every chunk and then flatline at the boundary, and an ETA derived from it
 * promises seconds that the transfer cannot deliver. It is measured from the
 * offsets the server acknowledges instead — see createSpeedTracker.
 *
 * Deliberately not the average since the start on either path: the first second
 * of any connection is its slowest, and one slow second drags the average down
 * for the rest of a large file, reporting a connection as far slower than it is.
 */

/** Preferred measurement window. */
const MIN_SPAN_MS = 1000

/**
 * Ceiling on how far back the window reaches. Without it, a chunked upload that
 * has been running for ten minutes would keep measuring over all ten of them,
 * so a stall right now would still report the speed from nine minutes ago.
 */
const MAX_SPAN_MS = 3000

/** Samples retained per job. Bounds memory; well past any window above. */
const MAX_SAMPLES = 64

/**
 * How far back acknowledged bytes are measured.
 *
 * A tus upload is acknowledged a whole chunk at a time, so two acknowledgements
 * can be many seconds apart. The window is deliberately wider than the one used
 * for progress samples so that it spans several chunks: that is what makes the
 * figure a measurement of the link rather than of whichever round trip happened
 * to be slowest.
 */
const ACK_WINDOW_MS = 30000

/** Acknowledgements retained per job. Bounds memory on a very long upload. */
const MAX_ACKS = 64

/**
 * Bytes per second across the newest sample and one at least MIN_SPAN_MS older,
 * never reaching further back than MAX_SPAN_MS. Returns null when there is not
 * yet enough evidence to say anything, so callers can show nothing rather than
 * a confident zero.
 *
 * This reads the progress events a plain upload emits — bytes as the browser
 * hands them to the socket. A tus upload uses rateFromAcks instead, for the
 * reason spelled out on createSpeedTracker.
 */
export function rateFromSamples(samples) {
  if (!Array.isArray(samples) || samples.length < 2) return null

  const last = samples[samples.length - 1]
  let start = 0
  for (let i = samples.length - 2; i >= 0; i -= 1) {
    start = i
    const span = last.t - samples[i].t
    if (span >= MIN_SPAN_MS || span >= MAX_SPAN_MS) break
  }

  const seconds = (last.t - samples[start].t) / 1000
  if (seconds <= 0) return null

  const bytes = last.loaded - samples[start].loaded
  return bytes > 0 ? bytes / seconds : 0
}

/**
 * Bytes per second from offsets the server has acknowledged.
 *
 * `acks` are `{ t, bytes }` pairs, one per chunk the server confirmed. Two are
 * needed before a rate exists at all — a single point has no elapsed span —
 * which is why the tray stays quiet for the first chunk rather than printing a
 * number that only describes the socket buffer filling up.
 *
 * Measured across everything inside ACK_WINDOW_MS, falling back to the whole
 * history when acknowledgements are further apart than that (a chunk slower
 * than the window). Bytes that went backwards are ignored by the subtraction
 * itself: a smaller offset than the window start yields a negative delta and is
 * reported as no rate rather than a negative one.
 */
export function rateFromAcks(acks) {
  if (!Array.isArray(acks) || acks.length < 2) return null

  const last = acks[acks.length - 1]
  let start = 0
  for (let i = acks.length - 1; i >= 0; i -= 1) {
    if (last.t - acks[i].t > ACK_WINDOW_MS) break
    start = i
  }
  // A window narrow enough to hold only the newest point has no span to divide
  // by; measure the whole history instead of reporting nothing.
  if (start >= acks.length - 1) start = 0

  const seconds = (last.t - acks[start].t) / 1000
  if (seconds <= 0) return null

  const bytes = last.bytes - acks[start].bytes
  return bytes > 0 ? bytes / seconds : 0
}

/**
 * Per-job sample buffers.
 *
 * Kept outside React state on purpose: these are written on every progress
 * event and read only when rendering, so putting them in state would re-render
 * the tray on every one. The derived rate is what reaches state — one update
 * per event, which is what the tray already did for the percentage.
 *
 * Two sources feed a rate, and which one is used is the whole point here.
 *
 * A plain upload only ever reports bytes as the browser hands them to the
 * socket, so a short recent window is the honest answer.
 *
 * A tus upload reports the same way *within* a chunk — but those bytes are
 * still sitting in the socket buffer, so on a slow or tunneled link the figure
 * races ahead of what the server has actually taken, then freezes at the chunk
 * boundary while the tail drains and the next request waits its turn. The speed
 * read "1.8 MB/s · 2s left" moments before a pause several seconds long: an ETA
 * that can never be met. So a tus upload is measured only from offsets the
 * server acknowledged, which no amount of local buffering can inflate. The
 * number then moves in steps the size of a chunk, and the remaining time
 * divided by it is one that can actually be kept.
 */
export function createSpeedTracker() {
  const buffers = new Map()

  const entry = (jobId) => {
    let e = buffers.get(jobId)
    if (!e) {
      e = { samples: [], acks: [], hasAcks: false }
      buffers.set(jobId, e)
    }
    return e
  }

  return {
    /** Drops any history so a retried or requeued file starts from zero. */
    reset(jobId) {
      buffers.set(jobId, { samples: [], acks: [], hasAcks: false })
    },

    /**
     * Records progress and returns the current rate, or null if it is not
     * knowable yet.
     *
     * `confirmed` is the offset the server has acknowledged, when the caller
     * has one. Passing it at all switches this job to the acknowledged
     * measurement — including while the first chunk is still in flight, when
     * that yields null and the tray correctly says nothing. Events that carry
     * no new bytes are dropped: they would fill the buffer with repeated points
     * and flatten the window.
     */
    push(jobId, loaded, confirmed, now = Date.now()) {
      const e = entry(jobId)

      if (Number.isFinite(loaded) && loaded >= 0) {
        const prev = e.samples[e.samples.length - 1]
        if (!prev || prev.loaded !== loaded) {
          e.samples.push({ t: now, loaded })
          if (e.samples.length > MAX_SAMPLES) e.samples.shift()
        }
      }

      if (Number.isFinite(confirmed) && confirmed >= 0) {
        e.hasAcks = true
        const prev = e.acks[e.acks.length - 1]
        if (!prev || prev.bytes !== confirmed) {
          e.acks.push({ t: now, bytes: confirmed })
          if (e.acks.length > MAX_ACKS) e.acks.shift()
        }
      }

      return e.hasAcks ? rateFromAcks(e.acks) : rateFromSamples(e.samples)
    },

    /** Releases a finished job's buffer. */
    forget(jobId) {
      buffers.delete(jobId)
    },
  }
}

/** Seconds remaining, or null when the rate is not yet known. */
export function etaFromRate(rate, loaded, total) {
  if (!rate || rate <= 0) return null
  const remaining = total - loaded
  if (remaining <= 0) return null
  return remaining / rate
}

/** e.g. `4.2 MB/s`. Formatted through formatBytes so it matches the rest of the app. */
export function formatRate(bytesPerSecond, formatBytes) {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return null
  return `${formatBytes(bytesPerSecond)}/s`
}

/** e.g. `40s`, `1m 20s`, `2h 5m`. Rounded up, since a fraction of a second shown is noise. */
export function formatEta(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return null
  const total = Math.ceil(seconds)
  if (total < 60) return `${total}s`
  if (total < 3600) {
    const m = Math.floor(total / 60)
    const s = total % 60
    return s ? `${m}m ${s}s` : `${m}m`
  }
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  return m ? `${h}h ${m}m` : `${h}h`
}