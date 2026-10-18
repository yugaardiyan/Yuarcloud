/**
 * Upload speed and time remaining.
 *
 * The bytes were always known — `onUploadProgress` hands over `e.loaded`, and
 * the chunked uploader knows `sentBytes` — but both were collapsed to a
 * percentage before reaching the tray. This turns those bytes back into a rate.
 *
 * Two shapes of upload need different treatment, which is the whole reason
 * this is not a one-liner over elapsed time:
 *
 * A plain upload reports continuously, so a short recent window is both
 * available and the honest answer — the last two seconds say how fast the
 * connection is *now*.
 *
 * A chunked upload only reports once per chunk. Its samples are 5 MB apart and
 * seconds apart, so a short window holds a single sample and no rate at all.
 * Forcing the short window here would make the figure flicker between nothing
 * and a spike on every chunk boundary. It falls back to the widest window it
 * has, which is a stable average over the chunks sent so far.
 *
 * Deliberately not the average since the start on both paths: the first second
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
 * Bytes per second across the newest sample and one at least MIN_SPAN_MS older,
 * never reaching further back than MAX_SPAN_MS. Returns null when there is not
 * yet enough evidence to say anything, so callers can show nothing rather than
 * a confident zero.
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
 * Per-job sample buffers.
 *
 * Kept outside React state on purpose: these are written on every progress
 * event and read only when rendering, so putting them in state would re-render
 * the tray on every one. The derived rate is what reaches state — one update
 * per event, which is what the tray already did for the percentage.
 */
export function createSpeedTracker() {
  const buffers = new Map()

  return {
    /** Drops any history so a retried or requeued file starts from zero. */
    reset(jobId) {
      buffers.set(jobId, [])
    },

    /**
     * Records progress and returns the current rate, or null if it is not
     * knowable yet. Events that carry no new bytes are dropped: they would fill
     * the buffer with repeated points and flatten the window.
     */
    push(jobId, loaded, now = Date.now()) {
      if (!Number.isFinite(loaded) || loaded < 0) return null
      const samples = buffers.get(jobId) || []
      const prev = samples[samples.length - 1]
      if (prev && prev.loaded === loaded) return rateFromSamples(samples)
      samples.push({ t: now, loaded })
      if (samples.length > MAX_SAMPLES) samples.shift()
      buffers.set(jobId, samples)
      return rateFromSamples(samples)
    },

    /** Releases a finished job's buffer. */
    forget(jobId) {
      buffers.delete(jobId)
    },
  }
}

/** Seconds remaining, or null when the rate is unknown or has stalled. */
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