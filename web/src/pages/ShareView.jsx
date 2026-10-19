import React, { useState, useEffect, useCallback, useRef } from 'react'
import { useParams } from 'react-router-dom'
import { shareApi, apiError } from '../utils/api'
import { Download, Lock, AlertCircle, CheckCircle, Copy, FolderOpen, Upload, Eye, ChevronLeft, ChevronRight } from 'lucide-react'
import { formatBytes, formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from '../components/FileIcon'
import PreviewModal from '../components/PreviewModal'
import { canPreview } from '../utils/fileType'
import Logo from '../components/Logo'
import toast from 'react-hot-toast'

/** The mark and wordmark, plain. The header's bordered capsule carries a
 *  shadow and a 46px height that dwarf a single card, so the share page uses
 *  the same two-tone lockup without the chrome. */
function ShareBrand() {
  return (
    <div className="share-brand">
      <Logo size={22} />
      <span className="share-brand-name">
        Yuar<span className="brand-name-tail">cloud</span>
      </span>
    </div>
  )
}

// A folder listing renders every row it is handed. A share of a folder with
// tens of thousands of entries would put that many buttons in the DOM at once,
// so the view stops early and says so rather than locking the tab up. The
// count in the header still reports the real total.
const CONTENT_RENDER_CAP = 500

/** Public landing page for a shared file. No authentication required. */
export default function ShareView() {
  const { token } = useParams()
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const [share, setShare] = useState(null)
  const [file, setFile] = useState(null)
  const [password, setPassword] = useState('')
  const [needsPassword, setNeedsPassword] = useState(false)
  const [passwordError, setPasswordError] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  // uploads: [{ id, name, progress, status: 'uploading'|'done'|'error', error? }]
  const [uploads, setUploads] = useState([])
  const [dragOver, setDragOver] = useState(false)
  // File shown in the viewer. The share root and any row inside a browsed
  // folder both land here; the URL is built from .path, not from a boolean.
  const [previewFile, setPreviewFile] = useState(null)
  // Folder browsing. `trail` is the path walked from the shared root, so the
  // back button is a matter of dropping the last entry, and the breadcrumb
  // never has to be recomputed from strings.
  const [contents, setContents] = useState([])
  const [trail, setTrail] = useState([])
  const [contentsLoading, setContentsLoading] = useState(false)
  const [contentsError, setContentsError] = useState(null)
  const [showContents, setShowContents] = useState(true)
  const contentsSeq = useRef(0)
  const fileInputRef = useRef(null)

  // Discards a response that arrives after a newer request went out.
  //
  // Typing a wrong password and immediately typing the right one fires two
  // requests; if the first came back last, it won the race and overwrote the
  // good result with the failure — the page reported "wrong password" for a
  // password that worked. The counter is the same shape as the one in
  // FileContext.
  const requestId = useRef(0)

  const load = useCallback(
    async (pwd = '') => {
      const id = ++requestId.current
      setLoading(true)
      try {
        const response = await shareApi.access(token, pwd)
        if (id !== requestId.current) return
        setShare(response.data.share)
        setFile(response.data.file)
        setError(null)
        setNeedsPassword(false)
        setPasswordError(false)
      } catch (err) {
        if (id !== requestId.current) return
        if (err.response?.status === 401) {
          setNeedsPassword(true)
          setError('This link is password protected.')
        } else if (err.response?.status === 410) {
          setError('This share link has expired.')
        } else if (err.response?.status === 403) {
          setError('This share link is no longer available.')
        } else {
          setError(apiError(err, 'This share link could not be opened.'))
        }
      } finally {
        if (id === requestId.current) setLoading(false)
      }
    },
    [token]
  )

  useEffect(() => { load() }, [load])

  const submitPassword = async (e) => {
    e.preventDefault()
    setSubmitting(true)
    try {
      await load(password)
    } finally {
      setSubmitting(false)
    }
  }

  // Folders stream from the server as a ZIP, so the anchor's download name is
  // only a hint — the Content-Disposition header decides the final filename.
  // A target can be any row in a browsed folder; its path is what the public
  // endpoint resolves against the shared root.
  const handleDownload = (target) => {
    const item = target || file
    if (!item) return
    const link = document.createElement('a')
    link.href = shareApi.downloadUrl(token, password, item.path)
    link.download = item.is_dir ? `${item.name}.zip` : item.name
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
  }

  // Content requests race like access requests: opening one folder and
  // immediately opening another must not let the first response paint the
  // second folder's list. Same counter pattern as `requestId` above.
  const openFolder = useCallback(
    async (path, name) => {
      const id = ++contentsSeq.current
      setContentsLoading(true)
      setContentsError(null)
      setTrail((prev) => {
        const at = prev.findIndex((entry) => entry.path === path)
        return at >= 0 ? prev.slice(0, at + 1) : [...prev, { path, name }]
      })
      try {
        const response = await shareApi.children(token, password, path)
        if (id !== contentsSeq.current) return
        setContents(response.data.items || [])
      } catch (err) {
        if (id !== contentsSeq.current) return
        setContentsError(apiError(err, 'Could not load this folder'))
      } finally {
        if (id === contentsSeq.current) setContentsLoading(false)
      }
    },
    [token, password]
  )

  // The shared root is the first folder every visitor sees. Running this on
  // the file id (not on every render) keeps a password change, which produces
  // a new file object, from re-fetching contents twice.
  useEffect(() => {
    if (!file?.is_dir) return
    setContents([])
    setTrail([])
    setShowContents(true)
    openFolder(file.path, file.name)
  }, [file?.id, file?.path, file?.name, file?.is_dir, openFolder])

  const goBack = () => {
    if (trail.length < 2) return
    const parent = trail[trail.length - 2]
    openFolder(parent.path, parent.name)
  }

  // A folder row walks in; a file opens the viewer when it can be rendered,
  // and downloads otherwise. Only files small enough for the viewer qualify,
  // which canPreview already encodes.
  const openEntry = (item) => {
    if (item.is_dir) {
      openFolder(item.path, item.name)
      return
    }
    if (canPreview(item)) {
      setPreviewFile(item)
      return
    }
    handleDownload(item)
  }

  const copyLink = async () => {
    try {
      await navigator.clipboard.writeText(window.location.href)
      toast.success('Link copied')
    } catch {
      toast.error('Could not copy — select the address bar instead')
    }
  }

  const canUpload = !!file && !!file.is_dir && share?.permission === 'write'

  const updateUpload = (id, patch) =>
    setUploads((prev) => prev.map((u) => (u.id === id ? { ...u, ...patch } : u)))

  const handleUploadFiles = async (picked) => {
    if (!picked || !picked.length) return
    // Queue each file as a row so the visitor sees per-file progress.
    const items = Array.from(picked).map((f) => ({
      id: `${f.name}-${f.size}-${Math.random().toString(36).slice(2, 8)}`,
      name: f.name,
      progress: 0,
      status: 'uploading',
    }))
    setUploads((prev) => [...prev, ...items])

    for (let i = 0; i < picked.length; i++) {
      const file = picked[i]
      const meta = items[i]
      const form = new FormData()
      form.append('file', file)
      try {
        await shareApi.upload(token, form, password, (pct) => updateUpload(meta.id, { progress: pct }))
        updateUpload(meta.id, { progress: 100, status: 'done' })
      } catch (err) {
        updateUpload(meta.id, { status: 'error', error: apiError(err, 'Upload failed') })
      }
    }
  }

  const onDrop = (e) => {
    e.preventDefault()
    setDragOver(false)
    if (!canUpload) return
    handleUploadFiles(e.dataTransfer.files)
  }

  // ---------------------------------------------------------------- states

  if (loading) {
    return (
      <div className="centered-page">
        <div className="share-shell">
          <ShareBrand />
          <div key="loading" className="loading-spinner" style={{ padding: 24 }}>Loading shared file…</div>
        </div>
      </div>
    )
  }

  if (needsPassword) {
    return (
      <div className="centered-page">
        <div className="share-shell">
          <ShareBrand />
          <div key="card" className="card share-card">
            <h1 className="share-state-title">This link is protected</h1>
            <p className="share-state-text">Enter the password to access the shared file.</p>

            <form onSubmit={submitPassword}>
              <label className="field-label" htmlFor="share-password">Password</label>
              <div style={{ position: 'relative', marginBottom: 14 }}>
                <Lock
                  size={15}
                  style={{
                    position: 'absolute', left: 11, top: '50%', transform: 'translateY(-50%)',
                    color: 'var(--text-tertiary)', pointerEvents: 'none',
                  }}
                />
                <input
                  id="share-password"
                  type="password"
                  className={`input ${passwordError ? 'input-error' : ''}`}
                  placeholder="Enter password"
                  value={password}
                  onChange={(e) => { setPassword(e.target.value); setPasswordError(false) }}
                  style={{ paddingLeft: 34 }}
                  autoFocus
                />
              </div>
              <button type="submit" className="btn btn-primary" style={{ width: '100%', padding: 9 }} disabled={submitting || !password}>
                {submitting ? 'Checking…' : 'Access file'}
              </button>
            </form>
          </div>
        </div>
      </div>
    )
  }

  if (error || !file) {
    return (
      <div className="centered-page">
        <div className="share-shell">
          <ShareBrand />
          <div key="card" className="card share-card" style={{ textAlign: 'center' }}>
            <div className="error-mark" style={{ margin: '0 auto 10px' }}>
              <AlertCircle size={22} color="var(--accent-danger)" />
            </div>
            <h1 className="share-state-title">Unable to open</h1>
            <p className="share-state-text" style={{ marginBottom: 0 }}>{error}</p>
          </div>
        </div>
      </div>
    )
  }

  const isWrite = share?.permission === 'write'
  const PermIcon = isWrite ? Upload : Lock
  const trailLabel = trail.length ? trail.map((entry) => entry.name).join(' / ') : file.name

  return (
    <div className="centered-page">
      <div className="share-shell">
        <ShareBrand />
        <div key="card" className="card share-file-card">
          <div className="share-file-head">
            <div className={`file-icon share-icon ${iconClassFor(file)}`}>
              <FileIcon item={file} />
            </div>
            <div style={{ flex: 1, minWidth: 0 }}>
              <h1 className="share-file-name">
                {file.name}
              </h1>
              <div className="share-file-meta">
                <span>{file.is_dir ? 'Folder' : formatBytes(file.size)}</span>
                {share?.expires_at && <span>Expires {formatDate(share.expires_at)}</span>}
                {share?.max_views > 0 && (
                  <span>{share.view_count} / {share.max_views} downloads</span>
                )}
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, color: isWrite ? 'var(--accent-primary)' : 'var(--text-tertiary)' }}>
                  <PermIcon size={11} />
                  {isWrite ? 'Read & write' : 'Read only'}
                </span>
              </div>
            </div>
          </div>

          {share?.has_password && (
            <div className="share-badge">
              <CheckCircle size={13} />
              Password verified
            </div>
          )}

          {/* One row, equal widths, two buttons or three. */}
          <div className="share-actions">
            <button
              className="btn btn-primary"
              onClick={() => handleDownload(file)}
              title={file.is_dir ? 'Download the folder as a ZIP archive' : undefined}
            >
              <Download size={14} />
              Download
            </button>
            {!file.is_dir && canPreview(file) && (
              <button className="btn btn-secondary" onClick={() => setPreviewFile(file)}>
                <Eye size={14} />
                Preview
              </button>
            )}
            <button className="btn btn-secondary" onClick={copyLink}>
              <Copy size={14} /> Copy link
            </button>
            {file.is_dir && (
              <button
                className="btn btn-secondary"
                onClick={() => setShowContents((open) => !open)}
                aria-expanded={showContents}
              >
                <FolderOpen size={14} /> {showContents ? 'Hide' : 'Browse'}
              </button>
            )}
          </div>

          {file.is_dir && showContents && (
            <div className="share-contents">
              <div className="share-contents-head">
                {trail.length > 1 ? (
                  <button
                    className="share-contents-back"
                    onClick={goBack}
                    aria-label="Back to the parent folder"
                  >
                    <ChevronLeft size={15} />
                  </button>
                ) : (
                  <span className="share-contents-back is-placeholder" aria-hidden="true" />
                )}
                <span className="share-contents-path" title={trailLabel}>
                  {trailLabel}
                </span>
                <span className="share-contents-count">
                  {contentsLoading
                    ? 'Loading…'
                    : `${contents.length} item${contents.length === 1 ? '' : 's'}`}
                </span>
              </div>

              <div className="share-contents-list">
                {contentsLoading && contents.length === 0 ? (
                  <div className="share-contents-empty">Loading…</div>
                ) : contentsError ? (
                  <div className="share-contents-empty share-contents-error">{contentsError}</div>
                ) : contents.length === 0 ? (
                  <div className="share-contents-empty">This folder is empty.</div>
                ) : (
                  contents.slice(0, CONTENT_RENDER_CAP).map((item) => (
                    <button
                      key={item.id}
                      className="share-contents-row"
                      onClick={() => openEntry(item)}
                      title={
                        item.is_dir
                          ? `Open ${item.name}`
                          : canPreview(item)
                            ? `Preview ${item.name}`
                            : `Download ${item.name}`
                      }
                    >
                      <span className={`file-icon ${iconClassFor(item)}`}>
                        <FileIcon item={item} />
                      </span>
                      <span className="share-contents-name">{item.name}</span>
                      <span className="share-contents-size">
                        {item.is_dir ? <ChevronRight size={14} /> : formatBytes(item.size)}
                      </span>
                    </button>
                  ))
                )}
                {!contentsLoading && !contentsError && contents.length > CONTENT_RENDER_CAP && (
                  <div className="share-contents-empty">
                    Showing the first {CONTENT_RENDER_CAP} of {contents.length} items — download the
                    folder to get the rest.
                  </div>
                )}
              </div>
            </div>
          )}

          {canUpload && (
            <div className="share-upload">
              <div className="share-upload-title">Upload to this folder</div>
              <div
                className={`share-dropzone${dragOver ? ' is-over' : ''}`}
                onDragOver={(e) => { e.preventDefault(); if (canUpload) setDragOver(true) }}
                onDragLeave={() => setDragOver(false)}
                onDrop={onDrop}
                onClick={() => fileInputRef.current?.click()}
                role="button"
                tabIndex={0}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') fileInputRef.current?.click() }}
              >
                <Upload size={18} color="var(--accent-primary)" style={{ marginBottom: 4 }} />
                <div className="share-dropzone-title">
                  Drop files here, or click to pick
                </div>
                <div className="share-dropzone-hint">
                  Added to "{file.name}" without an account.
                </div>
                <input
                  ref={fileInputRef}
                  type="file"
                  multiple
                  style={{ display: 'none' }}
                  onChange={(e) => { handleUploadFiles(e.target.files); e.target.value = '' }}
                />
              </div>

              {uploads.length > 0 && (
                <ul style={{ listStyle: 'none', padding: 0, margin: '10px 0 0', display: 'flex', flexDirection: 'column', gap: 6 }}>
                  {uploads.map((u) => (
                    <li
                      key={u.id}
                      style={{
                        display: 'flex', alignItems: 'center', gap: 8,
                        padding: '7px 10px', borderRadius: 'var(--radius-sm)',
                        background: 'var(--bg-secondary)', fontSize: 11.5,
                      }}
                    >
                      {u.status === 'done' ? (
                        <CheckCircle size={13} color="var(--accent-success)" />
                      ) : u.status === 'error' ? (
                        <AlertCircle size={13} color="var(--accent-danger)" />
                      ) : (
                        <Upload size={13} color="var(--accent-primary)" />
                      )}
                      <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {u.name}
                      </span>
                      {u.status === 'uploading' ? (
                        <span style={{ color: 'var(--text-tertiary)' }}>{u.progress}%</span>
                      ) : u.status === 'error' ? (
                        <span style={{ color: 'var(--accent-danger)' }}>{u.error}</span>
                      ) : (
                        <span style={{ color: 'var(--accent-success)' }}>Uploaded</span>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
        </div>
      </div>

      {/* Same viewer as the Drive, pointed at the public share endpoint so a
          visitor with no account can still look at the file. */}
      {previewFile && !previewFile.is_dir && (
        <PreviewModal
          item={previewFile}
          items={[previewFile]}
          onNavigate={() => {}}
          onClose={() => setPreviewFile(null)}
          onDownload={handleDownload}
          streamUrlFor={() => shareApi.previewUrl(token, password, previewFile.path)}
          fetchUrlFor={() => shareApi.previewUrl(token, password, previewFile.path)}
          needsPreviewToken={false}
        />
      )}
    </div>
  )
}
