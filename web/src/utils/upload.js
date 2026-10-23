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
 * Hard client-side ceiling per chunk, regardless of what the server allows.
 *
 * The server's max_chunk_size is the tunable knob, but it can be set above
 * what Cloudflare will accept in one request — so this stays as a cap on our
 * own choice and we take whichever of the two is smaller.
 */
export const MAX_CHUNK_BYTES = 10 * 1024 * 1024 // 10 MB

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

/**
 * Send one chunk, retrying a dropped request.
 *
 * A chunk crossing a Cloudflare tunnel or a phone hotspot fails for reasons
 * that have nothing to do with the bytes — and one failure used to abort the
 * whole file. Retrying is safe by construction: the server records chunk
 * indices in a set, so a repeat overwrites in place and reports that it was a
 * duplicate rather than inflating the counter. That is exactly the case the
 * duplicate handling exists for.
 *
 * Deliberately not retried: 4xx. A rejected chunk means the session itself is
 * wrong, and hammering it just delays the real error.
 *
 * `onBytes` is forwarded so the chunk being sent reports progress continuously.
 * Without it a chunked upload only moved its bar once per chunk, every few
 * seconds — coarse enough that neither the bar nor any speed derived from it
 * could describe what was actually happening.
 */
async function sendChunkWithRetry(uploadId, index, blob, signal, onBytes) {
  const MAX_ATTEMPTS = 4
  let lastError

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt += 1) {
    if (signal?.aborted) throw new Error('upload cancelled')
    try {
      return await fileApi.uploadChunk(uploadId, index, blob, onBytes)
    } catch (error) {
      lastError = error
      const status = error?.response?.status
      if (status && status >= 400 && status < 500) throw error
      if (attempt === MAX_ATTEMPTS) break
      // Short, growing backoff: long enough for a dropped connection to come
      // back, short enough that four attempts still feel immediate.
      await new Promise((r) => setTimeout(r, 400 * 2 ** (attempt - 1)))
    }
  }

  throw lastError
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
 * @param {(pct:number, loaded:number)=>void} options.onProgress  overall progress 0-100,
 *        plus the bytes actually sent so far — the percentage alone cannot say
 *        whether a slow transfer is moving
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

    const response = await fileApi.upload(form, ({ pct, loaded }) => onProgress?.(pct, loaded))
    return response.data
  }

  const resolvedChunkSize = await resolveChunkSize(chunkSize)
  const totalChunks = Math.ceil(file.size / resolvedChunkSize)
  const checksum = await sha256Hex(file)

  const init = await fileApi.initChunked({
    filename: file.name,
    total_size: file.size,
    chunk_size: resolvedChunkSize,
    total_chunks: totalChunks,
    checksum,
    parent_id: destination ?? undefined,
  })
  const uploadId = init.data.upload_id

  // Bytes already committed by finished chunks. Progress inside the chunk in
  // flight is added on top of this, so the reported count is always "sent so
  // far across the whole file" rather than a position that resets each chunk.
  let committed = 0

  try {
    for (let index = 0; index < totalChunks; index += 1) {
      if (signal?.aborted) throw new Error('upload cancelled')

      const start = index * resolvedChunkSize
      const end = Math.min(start + resolvedChunkSize, file.size)
      const blob = file.slice(start, end)

      await sendChunkWithRetry(uploadId, index, blob, signal, ({ loaded }) => {
        const sent = committed + Math.min(loaded, blob.size)
        onProgress?.(Math.round((sent * 100) / file.size), sent)
      })

      // Progress across the whole file, not just the current chunk.
      committed = Math.min(end, file.size)
      onProgress?.(Math.round((committed * 100) / file.size), committed)
    }

    const done = await fileApi.completeChunked(uploadId)
    onProgress?.(100, file.size)
    return done.data
  } catch (error) {
    // Best-effort cleanup so temp chunks are not left behind.
    try {
      await fileApi.cancelChunked(uploadId)
    } catch {
      /* the server's expiry sweep will reclaim it */
    }
    const message = apiError(error, `Failed to upload ${file.name}`)
    onError?.(message)
    throw new Error(message)
  }
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
          onProgress: (progress, loaded) => onUpdate?.({ file, index, progress, loaded, status: 'uploading' }),
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
