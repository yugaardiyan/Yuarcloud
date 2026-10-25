import React, { useState, useEffect, useCallback } from 'react'
import { createPortal } from 'react-dom'
import { X, Link2, Copy, Calendar, Lock, User, CheckCircle, Eye, Upload } from 'lucide-react'
import { shareApi, apiError } from '../utils/api'
import { nextExpiryDelay } from '../utils/shareExpiry'
import { formatDate } from '../utils/helpers'
import ConfirmModal from './ConfirmModal'
import toast from 'react-hot-toast'
import { claimKeyboard } from '../utils/keyboardOwner'

// Duration presets (days). Default 7. Keeps the form to two choices the
// visitor actually has to think about: password, and how long the link lives.
// -1 is a sentinel for "no expiry": the link stays open until the owner
// deletes the file or the share itself.
const DURATIONS = [
  { value: -1, label: 'No expiry' },
  { value: 1, label: '1 day' },
  { value: 7, label: '7 days' },
  { value: 30, label: '30 days' },
  { value: 90, label: '90 days' },
  { value: 365, label: '1 year' },
]

const STATUS = {
  active: { label: 'Active', tone: 'ok' },
  inactive: { label: 'Deactivated', tone: 'muted' },
  expired: { label: 'Expired', tone: 'danger' },
  limit_reached: { label: 'View limit reached', tone: 'danger' },
}

const PERMISSION = {
  read: { label: 'View', icon: Eye },
  write: { label: 'Edit', icon: Upload },
}

