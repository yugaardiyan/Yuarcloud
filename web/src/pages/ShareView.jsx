import React, { useState, useEffect, useCallback, useRef } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { shareApi, apiError } from '../utils/api'
import { Download, Lock, AlertCircle, CheckCircle, Copy, Folder, FolderOpen, Upload, Eye } from 'lucide-react'
import { formatBytes, formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from '../components/FileIcon'
import PreviewModal from '../components/PreviewModal'
import { canPreview } from '../utils/fileType'
import Logo from '../components/Logo'
import toast from 'react-hot-toast'
import { POST_LOGIN_REDIRECT } from '../utils/postLoginRedirect'

/** Public landing page for a shared file. No authentication required. */
export default function ShareView() {
  const { token } = useParams()
  const navigate = useNavigate()
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
  const [previewOpen, setPreviewOpen] = useState(false)
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
  const handleDownload = () => {
    if (!file) return
    const link = document.createElement('a')
    link.href = shareApi.downloadUrl(token, password)
    link.download = file.is_dir ? `${file.name}.zip` : file.name
    document.body.appendChild(link)
    link.click()
    document.body.removeChild(link)
  }

  // Browsing needs a session, so the destination is stashed before the router
// sends an anonymous visitor to /login — Login.jsx reads it back and forwards
// there once the sign-in succeeds.
  const browseFolder = () => {
    if (!file?.path) return
    const target = `/cloud/${file.path}`
    sessionStorage.setItem(POST_LOGIN_REDIRECT, target)
    navigate(target)
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
        <div className="loading-spinner">Loading shared file…</div>
      </div>
    )
  }

  if (needsPassword) {
    return (
      <div className="centered-page">
        <div className="share-brand" style={{ marginBottom: 24 }}>
          <span className="brand-mark"><Logo size={28} /></span>
          <span className="brand-name">
            <span className="brand-name-lead">Yuar</span>
            <span className="brand-name-tail">cloud</span>
          </span>
        </div>
        <div className="card share-card">
          <h1 style={{ fontSize: 18, fontWeight: 600, marginBottom: 4, textAlign: 'center' }}>
            This link is protected
          </h1>
          <p style={{ fontSize: 13, color: 'var(--text-secondary)', textAlign: 'center', marginBottom: 16 }}>
            Enter the password to access the shared file.
          </p>

          <form onSubmit={submitPassword}>
            <label className="field-label" htmlFor="share-password">Password</label>
            <div style={{ position: 'relative', marginBottom: 16 }}>
              <Lock
                size={17}
                style={{
                  position: 'absolute', left: 12, top: '50%', transform: 'translateY(-50%)',
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
                style={{ paddingLeft: 40 }}
                autoFocus
              />
            </div>
            <button type="submit" className="btn btn-primary" style={{ width: '100%', padding: 12 }} disabled={submitting || !password}>
              {submitting ? 'Checking…' : 'Access file'}
            </button>
          </form>
        </div>
      </div>
    )
  }

  if (error || !file) {
    return (
      <div className="centered-page">
        <div className="share-brand" style={{ marginBottom: 24 }}>
          <span className="brand-mark"><Logo size={28} /></span>
          <span className="brand-name">
            <span className="brand-name-lead">Yuar</span>
            <span className="brand-name-tail">cloud</span>
          </span>
        </div>
        <div className="card share-card" style={{ textAlign: 'center', padding: 24 }}>
          <div className="error-mark" style={{ margin: '0 auto 12px' }}>
            <AlertCircle size={24} color="var(--accent-danger)" />
          </div>
          <h1 style={{ fontSize: 18, fontWeight: 600, marginBottom: 6 }}>Unable to open</h1>
          <p style={{ fontSize: 13, color: 'var(--text-secondary)' }}>{error}</p>
        </div>
      </div>
    )
  }

  const isWrite = share?.permission === 'write'
  const PermIcon = isWrite ? Upload : Lock

  return (
    <div className="centered-page" style={{ alignItems: 'flex-start', paddingTop: 24 }}>
      <div style={{ width: '100%', maxWidth: 560 }}>
        <div className="share-brand" style={{ marginBottom: 16 }}>
          <span className="brand-mark"><Logo size={28} /></span>
          <span className="brand-name">
            <span className="brand-name-lead">Yuar</span>
            <span className="brand-name-tail">cloud</span>
          </span>
        </div>
        <div className="card share-file-card" style={{ padding: 20 }}>
          <div style={{ display: 'flex', alignItems: 'flex-start', gap: 12, marginBottom: 16 }}>
            <div className={`file-icon share-icon ${iconClassFor(file)}`}>
              <FileIcon item={file} />
            </div>
            <div style={{ flex: 1, minWidth: 0 }}>
              <h1 style={{ fontSize: 17, fontWeight: 600, wordBreak: 'break-word', marginBottom: 4 }}>
                {file.name}
              </h1>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, fontSize: 12, color: 'var(--text-tertiary)' }}>
                <span>{file.is_dir ? 'Folder' : formatBytes(file.size)}</span>
                {share?.expires_at && <span>Expires {formatDate(share.expires_at)}</span>}
                {share?.max_views > 0 && (
                  <span>{share.view_count} / {share.max_views} downloads</span>
                )}
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, color: isWrite ? 'var(--accent-primary)' : 'var(--text-tertiary)' }}>
                  <PermIcon size={12} />
                  {isWrite ? 'Read & write' : 'Read only'}
                </span>
              </div>
            </div>
          </div>

          {share?.has_password && (
            <div style={{
              display: 'flex', alignItems: 'center', gap: 7, padding: '6px 12px',
              background: 'var(--accent-primary-light)', color: 'var(--accent-primary)',
              borderRadius: 'var(--radius-md)', fontSize: 12, marginBottom: 16,
            }}>
              <CheckCircle size={15} />
              Password verified
            </div>
          )}

          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            {!file.is_dir && canPreview(file) && (
              <button className="btn btn-primary" onClick={() => setPreviewOpen(true)}>
                <Eye size={15} />
                Preview
              </button>
            )}
            <button className={`btn ${canPreview(file) && !file.is_dir ? 'btn-secondary' : 'btn-primary'}`} onClick={handleDownload}>
              <Download size={15} />
              {file.is_dir ? 'Download folder' : 'Download'}
            </button>
            <button className="btn btn-secondary" onClick={copyLink}>
              <Copy size={15} /> Copy link
            </button>
            {file.is_dir && (
              <button className="btn btn-secondary" onClick={browseFolder}>
                <FolderOpen size={15} /> Browse contents
              </button>
            )}
          </div>

          {canUpload && (
            <div style={{ marginTop: 24 }}>
              <h2 style={{ fontSize: 14, fontWeight: 600, marginBottom: 10, color: 'var(--text-primary)' }}>
                Upload to this folder
              </h2>
              <div
                onDragOver={(e) => { e.preventDefault(); if (canUpload) setDragOver(true) }}
                onDragLeave={() => setDragOver(false)}
                onDrop={onDrop}
                onClick={() => fileInputRef.current?.click()}
                role="button"
                tabIndex={0}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') fileInputRef.current?.click() }}
                style={{
                  border: `1.5px dashed ${dragOver ? 'var(--accent-primary)' : 'var(--border-color)'}`,
                  borderRadius: 'var(--radius-md)',
                  padding: '16px 12px',
                  textAlign: 'center',
                  cursor: 'pointer',
                  background: dragOver ? 'var(--accent-primary-light)' : 'var(--bg-secondary)',
                  transition: 'all 120ms ease',
                }}
              >
                <Upload size={22} color="var(--accent-primary)" style={{ marginBottom: 6 }} />
                <div style={{ fontSize: 13, color: 'var(--text-primary)', fontWeight: 500 }}>
                  Drop files here, or click to pick
                </div>
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 3 }}>
                  Files are added to "{file.name}" without an account.
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
                <ul style={{ listStyle: 'none', padding: 0, margin: '12px 0 0', display: 'flex', flexDirection: 'column', gap: 6 }}>
                  {uploads.map((u) => (
                    <li
                      key={u.id}
                      style={{
                        display: 'flex', alignItems: 'center', gap: 10,
                        padding: '8px 12px', borderRadius: 'var(--radius-sm)',
                        background: 'var(--bg-secondary)', fontSize: 12,
                      }}
                    >
                      {u.status === 'done' ? (
                        <CheckCircle size={15} color="var(--accent-success)" />
                      ) : u.status === 'error' ? (
                        <AlertCircle size={15} color="var(--accent-danger)" />
                      ) : (
                        <Upload size={15} color="var(--accent-primary)" />
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

          {file.is_dir && !canUpload && (
            <p style={{ fontSize: 12, color: 'var(--text-tertiary)', marginTop: 20, display: 'flex', alignItems: 'center', gap: 6 }}>
              <Folder size={14} />
              Browsing shared folders is not enabled yet.
            </p>
          )}
        </div>

        <p style={{ textAlign: 'center', marginTop: 12, color: 'var(--text-tertiary)', fontSize: 11 }}>
          Shared with <strong>Yuarcloud</strong>
        </p>
      </div>

      {/* Same viewer as the Drive, pointed at the public share endpoint so a
          visitor with no account can still look at the file. */}
      {previewOpen && !file.is_dir && (
        <PreviewModal
          item={file}
          items={[file]}
          onNavigate={() => {}}
          onClose={() => setPreviewOpen(false)}
          onDownload={handleDownload}
          streamUrlFor={() => shareApi.previewUrl(token, password)}
          fetchUrlFor={() => shareApi.previewUrl(token, password)}
          needsPreviewToken={false}
        />
      )}
    </div>
  )
}
