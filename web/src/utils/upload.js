/**
 * Chunked upload client.
 *
 * Splits each file into fixed-size chunks and uploads them one at a time so no
 * single HTTP request ever exceeds the Cloudflare Tunnel request-body limit
 * (100 MB on the free plan). Falls back to a plain single-POST upload when the
 * server has not enabled chunked sessions or the file is small.
 */

import { fileApi, apiError } from './api'

/**
 * Hard client-side ceiling per chunk — Cloudflare Free tier limit.
 *
 * The server's max_chunk_size is the tunable knob (set in Admin UI).
 * Cloudflare rejects request bodies > 100 MB, so we cap at that.
 * The adaptive probe (below) will automatically use a smaller chunk
 * if the connection is too slow to finish a large chunk within the
 * ~100–125s proxy read timeout.
 */
export const MAX_CHUNK_BYTES = 100 * 1024 * 1024 // 100 MB (Cloudflare hard limit)

/** Files at or below this go through the simple upload path. */
export const SIMPLE_UPLOAD_LIMIT = 16 * 1024 * 1024 // 16 MB

/**
 * How many files a browser sends at once.
 *
 * The server stores the admin's choice and hands it back with the chunk limits,
 * so every client agrees on it. Three is enough to keep a batch moving without
 * turning ten files into ten competing streams over a phone hotspot.
 */
export const DEFAULT_CONCURRENT_UPLOADS = 3
export const MAX_CONCURRENT_UPLOADS = 10

function normalizeConcurrentUploads(value) {
  const n = Number(value)
  if (!Number.isFinite(n) || n < 1) return DEFAULT_CONCURRENT_UPLOADS
  return Math.min(Math.floor(n), MAX_CONCURRENT_UPLOADS)
}

/**
 * The upload settings actually used, resolved against the server.
 *
 * This is the fix for "chunk size too large" on every file over
 * SIMPLE_UPLOAD_LIMIT: the client used to send a hard-coded 8 MB while the
 * server ships with a 5 MB limit, so each chunked upload was rejected at
 * `init` before a single byte was sent. Asking the server removes the class of
 * bug — it stops mattering what max_chunk_size is set to.
 *
 * Cached after the first call: the limits only change when an admin edits
 * settings, and a round trip per file would be wasteful.
 */
let limitsPromise = null

function fetchUploadLimits() {
  return fileApi.getUploadLimits().then((res) => {
    const max = Number(res?.data?.max_chunk_size)
    if (!(max > 0)) throw new Error('server reported no chunk limit')
    return {
      maxChunkSize: max,
      maxConcurrentUploads: normalizeConcurrentUploads(res?.data?.max_concurrent_uploads),
      maxParallelChunks: normalizeConcurrentUploads(res?.data?.max_parallel_chunks),
    }
  })
}

function resolveUploadLimits() {
  // Only a probe that actually succeeded is worth keeping.
  //
  // It used to cache whatever came back, failures included: one bad probe —
  // a tunnel hiccup, the server mid-restart — resolved to 0, which means "no
  // server limit", which resolves to MAX_CHUNK_BYTES. Against the shipped 5 MB
  // max_chunk_size that overshoots, so every subsequent large upload was
  // rejected at `init` with "chunk size too large", and nothing ever re-probed.
  // One blip at page load and chunked uploads stayed broken for the rest of
  // the session — reintroducing, permanently, the exact failure this function
  // exists to prevent.
  if (!limitsPromise) {
    limitsPromise = fetchUploadLimits().catch((err) => {
      // Forget the failure so the next upload probes again.
      limitsPromise = null
      throw err
    })
  }
  return limitsPromise
}

export async function resolveChunkSize(explicit) {
  if (typeof explicit === 'number' && explicit > 0) {
    return Math.min(explicit, MAX_CHUNK_BYTES)
  }

  let serverMax = 0
  try {
    serverMax = (await resolveUploadLimits()).maxChunkSize
  } catch {
    // Fall back to the client default. The server would reject it, but the user
    // gets a real error naming the size rather than a silent zero, and the
    // failed probe is not cached.
    serverMax = 0
  }

  if (!(serverMax > 0)) return MAX_CHUNK_BYTES
  return Math.min(serverMax, MAX_CHUNK_BYTES)
}