export default function ShareModal({ fileId, fileName, isDir = false, onClose, onCreated }) {
  // Register as an open surface so document-level shortcuts stand down.
  // See utils/keyboardOwner — a shortcut asking "is anything open?" cannot be
  // defeated by a new overlay class it does not know about.
  useEffect(() => {
    const release = claimKeyboard('share-modal')
    return release
  }, [])

  const [loading, setLoading] = useState(false)
  const [shares, setShares] = useState([])
  const [loadingShares, setLoadingShares] = useState(true)
  const [formData, setFormData] = useState({
    password: '',
    expiryDays: 7,
    permission: 'read',
  })
  const [formErrors, setFormErrors] = useState({})
  const [copySuccess, setCopySuccess] = useState(null)
  const [pendingDelete, setPendingDelete] = useState(null)

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  // Defensive load: always land on a valid array so the render never throws
  // and the list can never go "blank" because of a transient shape mismatch.
  // Memoised so the effect below can name it as a dependency instead of
  // suppressing the rule. `fileId` is the only thing it reads.
  const loadShares = useCallback(async () => {
    try {
      const response = await shareApi.list(fileId)
      const list = Array.isArray(response?.data?.shares) ? response.data.shares : []
      setShares(list)
    } catch (error) {
      console.error('Failed to load shares:', error)
      setShares([])
    } finally {
      setLoadingShares(false)
    }
  }, [fileId])

  // After loadShares, not before: a dependency array is evaluated where it is
  // written, so naming a `const` declared further down throws.
  useEffect(() => {
    setLoadingShares(true)
    loadShares()
  }, [loadShares])

  /*
   * A link with a duration stops existing the moment its date passes: the
   * server leaves expired links out of the list, which is why the settings page
   * shows them dropping away on its own. This dialog fetches once, though, so a
   * link that expired while the dialog sat open would stay on screen looking
   * copyable. Sleep until the soonest expiry and read the list again, so the
   * row goes away by itself without the owner closing and reopening. A link
   * with no date is one the owner chose to keep, and never wakes the timer.
   */
  useEffect(() => {
    if (loadingShares) return undefined
    const delay = nextExpiryDelay(shares)
    if (delay === null) return undefined
    const timer = setTimeout(loadShares, delay)
    return () => clearTimeout(timer)
  }, [shares, loadingShares, loadShares])

  // The same rule the server enforces, restated where it is drawn: an expired
  // link is never listed, even if one slips through the response.
  const visibleShares = shares.filter((share) => share.status !== 'expired')

  const handleSubmit = async (e) => {
    e.preventDefault()
    setFormErrors({})

    const errors = {}
    if (formData.expiryDays !== -1 && (formData.expiryDays < 1 || formData.expiryDays > 365)) {
      errors.expiryDays = 'Expiry must be between 1 and 365 days'
    }
    if (Object.keys(errors).length > 0) {
      setFormErrors(errors)
      return
    }

    setLoading(true)
    try {
      await shareApi.create({
        file_id: fileId,
        password: formData.password || undefined,
        expiry_days: formData.expiryDays,
        permission: formData.permission,
      })
      toast.success('Share link created')
      setFormData({ password: '', expiryDays: 7, permission: 'read' })
      await loadShares()
      onCreated?.()
    } catch (error) {
      toast.error(apiError(error, 'Failed to create share link'))
    } finally {
      setLoading(false)
    }
  }

  const copyToClipboard = async (url) => {
    try {
      await navigator.clipboard.writeText(url)
      setCopySuccess(url)
      toast.success('Link copied to clipboard')
      setTimeout(() => setCopySuccess(null), 2000)
    } catch {
      const textarea = document.createElement('textarea')
      textarea.value = url
      document.body.appendChild(textarea)
      textarea.select()
      document.execCommand('copy')
      document.body.removeChild(textarea)
      setCopySuccess(url)
      toast.success('Link copied')
      setTimeout(() => setCopySuccess(null), 2000)
    }
  }

  // Optimistic delete: drop the row immediately so the list feels instant
  // and can never look "stuck" while the server round-trips. If the API
  // call fails we roll the row back so the list stays truthful.
  const requestDeleteShare = (shareId) => setPendingDelete(shareId)

  const confirmDeleteShare = async () => {
    const shareId = pendingDelete
    if (!shareId) return
    setPendingDelete(null)
    const previous = shares
    setShares(shares.filter((s) => s.id !== shareId))
    try {
      await shareApi.remove(shareId)
      toast.success('Share link deleted')
      // Re-sync from the server in case anything else changed under us.
      loadShares()
    } catch (err) {
      setShares(previous)
      toast.error(apiError(err, 'Failed to delete share link'))
    }
  }

  return createPortal(
    <div className="modal-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-labelledby="share-modal-title">
      <div className="modal share-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          {/* The title states what the dialog is showing. With links already on
              the file it is a list to review, not a form to fill in, so saying
              "Share" over a list read as the wrong verb. */}
          <h2 id="share-modal-title" className="modal-title">
            {visibleShares.length > 0 ? 'Existing Share links' : 'Share'} "{fileName}"
          </h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            <X size={15} />
          </button>
        </div>

        <div className={`modal-body ${!loadingShares && visibleShares.length > 0 ? 'is-links' : ''}`}>
          {loadingShares && (
            <div className="share-loading" role="status">
              Loading…
            </div>
          )}

          {!loadingShares && visibleShares.length > 0 && (
            <>
              {visibleShares.map((share) => {
                const status = STATUS[share.status] || STATUS.active
                const perm = PERMISSION[share.permission] || PERMISSION.read
                const PermIcon = perm.icon
                return (
                  <div key={share.id} className={`share-row is-${status.tone}`}>
                    <div className="share-row-top">
                      <Link2 size={16} className="share-row-link-icon" />
                      <span className="share-row-url truncate">{share.url}</span>

                      <span className={`share-status share-status-${status.tone}`}>{status.label}</span>
                      <span className="share-perm" title={perm.label}>
                        <PermIcon size={13} />
                        {perm.label}
                      </span>

                      <div className="share-row-actions">
                        <button
                          className="btn btn-ghost btn-icon btn-sm"
                          onClick={() => copyToClipboard(share.url)}
                          aria-label={copySuccess === share.url ? 'Copied' : 'Copy link'}
                          title={copySuccess === share.url ? 'Copied!' : 'Copy link'}
                          style={copySuccess === share.url ? { color: 'var(--accent-success)' } : undefined}
                        >
                          {copySuccess === share.url ? <CheckCircle size={16} /> : <Copy size={16} />}
                        </button>
                        <button
                          className="btn btn-ghost btn-icon btn-sm"
                          onClick={() => requestDeleteShare(share.id)}
                          title="Delete share link"
                          aria-label="Delete share link"
                          style={{ color: 'var(--accent-danger)' }}
                        >
                          <X size={16} />
                        </button>
                      </div>
                    </div>

                    <div className="share-row-meta">
                      <span>
                        <Calendar size={12} />
                        {share.expires_at ? `Expires ${formatDate(share.expires_at)}` : 'Never expires'}
                      </span>
                      <span>
                        <User size={12} />
                        {share.view_count} downloads
                      </span>
                      {share.has_password && (
                        <span className="is-locked">
                          <Lock size={12} />
                          Password protected
                        </span>
                      )}
                    </div>
                  </div>
                )
              })}
            </>
          )}

          {!loadingShares && visibleShares.length === 0 && (
            <form onSubmit={handleSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 16, marginTop: 20 }}>
              <div className="share-form-row">
                <div>
                  <label style={{ display: 'block', fontSize: 13, fontWeight: 500, marginBottom: 6, color: 'var(--text-primary)' }}>
                    Password (optional)
                  </label>
                  <input
                    type="password"
                    className="input input-capsule"
                    value={formData.password}
                    onChange={(e) => setFormData((prev) => ({ ...prev, password: e.target.value }))}
                    autoComplete="new-password"
                  />
                </div>

                <div>
                  <label style={{ display: 'block', fontSize: 13, fontWeight: 500, marginBottom: 6, color: 'var(--text-primary)' }}>
                    Duration
                  </label>
                  <select
                    className={`input input-capsule ${formErrors.expiryDays ? 'input-error' : ''}`}
                    value={formData.expiryDays}
                    onChange={(e) => setFormData((prev) => ({ ...prev, expiryDays: parseInt(e.target.value, 10) }))}
                  >
                    {DURATIONS.map((d) => (
                      <option key={d.value} value={d.value}>{d.label}</option>
                    ))}
                  </select>
                  {formErrors.expiryDays && (
                    <span style={{ fontSize: 11, color: 'var(--accent-danger)', marginTop: 4, display: 'block' }}>{formErrors.expiryDays}</span>
                  )}
                </div>
              </div>

              <div>
                <label style={{ display: 'block', fontSize: 13, fontWeight: 500, marginBottom: 6, color: 'var(--text-primary)' }}>
                  Access
                </label>
                <div className="share-perm-picker" role="radiogroup" aria-label="Access permission">
                  {[
                    { value: 'read', label: 'View', icon: Eye, desc: 'Visitors can view and download.' },
                    { value: 'write', label: 'Edit', icon: Upload, desc: isDir ? 'Visitors can also upload into this folder.' : 'Available for folders only.' },
                  ].map((opt) => {
                    const Icon = opt.icon
                    const active = formData.permission === opt.value
                    const disabled = opt.value === 'write' && !isDir
                    return (
                      <button
                        key={opt.value}
                        type="button"
                        role="radio"
                        aria-checked={active}
                        disabled={disabled}
                        className={`share-perm-option ${active ? 'is-active' : ''}`}
                        onClick={() => !disabled && setFormData((prev) => ({ ...prev, permission: opt.value }))}
                        title={opt.desc}
                      >
                        <Icon size={16} />
                        <span>
                          <strong>{opt.label}</strong>
                          <small>{opt.desc}</small>
                        </span>
                      </button>
                    )
                  })}
                </div>
              </div>

              <div className="modal-footer" style={{ padding: 0, border: 'none' }}>
                <button type="button" className="btn btn-secondary" onClick={onClose}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary" disabled={loading}>
                  {loading ? 'Creating...' : 'Create Share Link'}
                </button>
              </div>
            </form>
          )}
        </div>
      </div>

      {pendingDelete !== null && (
        <ConfirmModal
          simple
          title="Delete this share link?"
          confirmLabel="Ok"
          cancelLabel="Cancel"
          danger
          onConfirm={confirmDeleteShare}
          onCancel={() => setPendingDelete(null)}
        />
      )}
    </div>,
    document.body
  )
}
