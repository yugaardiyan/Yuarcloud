/**
 * Chunked upload for a public share link.
 *
 * The signed-in uploader talks to /files/... and learns its ceilings from the
 * authenticated limits endpoint. A visitor has neither an account nor that
 * endpoint, so this is the same protocol against the share-scoped routes: a
 * small file still goes in one multipart POST, anything larger is split so it
 * stays inside Cloudflare's request-body and request-time limits.
 */

import { shareApi, apiError } from './api'

/** Files at or below this still go in one POST, matching the signed-in path. */
const SIMPLE_UPLOAD_LIMIT = 16 * 1024 * 1024

/**
 * Chunk size to use when the server's ceiling is unknown.
 *
 * The chunked protocol fixes the size at init — every chunk index maps to a
 * fixed offset — so it cannot adapt mid-flight the way tus does. Fitting the
 * proxy's ~100 second window on a slow phone uplink matters more than
 * minimising request count: 4 MB stays inside it down to roughly 0.3 Mbps.
 */
const DEFAULT_CHUNK_BYTES = 4 * 1024 * 1024

/** Attempts for one chunk before the upload is reported as failed. */
const CHUNK_ATTEMPTS = 6

/** How often a client asks whether background assembly is done. */
const COMPLETION_POLL_INTERVAL_MS = 2000

/** Ceiling on background assembly polling; see the signed-in uploader. */
const COMPLETION_TIMEOUT_MS = 60 * 60 * 1000

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

async function resolveChunkSize(token, password) {
  try {
    const res = await shareApi.uploadLimits(token, password)
    const max = Number(res?.data?.max_chunk_size)
    // The server's value is a ceiling, not a target. Capping at the 4 MB
    // default keeps a chunk inside the proxy's window on a slow uplink; an
    // admin who configured something smaller still wins.
    if (max > 0) return Math.max(1, Math.min(max, DEFAULT_CHUNK_BYTES))
  } catch {
    // The link may still upload; fall through to the conservative default. A
    // chunk the server considers too large comes back as a clear 4xx, so the
    // visitor sees a message rather than a silent stall.
  }
  return DEFAULT_CHUNK_BYTES
}

async function waitForCompletion(token, password, uploadId, signal, onProgress) {
  const started = Date.now()

  for (;;) {
    if (signal?.aborted) throw new Error('upload cancelled')

    const res = await shareApi.uploadStatus(token, password, uploadId)
    const status = res?.data || {}

    if (status.status === 'completed') return status.file || null
    if (status.status === 'failed') {
      throw new Error(status.error || 'the server could not finalise the upload')
    }
    if (Date.now() - started > COMPLETION_TIMEOUT_MS) {
      throw new Error('the server is still finalising this upload; try again later')
    }

    onProgress?.(99)
    await sleep(COMPLETION_POLL_INTERVAL_MS)
  }
}

/**
 * Upload one file to a write-enabled share.
 *
 * @param {string} token
 * @param {string} password   empty when the link has none
 * @param {File} file
 * @param {object} options
 * @param {string|null} [options.parentId]  shared folder the file belongs to
 * @param {(pct:number)=>void} [options.onProgress]  overall progress 0-100
 * @param {AbortSignal} [options.signal]
 * @returns {Promise<object|null>} the created file record, when the server
 *          returns one (background assembly reports it through status)
 */
export async function uploadToShare(token, password, file, options = {}) {
  const { parentId = null, onProgress, signal } = options

  if (file.size <= SIMPLE_UPLOAD_LIMIT) {
    const form = new FormData()
    form.append('file', file, file.name)
    const res = await shareApi.upload(token, form, password, (pct) => onProgress?.(pct))
    return res.data
  }

  const chunkSize = await resolveChunkSize(token, password)
  const totalChunks = Math.ceil(file.size / chunkSize)

  const init = await shareApi.uploadInit(token, password, {
    filename: file.name,
    total_size: file.size,
    chunk_size: chunkSize,
    total_chunks: totalChunks,
    parent_id: parentId,
  })
  const uploadId = init?.data?.upload_id
  if (!uploadId) throw new Error('the server did not return an upload id')

  try {
    for (let index = 0; index < totalChunks; index++) {
      if (signal?.aborted) throw new Error('upload cancelled')

      const start = index * chunkSize
      const end = Math.min(start + chunkSize, file.size)
      const blob = file.slice(start, end)

      let attempt = 0
      for (;;) {
        const form = new FormData()
        form.append('chunk', blob, file.name)
        try {
          await shareApi.uploadChunk(
            token, password, uploadId, index, form,
            (pct) => {
              const sent = start + Math.round((end - start) * (pct / 100))
              onProgress?.(Math.min(99, Math.round((sent * 100) / file.size)))
            },
            signal,
          )
          break
        } catch (error) {
          if (signal?.aborted) throw new Error('upload cancelled')
          const status = error?.response?.status
          // A 4xx says the session or the chunk itself is wrong; resending the
          // same bytes cannot fix it. Network errors, timeouts and 5xx are the
          // ones worth another attempt.
          if (status && status >= 400 && status < 500) throw error
          attempt += 1
          if (attempt >= CHUNK_ATTEMPTS) throw error
          await sleep(500 * 2 ** (attempt - 1))
        }
      }

      onProgress?.(Math.min(99, Math.round((end * 100) / file.size)))
    }

    onProgress?.(99)
    const completed = await shareApi.uploadComplete(token, password, uploadId)
    if (completed?.status === 202 || completed?.data?.status === 'completing') {
      const record = await waitForCompletion(token, password, uploadId, signal, onProgress)
      onProgress?.(100)
      return record
    }
    onProgress?.(100)
    return completed.data
  } catch (error) {
    if (signal?.aborted) {
      try {
        await shareApi.uploadCancel(token, password, uploadId)
      } catch {
        // The sweeper reclaims an abandoned session on its own.
      }
    }
    throw new Error(apiError(error, `Failed to upload ${file.name}`))
  }
}