/**
 * How many files the next batch may upload in parallel.
 *
 * A failed probe is not fatal: the batch uses the default pool size, and the
 * failed probe is not cached, so the next batch asks again.
 */
export async function resolveConcurrentUploads() {
  try {
    return (await resolveUploadLimits()).maxConcurrentUploads
  } catch {
    return DEFAULT_CONCURRENT_UPLOADS
  }
}

/** Drops the cached limits; call after changing the chunked-upload settings. */
export function resetUploadLimits() {
  limitsPromise = null
}

/**
 * Default parallel chunks per file.
 * Kept as the fallback reported when the server cannot tell us its own value.
 */
const CHUNK_UPLOAD_CONCURRENCY = 3

/** How often a client asks whether background assembly is done. */
const COMPLETION_POLL_INTERVAL_MS = 2000

/**
 * Ceiling on background assembly polling. Assembly is bounded by disk speed,
 * not the network; an hour is generous even for 50 GB on a slow array, while
 * still giving up rather than polling a stuck session forever.
 */
const COMPLETION_TIMEOUT_MS = 60 * 60 * 1000

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/**
 * Max parallel chunks for a single file upload.
 * Fetched from server settings (Admin UI: Max Parallel Chunks).
 */
export async function resolveMaxParallelChunks() {
  try {
    return (await resolveUploadLimits()).maxParallelChunks
  } catch {
    return CHUNK_UPLOAD_CONCURRENCY
  }
}

/**
 * Largest file we will hash client-side.
 *
 * SubtleCrypto has no streaming digest — `digest()` takes the whole message at
 * once — so a checksum means the entire file as one ArrayBuffer. The comment on
 * this function used to claim it read in slices; it did not, and a 4 GB upload
 * would try to materialise 4 GB in a tab before sending a single chunk.
 *
 * Past the ceiling we send no checksum, which is fine: the server measures the
 * digest itself while it concatenates the chunks and stores that, so the
 * recorded checksum is the real one either way. The client value is only a
 * claim to check against, and there is no reason to pay for one we cannot
 * afford.
 */
export const CHECKSUM_LIMIT_BYTES = 256 * 1024 * 1024 // 256 MB

/**
 * Compute the SHA-256 of a File as a lowercase hex string, or '' if that is not
 * possible or not worth the memory.
 */
export async function sha256Hex(file) {
  if (!globalThis.crypto?.subtle) return ''
  if (file.size > CHECKSUM_LIMIT_BYTES) return ''
  try {
    const hash = await crypto.subtle.digest('SHA-256', await file.arrayBuffer())
    return Array.from(new Uint8Array(hash))
      .map((b) => b.toString(16).padStart(2, '0'))
      .join('')
  } catch {
    return ''
  }
}

/** Where a resumable upload's id is remembered between attempts. */
const TUS_STORAGE_PREFIX = 'yuarcloud:tus:'

/** PATCH retries before a dropped connection becomes a failure. */
const TUS_CHUNK_ATTEMPTS = 6

/**
 * Identifies "the same file going to the same place" across reloads.
 *
 * Name, size, last-modified and destination are what a tus server would use to
 * decide whether a stored upload can be resumed. Two genuinely different files
 * sharing all four are the same file for practical purposes; a file edited
 * between attempts has a new mtime and correctly starts over.
 */
function tusFingerprint(file, destination) {
  return `${TUS_STORAGE_PREFIX}${file.name}:${file.size}:${file.lastModified}:${destination ?? ''}`
}

/**
 * Encode Upload-Metadata's `key base64value` form.
 *
 * TextEncoder first: btoa alone only accepts Latin-1, so a filename with an
 * accent, an emoji or a non-Latin script would throw before the upload began.
 */
function encodeTusMetadata(values) {
  const encoder = new TextEncoder()
  return Object.entries(values)
    .filter(([, value]) => value !== undefined && value !== null && value !== '')
    .map(([key, value]) => {
      const bytes = encoder.encode(String(value))
      let binary = ''
      for (const byte of bytes) binary += String.fromCharCode(byte)
      return `${key} ${btoa(binary)}`
    })
    .join(',')
}

