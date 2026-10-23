// API surface for Yuarcloud. Every call goes through the shared axios instance
// so the auth header and token-refresh interceptor are applied consistently.

import axios from 'axios'

const api = axios.create({
  baseURL: '/api',
  timeout: 30000,
  withCredentials: false,
})

// Read fresh on every call rather than caching: the refresh interceptor can
// rotate the access token at any time, and a preview URL built from a stale
// one would 401 the moment the media element asked for bytes.
const getToken = () => localStorage.getItem('access_token')

api.interceptors.request.use((config) => {
  const token = getToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// One refresh at a time, shared by everything that 401s while it runs.
//
// A drive screen fires several requests at once — the listing, the trash
// set, a preview token. When the access token has expired they all come back
// 401 at the same moment, and each one used to POST /auth/refresh
// independently. That is several near-identical round trips, several writes to
// localStorage racing each other, and — with server-side revocation in play —
// several chances for one of them to present a token another just replaced.
// Deduplicating makes it one.
let refreshInFlight = null

function refreshSession() {
  if (!refreshInFlight) {
    refreshInFlight = axios
      .post('/api/auth/refresh', { refresh_token: localStorage.getItem('refresh_token') })
      .then((response) => {
        const { access_token, refresh_token } = response.data.tokens
        localStorage.setItem('access_token', access_token)
        localStorage.setItem('refresh_token', refresh_token)
        return access_token
      })
      .finally(() => {
        refreshInFlight = null
      })
  }
  return refreshInFlight
}

api.interceptors.response.use(
  (response) => response,
  async (error) => {
    const original = error.config

    // A request that never reached the server — DNS failure, CORS rejection, an
    // aborted upload — has no config, and no response either. Reading
    // `original._retried` on it threw inside the interceptor, replacing the real
    // network error with "Cannot read properties of undefined". Every caller
    // then reported a generic message for something it could have described
    // exactly. Nothing below needs to run without a config, so bail first.
    if (!original) return Promise.reject(error)

    // Public share endpoints answer 401 to mean "this link needs a password".
    // That is not an expired session, so never redirect away from a share page.
    // DELETE /shares/:id is an owner action, not a public one: matching on the
    // path alone swallowed its expired-token 401, which then surfaced as a
    // failed delete instead of a silent refresh.
    const method = (original.method || 'get').toLowerCase()
    const isPublicShare =
      original.url?.startsWith('/shares/') && (method === 'get' || method === 'post')

    if (error.response?.status === 401 && !original._retried && !isPublicShare) {
      original._retried = true
      const refreshToken = localStorage.getItem('refresh_token')

      if (refreshToken) {
        try {
          const accessToken = await refreshSession()
          original.headers = original.headers || {}
          original.headers.Authorization = `Bearer ${accessToken}`
          return api(original)
        } catch {
          // Refresh failed — the session is gone.
          clearSession()
        }
      } else {
        clearSession()
      }
    }

    return Promise.reject(error)
  }
)

function clearSession() {
  localStorage.removeItem('access_token')
  localStorage.removeItem('refresh_token')
  // A visitor on a public share link has no session to lose; leave them be.
  // The legacy /s/ prefix is still reachable via the redirect in App.jsx, so
  // both forms are spared or a visitor following an old link gets bounced.
  if (
    !location.pathname.startsWith('/login') &&
    !location.pathname.startsWith('/yc/') &&
    !location.pathname.startsWith('/s/')
  ) {
    location.href = '/login'
  }
}

/** Pull a human-readable message out of an axios error. */
export function apiError(error, fallback = 'Something went wrong') {
  // A timeout and an offline browser both arrive as an axios Error with no
  // response. Its `message` is accurate ("timeout of 30000ms exceeded",
  // "Network Error") but reads like a developer string in a toast, so name the
  // two cases the user can act on and keep the rest as the fallback.
  if (!error?.response) {
    if (error?.code === 'ECONNABORTED' || /timeout/i.test(error?.message || '')) {
      return 'The server took too long to respond.'
    }
    if (/network|failed to fetch/i.test(error?.message || '')) {
      return 'Could not reach the server.'
    }
  }
  return (
    error?.response?.data?.error ||
    error?.message ||
    fallback
  )
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

export const authApi = {
  login: (username, password) => api.post('/auth/login', { username, password }),
  
  me: () => api.get('/auth/me'),
  changePassword: (data) => api.post('/auth/change-password', data),
  logout: () => api.post('/auth/logout'),
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

export const fileApi = {
  list: (params = {}) => api.get('/files', { params }),
  get: (id) => api.get(`/files/${id}`),
  createFolder: (data) => api.post('/files/folder', data),
  // Reports the byte count alongside the percentage. The percentage alone was
  // enough to draw the bar and not enough to answer "is this stuck?", which is
  // the question an upload panel invites.
  upload: (formData, onProgress) =>
    api.post('/files/upload', formData, {
      headers: { 'Content-Type': 'multipart/form-data' },
      onUploadProgress: (e) => {
        if (onProgress && e.total) onProgress({ pct: Math.round((e.loaded * 100) / e.total), loaded: e.loaded })
      },
    }),
  remove: (id, permanent = false) => api.delete(`/files/${id}`, { params: { permanent } }),
  rename: (id, name) => api.patch(`/files/${id}/rename`, { name }),
  move: (id, data) => api.post(`/files/${id}/move`, data),
  copy: (id, data) => api.post(`/files/${id}/copy`, data),

  downloadUrl: (id) => `/api/files/${id}/download`,
  downloadZipUrl: (id) => `/api/files/${id}/download-zip`,

  // Asks for the short-lived token the viewer puts in a media URL. The session
  // access token never leaves a header — this one lives five minutes, is
  // pinned to this one file, and is rejected by every other endpoint.
  previewToken: (id) => api.post(`/files/${id}/preview-token`),

  // Ceilings the uploader has to respect. Read once and cached by upload.js:
  // the admin can change max_chunk_size or the parallel-upload count at
  // runtime, and a client that guessed its own values would start failing
  // uploads or run a different pool size as soon as it did.
  getUploadLimits: () => api.get('/files/upload-limits'),

  // Inline streaming URL for <img>/<video>/<audio>/<iframe>. Takes the file
  // record, not the id — PreviewModal hands one straight through.
  previewUrl: (item, previewToken) =>
    `/api/files/${item.id}/preview?token=${encodeURIComponent(previewToken || '')}`,

  // Same endpoint, but without the query token, for the previewers that parse
  // in the browser (text, xlsx, docx) and therefore fetch with a header anyway.
  previewBlobUrl: (item) => `/api/files/${item.id}/preview`,


  // Trash
  listTrash: () => api.get('/trash'),
  emptyTrash: () => api.post('/trash/empty'),
  trash: (id) => api.post(`/files/${id}/trash`),
  restore: (id) => api.post(`/files/${id}/restore`),
  deletePermanent: (id) => api.delete(`/files/${id}/permanent`),

  // Views
  search: (q, limit = 100) => api.get('/files/search', { params: { q, limit } }),
  shared: () => api.get('/files/shared'),
  stats: () => api.get('/files/stats'),
  folderTree: () => api.get('/files/folders/tree'),
  folderFlat: () => api.get('/files/folders/flat'),

  // Chunked upload
  initChunked: (data) => api.post('/files/upload/chunked/init', data),
  uploadChunk: (uploadId, index, blob, onProgress) => {
    const form = new FormData()
    form.append('chunk', blob, `chunk_${index}`)
    return api.post(`/files/upload/chunked/${uploadId}/chunk/${index}`, form, {
      headers: { 'Content-Type': 'multipart/form-data' },
      onUploadProgress: (e) => {
        if (onProgress && e.total) onProgress({ pct: Math.round((e.loaded * 100) / e.total), loaded: e.loaded })
      },
    })
  },
  completeChunked: (uploadId) => api.post(`/files/upload/chunked/${uploadId}/complete`),
  chunkStatus: (uploadId) => api.get(`/files/upload/chunked/${uploadId}/status`),
  cancelChunked: (uploadId) => api.delete(`/files/upload/chunked/${uploadId}`),
}

// ---------------------------------------------------------------------------
// Shares
// ---------------------------------------------------------------------------

export const shareApi = {
  create: (data) => api.post('/shares', data),
  // fileId narrows the list server-side; without it a user with many links
  // would silently miss the older ones past the page size.
  list: (fileId) => api.get('/shares', { params: fileId ? { file_id: fileId } : {} }),
  remove: (id) => api.delete(`/shares/${id}`),
  access: (token, password) => api.get(`/shares/${token}`, { params: { password } }),
  // Direct children of the shared folder, or of a folder inside it. Public:
  // the token in the path is the credential, same as download and preview.
  children: (token, password, path) =>
    api.get(`/shares/${token}/children`, { params: { password, path } }),
  // `path` selects a row inside a browsed folder; omitted, the shared item
  // itself is served.
  downloadUrl: (token, password, path) => {
    const query = new URLSearchParams({ password: password || '' })
    if (path) query.set('path', path)
    return `/api/shares/${token}/download?${query}`
  },
  // Inline streaming URL for the share page's viewer. Public: the share token
  // in the path is the credential, which is also how the download link works.
  previewUrl: (token, password, path) => {
    const query = new URLSearchParams({ password: password || '' })
    if (path) query.set('path', path)
    return `/api/shares/${token}/preview?${query}`
  },
  // Public upload into a write-enabled shared folder.
  upload: (token, formData, password, onProgress) =>
    api.post(`/shares/${token}/upload`, formData, {
      headers: { 'Content-Type': 'multipart/form-data' },
      params: { password },
      onUploadProgress: (e) => {
        if (onProgress && e.total) onProgress(Math.round((e.loaded * 100) / e.total))
      },
    }),
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

export const adminApi = {
  getStats: () => api.get('/admin/stats'),
  getUsers: (params = {}) => api.get('/admin/users', { params }),
  getUser: (id) => api.get(`/admin/users/${id}`),
  createUser: (data) => api.post('/admin/users', data),
  updateUser: (id, data) => api.patch(`/admin/users/${id}`, data),
  deleteUser: (id) => api.delete(`/admin/users/${id}`),
  updateUserQuota: (id, quotaBytes) => api.patch(`/admin/users/${id}/quota`, { quota_bytes: quotaBytes }),
  updateUserRole: (id, role) => api.patch(`/admin/users/${id}/role`, { role }),
  // These take a plain object of query params. Passing a pre-built query string
  // throws "target must be an object" inside axios, which is how the audit tab
  // managed to render empty for so long — it only ever logged to the console.
  getAuditLogs: (params = {}) => api.get('/admin/audit-logs', { params }),
  getShares: (params = {}) => api.get('/admin/shares', { params }),
  deleteShare: (id) => api.delete(`/admin/shares/${id}`),
  getChunkedConfig: () => api.get('/admin/chunked-config'),
  updateChunkedConfig: (data) => api.post('/admin/chunked-config', data),
}

export default api