/** The upload id is the final path element of the Location header. */
function tusIDFromLocation(location) {
  if (!location) return null
  const path = String(location).split('?')[0].replace(/\/+$/, '')
  const id = path.slice(path.lastIndexOf('/') + 1)
  return id || null
}

/** Axios exposes header names case-insensitively; this normalises both shapes. */
function headerValue(headers, name) {
  if (!headers) return undefined
  const value = headers[name] ?? headers[name.toLowerCase()]
  return Array.isArray(value) ? value[0] : value
}

/**
 * Wait for the server to turn the received bytes into a file row.
 *
 * Finalisation is synchronous up to the async threshold and runs in the
 * background above it, so the last PATCH can answer before the file exists.
 * Holding at 99 while that happens is honest; 100 would claim a completion that
 * has not happened yet.
 */
async function waitForTusCompletion(uploadId, fileSize, signal, onProgress) {
  const started = Date.now()

  for (;;) {
    if (signal?.aborted) throw new Error('upload cancelled')

    const res = await fileApi.tusStatus(uploadId)
    const status = res?.data || {}

    if (status.status === 'completed') {
      if (status.file) return status.file
      throw new Error('upload finished but the server did not return the file record')
    }
    if (status.status === 'failed') {
      throw new Error(status.error || 'the server could not finalise the upload')
    }
    if (Date.now() - started > COMPLETION_TIMEOUT_MS) {
      throw new Error('the server is still finalising this upload; try again later')
    }

    onProgress?.(99, fileSize, fileSize)
    await sleep(COMPLETION_POLL_INTERVAL_MS)
  }
}

/**
 * Upload a large file over tus 1.0.
 *
 * tus is deliberately sequential: each PATCH starts where the previous one
 * ended, which is exactly what lets the server answer HEAD with a single offset
 * after a dropped connection. FileBrowser moves large files the same way, so
 * the throughput a 50 MB PATCH reaches on the same tunnel is the comparison
 * that matters. Parallel chunk requests were an optimisation of the old
 * protocol; they cannot be combined with one whose correctness is "one offset".
 *
 * Resume works in two layers. Inside one page session a failed PATCH retries
 * and asks HEAD where the server actually is, so a tunnel hiccup costs one
 * chunk rather than the file. Across a reload, the upload id is kept in
 * localStorage under a fingerprint of the file, and re-selecting the same file
 * continues from the server's offset instead of starting from zero.
 */
async function uploadFileTus(file, { destination, chunkSize, onProgress, signal }) {
  const resolvedChunkSize = await resolveChunkSize(chunkSize)
  const fingerprint = tusFingerprint(file, destination)

  let uploadId = null
  let offset = 0

  const remembered = localStorage.getItem(fingerprint)
  if (remembered) {
    try {
      const head = await fileApi.tusHead(remembered)
      const status = String(headerValue(head.headers, 'x-yuarcloud-status') || '')
      const total = Number(headerValue(head.headers, 'upload-length'))
      const serverOffset = Number(headerValue(head.headers, 'upload-offset'))
      // Only a live session is resumable. "completed" already produced its
      // file, and "failed" can never be patched again (the server answers 410),
      // so reusing either id would turn every retry into the same error. Both
      // fall through to a fresh creation.
      const resumable = status === 'uploading' || status === 'finalizing'
      if (resumable && total === file.size && Number.isFinite(serverOffset) && serverOffset >= 0) {
        uploadId = remembered
        offset = Math.min(serverOffset, file.size)
      }
    } catch {
      // Expired or swept: fall through to a fresh creation.
    }
    if (!uploadId) localStorage.removeItem(fingerprint)
  }

  if (!uploadId) {
    const metadata = encodeTusMetadata({
      filename: file.name,
      parent_id: destination ?? '',
      checksum: await sha256Hex(file),
    })
    const created = await fileApi.tusCreate({ size: file.size, metadata })
    uploadId = tusIDFromLocation(headerValue(created.headers, 'location'))
    if (!uploadId) throw new Error('the server did not return an upload location')
    localStorage.setItem(fingerprint, uploadId)
    offset = 0
  }

  if (offset > 0) {
    // Resuming: show what the server already has rather than restarting at 0.
    // The same offset is the first acknowledged data point for the speed
    // tracker, so a resumed upload is not measured as if it began at zero.
    onProgress?.(Math.min(99, Math.round((offset * 100) / file.size)), offset, offset)
  }

  try {
    let attempt = 0
    while (offset < file.size) {
      if (signal?.aborted) throw new Error('upload cancelled')

      const start = offset
      const end = Math.min(start + resolvedChunkSize, file.size)
      // The third argument makes the Blob itself carry tus' media type. The
      // request header says the same thing, but axios' Node adapter overwrites
      // Content-Type with blob.type for any Blob it sends, so a slice left with
      // the picked file's own MIME type would be rejected as 415 there — and a
      // browser extension or proxy that rebuilds the request would hit the same
      // wall. Both sources now agree.
      const blob = file.slice(start, end, 'application/offset+octet-stream')

      try {
        const response = await fileApi.tusPatch(uploadId, start, blob, (loaded) => {
          const capped = Math.min(Math.max(Number(loaded) || 0, 0), blob.size)
          const overall = Math.min(file.size, start + capped)
          // The third argument is the last offset the server acknowledged, not
          // the bytes counted here. Those are still only in the socket buffer;
          // the tracker measures a tus upload from what the server has taken.
          onProgress?.(Math.min(99, Math.round((overall * 100) / file.size)), overall, start)
        })
        const next = Number(headerValue(response.headers, 'upload-offset'))
        if (!Number.isFinite(next) || next <= start) {
          throw new Error('the server did not advance the upload offset')
        }
        offset = Math.min(next, file.size)
        attempt = 0
        // Record the acknowledgement now that the response is in hand, so the
        // reported speed and remaining time are based on bytes that actually
        // arrived rather than on how fast the browser could hand them over.
        onProgress?.(Math.min(99, Math.round((offset * 100) / file.size)), offset, offset)
      } catch (error) {
        if (signal?.aborted) throw new Error('upload cancelled')

        const status = error?.response?.status
        // 409 is the protocol's "your offset is stale": recover by asking
        // HEAD. Any other 4xx means the session itself is wrong.
        if (status && status >= 400 && status < 500 && status !== 409) throw error

        attempt += 1
        if (attempt > TUS_CHUNK_ATTEMPTS) throw error
        await sleep(500 * 2 ** (attempt - 1))

        try {
          const head = await fileApi.tusHead(uploadId)
          const serverOffset = Number(headerValue(head.headers, 'upload-offset'))
          if (Number.isFinite(serverOffset) && serverOffset >= 0) {
            offset = Math.min(serverOffset, file.size)
          }
        } catch {
          // Keep the last known offset and retry the PATCH.
        }
      }
    }

    onProgress?.(99, file.size, file.size)
    const record = await waitForTusCompletion(uploadId, file.size, signal, onProgress)
    localStorage.removeItem(fingerprint)
    onProgress?.(100, file.size, file.size)
    return record
  } catch (error) {
    if (signal?.aborted) {
      // The user cancelled: they do not intend to resume, so reclaim the disk
      // now instead of waiting for the sweep interval.
      try {
        await fileApi.tusDelete(uploadId)
      } catch {
        /* the sweeper will reclaim it */
      }
      localStorage.removeItem(fingerprint)
    }
    throw error
  }
}

/**
 * Upload one file, using chunks when it is large.
 *
 * @param {File} file
 * @param {object} options
 * @param {string|null} options.parentId      destination folder id
 * @param {(file:File)=>string|null} [options.parentIdFor]  per-file destination,
 *        used by a folder pick where each file has its own place in the tree
 * @param {number} [options.chunkSize]        override; still clamped to the server
 * @param {(pct:number, loaded:number, confirmed?:number)=>void} options.onProgress  overall progress 0-100,
 *        plus the bytes actually sent so far — the percentage alone cannot say
 *        whether a slow transfer is moving — and, for a resumable upload, the
 *        offset the server has acknowledged, which is the only honest basis for
 *        a speed reading
 * @param {(msg:string)=>void} options.onError
 * @param {AbortSignal} [options.signal]
 * @returns {Promise<object>} the created file record
 */
export async function uploadFileChunked(file, options = {}) {
  const {
    parentId = null,
    parentIdFor = null,
    chunkSize,
    onProgress,
    onError,
    signal,
  } = options

  // A folder pick puts each file in a different destination, so the id is
  // resolved per file rather than passed once for the batch.
  const destination = typeof parentIdFor === 'function' ? parentIdFor(file) : parentId

  const useSimple = file.size <= SIMPLE_UPLOAD_LIMIT

  if (useSimple) {
    const form = new FormData()
    form.append('file', file, file.name)
    if (destination) form.append('parent_id', destination)

    try {
      const response = await fileApi.upload(form, ({ pct, loaded }) => onProgress?.(pct, loaded))
      return response.data
    } catch (error) {
      // The chunked path already turns its failures into a sentence. The simple
      // path rethrew the raw axios error, so the tray showed
      // "timeout of 30000ms exceeded" or "Network Error" while a human message
      // was one call away. It also left the row on "uploading" until the catch
      // in uploadFilesWithConcurrency happened to fire.
      const message = apiError(error, `Failed to upload ${file.name}`)
      onError?.(message)
      throw new Error(message)
    }
  }

  return uploadFileTus(file, { destination, chunkSize, onProgress, signal }).catch((error) => {
    const message = apiError(error, `Failed to upload ${file.name}`)
    onError?.(message)
    throw new Error(message)
  })
}

/**
 * Upload a list of files with a bounded number in flight.
 *
 * A batch used to run strictly one file at a time. That is safe, but ten small
 * files spent ten round trips waiting on each other, and the tray looked frozen
 * between rows. Sending everything at once is worse: ten large files compete
 * for the same uplink and a flaky tunnel can fail all of them together. The
 * worker pool keeps the middle ground, with the limit chosen by an admin (and
 * reported by /upload-limits) rather than hard-coded here.
 *
 * Each file's chunks still go out in order. Concurrency is across files, so a
 * single large file keeps its simple, resumable chunk sequence.
 *
 * @param {File[]} files
 * @param {object} options
 * @param {number} [options.concurrency]  files to run at once; clamped to 1..10
 * @param {(update:{file:File,index:number,progress:number,loaded:number,status:string})=>void} [options.onUpdate]
 */
export async function uploadFilesWithConcurrency(files, options = {}) {
  const {
    parentId = null,
    parentIdFor = null,
    chunkSize,
    concurrency = DEFAULT_CONCURRENT_UPLOADS,
    onUpdate,
    signal,
  } = options
  const results = []
  const failures = []

  if (!files.length) return { results, failures }

  const workers = Math.min(
    files.length,
    Math.max(1, Math.min(MAX_CONCURRENT_UPLOADS, Math.floor(concurrency) || DEFAULT_CONCURRENT_UPLOADS))
  )
  let nextIndex = 0

  const worker = async () => {
    while (nextIndex < files.length) {
      if (signal?.aborted) return
      const index = nextIndex
      nextIndex += 1
      const file = files[index]

      onUpdate?.({ file, index, progress: 0, status: 'uploading' })

      try {
        const record = await uploadFileChunked(file, {
          parentId,
          parentIdFor,
          chunkSize,
          signal,
          onProgress: (progress, loaded, confirmed) =>
            onUpdate?.({ file, index, progress, loaded, confirmed, status: 'uploading' }),
          onError: (message) => onUpdate?.({ file, index, progress: 0, status: 'error', message }),
        })
        // The index travels with the result. Callers used to match on file.name,
        // which cannot distinguish two files that share a basename — routine for
        // a folder pick containing a/report.doc and b/report.doc.
        results.push({ file, index, record })
        onUpdate?.({ file, index, progress: 100, loaded: file.size, status: 'done' })
      } catch (error) {
        failures.push({ file, index, error })
        // The chunked path reports its own failures through onError; the simple
        // path did not, so its row stayed on "uploading" with a frozen progress
        // bar until the panel was closed.
        onUpdate?.({
          file,
          index,
          progress: 0,
          status: 'error',
          message: error?.message || 'Upload failed',
        })
      }
    }
  }

  // Completion order is not stable under concurrency, but callers and the
  // receipt should be: the batch is reported in the order the files were
  // picked.
  await Promise.all(Array.from({ length: workers }, () => worker()))
  results.sort((a, b) => a.index - b.index)
  failures.sort((a, b) => a.index - b.index)

  return { results, failures }
}
