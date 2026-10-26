import React, { useState, useEffect, useRef, useCallback } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import {
  Users, BarChart3, Edit2, X,
  HardDrive, Share2, AlertCircle, Copy, ScrollText, Lock, CheckCircle
} from 'lucide-react'
import { adminApi, fileApi, apiError } from '../utils/api'
import { TrashGlyph } from '../components/icons'
import { useAuth } from '../context/AuthContext'
import AuditTab from './AuditTab'
import StorageMeter from '../components/StorageMeter'
import ConfirmModal from '../components/ConfirmModal'
import { formatBytes, copyText } from '../utils/helpers'
import { resetUploadLimits } from '../utils/upload'
import toast from 'react-hot-toast'

const TABS = [
  { id: 'users', label: 'Users', icon: Users },
  { id: 'chunked', label: 'Chunked Upload', icon: HardDrive },
  { id: 'stats', label: 'Statistics', icon: BarChart3 },
  { id: 'shares', label: 'Share Links', icon: Share2 },
  { id: 'audit', label: 'Audit Log', icon: ScrollText },
]

const CLEANUP_OPTIONS = [
  { value: '15m', seconds: 900, label: '15 minutes' },
  { value: '1h', seconds: 3600, label: '1 hour' },
  { value: '6h', seconds: 21600, label: '6 hours' },
  { value: '24h', seconds: 86400, label: '1 day' },
]

/**
 * Reads a Go duration string back as seconds.
 *
 * The server answers with the interval already spelled the way it is stored —
 * "6h", not "6h0m0s" — so on a current backend this never has anything to do.
 * It stays because a length the dropdown does not offer still has to be shown
 * truthfully: a hand-edited config can hold 45m, and a select that answers an
 * unfamiliar value with its first entry is showing a setting the operator did
 * not choose.
 */
function durationSeconds(text) {
  const str = String(text ?? '').trim()
  if (!str) return null
  const unit = { h: 3600, m: 60, s: 1, ms: 1 / 1000, 'µs': 0, us: 0, ns: 0 }
  let total = 0
  let matched = false
  const pattern = /([\d.]+)(ns|µs|us|ms|h|m|s)/g
  let part
  while ((part = pattern.exec(str)) !== null) {
    matched = true
    total += parseFloat(part[1]) * unit[part[2]]
  }
  return matched ? Math.round(total) : null
}

/**
 * Maps what the server reports onto the option that means the same length.
 *
 * A length that matches no option is returned unchanged rather than snapped to
 * the first one. A select showing an unfamiliar value is a question; a select
 * showing the wrong one is a setting the operator believes they changed.
 */
function cleanupOptionValue(serverValue) {
  const seconds = durationSeconds(serverValue)
  if (seconds !== null) {
    const hit = CLEANUP_OPTIONS.find((o) => o.seconds === seconds)
    if (hit) return hit.value
  }
  return serverValue || '1h'
}

/**
 * Every folder an admin can hand out as a home directory. Admins are not
 * scoped, so this is the whole tree; the pickers narrow it themselves.
 */
function useFolderPaths() {
  const [folders, setFolders] = useState([])

  useEffect(() => {
    let cancelled = false
    fileApi
      .folderFlat()
      .then((res) => {
        if (cancelled) return
        const list = (res.data.folders || [])
          .filter((f) => f.path)
          .sort((a, b) => a.path.localeCompare(b.path))
        setFolders(list)
      })
      .catch(() => {
        /* the picker just stays empty; creation still works */
      })
    return () => { cancelled = true }
  }, [])

  return folders
}

function Admin() {
  const { user: currentUser } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()

  // The URL is the source of truth so each tab can be linked directly.
  const segment = location.pathname.split('/')[2]
  const activeTab = TABS.some((t) => t.id === segment) ? segment : 'users'

  const [stats, setStats] = useState(null)
  const [users, setUsers] = useState([])
  const [usersTotal, setUsersTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize] = useState(20)
  const [loading, setLoading] = useState(false)
  const [loadError, setLoadError] = useState(null)
  const [showCreateUser, setShowCreateUser] = useState(false)
  const [showEditUser, setShowEditUser] = useState(null)
  // Held until the admin confirms. Kept as the whole user, not just an id, so
  // the dialog can name who is about to be deleted — "Delete this user?" over a
  // table of five is a guess, while "Delete alice?" is not.
  const [pendingDeleteUser, setPendingDeleteUser] = useState(null)
  const [chunkedConfig, setChunkedConfig] = useState(null)
  const [shares, setShares] = useState([])
  const [sharesError, setSharesError] = useState(null)

  // Pages through the user list fast enough that two requests overlap easily,
  // and nothing here said which answer belonged on screen. Clicking "next"
  // three times and having page 2 land after page 4 shows page 2 under a "3"
  // pager — the list and the control disagree, with no way to tell which is
  // wrong. Each load takes a ticket and only the newest may write state.
  const usersRequestId = useRef(0)

  // Memoised for the same reason as ShareModal's loadShares: so the paging
  // effect can depend on these by name rather than on nothing at all.
  const loadStats = useCallback(async () => {
    try {
      const response = await adminApi.getStats()
      setStats(response.data)
    } catch (error) {
      console.error('Failed to load stats:', error)
    }
  }, [])

  const loadUsers = useCallback(async () => {
    const id = ++usersRequestId.current
    setLoading(true)
    try {
      // A plain object, not params.toString(): axios expects a record here and
      // a string is serialised as a single unnamed parameter, so page 2 was
      // requested as the default page 1.
      const params = {}
      if (page > 1) params.page = page
      if (pageSize !== 20) params.page_size = pageSize

      const response = await adminApi.getUsers(params)
      if (id !== usersRequestId.current) return
      setLoadError(null)
      setUsers(response.data.users)
      setUsersTotal(response.data.total)
    } catch (error) {
      if (id !== usersRequestId.current) return
      // Say so. The failure used to be logged to a console nobody opens and
      // the table was left showing the previous page's rows under the new
      // pager, which reads as "these are the users" rather than "this failed".
      setUsers([])
      setUsersTotal(0)
      setLoadError(apiError(error, 'Could not load users'))
      console.error('Failed to load users:', error)
    } finally {
      if (id === usersRequestId.current) setLoading(false)
    }
  }, [page, pageSize])

  // Declared after both loaders: naming them in a dependency array reads them,
  // and a `const` still in its temporal dead zone throws. It used to be safe
  // only because the array said `[page]`.
  useEffect(() => {
    loadStats()
    loadUsers()
  }, [loadStats, loadUsers])

  const loadChunkedConfig = async () => {
    try {
      const response = await adminApi.getChunkedConfig()
      setChunkedConfig(response.data)
    } catch (error) {
      console.error('Failed to load chunked config:', error)
    }
  }

  const loadShares = async () => {
    try {
      const response = await adminApi.getShares({ page_size: 100 })
      setSharesError(null)
      setShares(response.data.shares || [])
    } catch (error) {
      // "No share links yet." was also what a failed request produced, because
      // the catch only wrote to the console. An admin looking at a list of
      // public links cannot tell "there are none" from "I could not see them",
      // and those demand opposite responses.
      setSharesError(apiError(error, 'Could not load share links'))
      setShares([])
      console.error('Failed to load share links:', error)
    }
  }

  const deleteUser = async () => {
    const user = pendingDeleteUser
    setPendingDeleteUser(null)
    if (!user) return
    try {
      await adminApi.deleteUser(user.id)
      toast.success(`Deleted ${user.display_name || user.username}`)
      loadUsers()
      loadStats()
    } catch (error) {
      toast.error(error?.response?.data?.error || 'Failed to delete user')
    }
  }

  // Fetch a tab's data the first time it is opened.
  useEffect(() => {
    if (activeTab === 'chunked') loadChunkedConfig()
    if (activeTab === 'shares') loadShares()
  }, [activeTab])

  // The create slot in the header capsule doubles as the tab's primary action:
  // "Add user" on the Users tab and "Save configuration" on the Chunked tab.
  // It announces itself through a window event rather than a prop — the
  // toolbar lives outside this page's component tree, and a callback drilled
  // through the layout would touch components that know nothing about admin.
  useEffect(() => {
    const onSettingsAction = (e) => {
      const { action } = e.detail || {}
      if (action === 'add-user' && activeTab === 'users') setShowCreateUser(true)
      if (action === 'save-chunked-config' && activeTab === 'chunked') {
        window.dispatchEvent(new CustomEvent('yuarcloud:save-chunked-form'))
      }
      if (action === 'reload-audit' && activeTab === 'audit') {
        window.dispatchEvent(new CustomEvent('yuarcloud:reload-audit'))
      }
    }
    window.addEventListener('yuarcloud:settings-action', onSettingsAction)
    return () => window.removeEventListener('yuarcloud:settings-action', onSettingsAction)
  }, [activeTab])

  return (
    /* Same shell as the drive: the page body sits in one rounded white card
       against the grey, with the header's pills lining up to its edges. The
       tabs ride at the top of the card rather than floating on the grey, so the
       two pages read as one surface rather than as a browser and a document.
       The class carries the matching bottom gap — see .settings-page in main.css. */
    <div className="settings-page" style={{ display: 'flex', flexDirection: 'column', minHeight: 0, height: '100%' }}>
      <div className="drive-shell">
        <div className="app-content">
          <header style={{ marginBottom: 20 }}>
            <h1 style={{ fontSize: 22, fontWeight: 700, color: 'var(--text-primary)' }}>
              Settings
            </h1>
          </header>

          <div className="tabs" style={{ marginBottom: 8 }}>
            {TABS.map((tab) => (
              <button
                key={tab.id}
                className={`tab ${activeTab === tab.id ? 'active' : ''}`}
                onClick={() => navigate(tab.id === 'users' ? '/admin' : `/admin/${tab.id}`)}
                aria-current={activeTab === tab.id ? 'page' : undefined}
                /* A phone drops the words and keeps the glyphs, so the name has
                   to live somewhere other than the visible text. No title: on a
                   desktop the label is already on screen, and a tooltip that
                   repeats the word you are pointing at is noise. */
                aria-label={tab.label}
              >
                <tab.icon size={16} className="tab-icon" />
                <span className="tab-label">{tab.label}</span>
              </button>
            ))}
          </div>

      {activeTab === 'users' && (
        <UsersTab
          users={users}
          usersTotal={usersTotal}
          page={page}
          setPage={setPage}
          loading={loading}
          loadError={loadError}
          onDelete={setPendingDeleteUser}
          showCreateUser={showCreateUser}
          setShowCreateUser={setShowCreateUser}
          showEditUser={showEditUser}
          setShowEditUser={setShowEditUser}
          loadUsers={loadUsers}
          currentUser={currentUser}
        />
      )}

      {activeTab === 'chunked' && <ChunkedConfigTab config={chunkedConfig} />}
      {activeTab === 'stats' && <StatsTab stats={stats} />}
      {activeTab === 'shares' && <SharesTab shares={shares} sharesError={sharesError} onReload={loadShares} />}
      {activeTab === 'audit' && <AuditTab />}

      {/* Delete is soft: the account goes, the files stay on disk and lose their
          owner. Say so here rather than letting someone assume the storage was
          reclaimed too. */}
      {pendingDeleteUser && (
        <ConfirmModal
          simple
          danger
          title={`Delete the user "${pendingDeleteUser.display_name || pendingDeleteUser.username}"? Their files stay in storage, but nobody can reach them afterwards.`}
          confirmLabel="Delete"
          onConfirm={deleteUser}
          onCancel={() => setPendingDeleteUser(null)}
        />
      )}
        </div>
      </div>
    </div>
  )
}

function UsersTab({ users, usersTotal, page, setPage, loading, loadError, onDelete, showCreateUser, setShowCreateUser, showEditUser, setShowEditUser, loadUsers, currentUser }) {
  const userActions = (user) => (
    <span className="admin-item-actions">
      {user.id !== currentUser?.id && (
        <button
          className="btn btn-ghost btn-icon btn-sm"
          style={{ color: 'var(--accent-danger)' }}
          onClick={() => onDelete(user)}
          title="Delete"
        >
          <TrashGlyph size={16} />
        </button>
      )}
      <button
        className="btn btn-ghost btn-icon btn-sm"
        onClick={() => setShowEditUser(user)}
        title="Edit"
      >
        <Edit2 size={14} />
      </button>
    </span>
  )

  return (
    <div>
      {loading ? (
        <div className="loading-spinner">Loading users…</div>
      ) : loadError ? (
        <div className="empty-state">
          <AlertCircle size={28} color="var(--accent-danger)" />
          <p>{loadError}</p>
          <button className="btn btn-secondary" onClick={loadUsers}>Try again</button>
        </div>
      ) : (
        <>
          <div className="admin-list">
            {users.map((user) => {
              const quota = `${formatBytes(user.used_bytes || 0)} / ${formatBytes(user.quota_bytes || 0)}`
              return (
                <div key={user.id} className="admin-item">
                  <div className="admin-item-head">
                    <span className="admin-item-title" title={user.display_name || user.username}>
                      {user.display_name || user.username}
                    </span>
                    {userActions(user)}
                  </div>
                  <div className="admin-item-meta">
                    {user.email}
                    {' · '}
                    {quota}
                    {' · '}
                    {user.home_path || 'All files'}
                  </div>
                </div>
              )
            })}
          </div>

          {usersTotal > 20 && (
            <div
              style={{
                display: 'flex',
                justifyContent: 'space-between',
                alignItems: 'center',
                marginTop: 16,
                fontSize: 12,
                color: 'var(--text-tertiary)',
              }}
            >
              <span>
                Showing {(page - 1) * 20 + 1} to{' '}
                {Math.min(page * 20, usersTotal)} of {usersTotal} users
              </span>
              <div style={{ display: 'flex', gap: 8 }}>
                <button
                  className="btn btn-secondary btn-sm"
                  disabled={page <= 1}
                  onClick={() => setPage(page - 1)}
                >
                  Previous
                </button>
                <button
                  className="btn btn-secondary btn-sm"
                  disabled={page >= Math.ceil(usersTotal / 20)}
                  onClick={() => setPage(page + 1)}
                >
                  Next
                </button>
              </div>
            </div>
          )}
        </>
      )}

      {showCreateUser && <UserModal onClose={() => setShowCreateUser(false)} onSuccess={loadUsers} />}
      {showEditUser && (
        <UserModal
          user={showEditUser}
          onClose={() => setShowEditUser(null)}
          onSuccess={loadUsers}
        />
      )}
    </div>
  )
}

/**
 * One dialog for both creating and editing a user.
 *
 * Editing reuses the create form rather than a cut-down variant, so every
 * field an admin can set at creation stays reachable afterwards. Password is
 * the one exception: leaving it blank keeps the existing one.
 */
function UserModal({ user, onClose, onSuccess }) {
  const editing = Boolean(user)

  // The quota the form starts from, expressed the way the form expresses it.
  //
  // The field is edited in whole gigabytes, so a quota set to something the
  // field cannot represent -- 1.5 GB, say -- comes back as 2. Comparing the
  // submitted bytes against the raw stored value would then always report a
  // change and quietly round the quota up on every save. Both the initial state
  // and the change test below have to start from the same rounded number.
  const DEFAULT_QUOTA_BYTES = 10737418240
  const initialQuotaBytes = Math.round(
    (user?.quota_bytes ?? DEFAULT_QUOTA_BYTES) / 1073741824
  ) * 1073741824

  const [formData, setFormData] = useState({
    username: user?.username || '',
    email: user?.email || '',
    display_name: user?.display_name || '',
    password: '',
    role: user?.role || 'user',
    quota_gb: initialQuotaBytes / 1073741824,
    home_path: user?.home_path || '',
  })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const folders = useFolderPaths()

  // A user can be homed in a folder that no longer appears in the tree — it was
// trashed, renamed, or moved out from under them. The picker only ever offers
// live folders, so that stored home had no matching <option>.
//
// A <select> whose value matches no option does not render blank: it falls back
// to the first option and *displays* "All files (storage root)". React's own
// state still holds the real path, so submitting sent the stale value and the
// server answered "home folder not found" — which made every save fail. An
// admin trying to change a role or a quota got a failure with no way forward,
// and the field meanwhile claimed the user had no home at all.
//
// Re-inserting the missing path keeps the value round-tripping and shows what
// is actually configured. Deleting the folder stays a deliberate choice: the
// admin picks "All files (storage root)" and the assignment is cleared on save.
const missingHome = formData.home_path && !folders.some((f) => f.path === formData.home_path)

const folderOptions = missingHome
  ? [{ path: formData.home_path, missing: true }, ...folders]
  : folders

const set = (key) => (e) => {
    const value = e.target.value
    setFormData((prev) => ({ ...prev, [key]: key === 'quota_gb' ? parseInt(value) || 0 : value }))
    setError('')
  }

  // Only what is actually being sent has to be valid.
  //
  // These checks used to be unconditional, so an account with no email on it --
  // which is exactly what the seeded admin is -- could never be edited at all:
  // the empty Email box failed the '@' test, Save stayed disabled, and the only
  // way forward looked like filling in every field. One field could not be
  // changed on its own. That is the same wall the quota bug put up from the
  // other side.
  //
  // The submit handler sends only changed fields, so an untouched field is not
  // being validated by the server either, and there is no reason to block the
  // dialog on it. A field the admin is editing still has to pass.
  const usernameChanged = formData.username !== (user?.username || '')
  const emailChanged = formData.email !== (user?.email || '')

  const canSubmit =
    (!usernameChanged || formData.username.length >= 3) &&
    (!emailChanged || formData.email.includes('@')) &&
    (editing || formData.password.length >= 8)

  const handleSubmit = async (e) => {
    e.preventDefault()
    if (!canSubmit || loading) return

    setLoading(true)
    setError('')
    try {
      if (editing) {
        // Only the fields that actually changed.
        //
        // The quota is not in here at all: it is deliberately absent from the
        // general PATCH's allow-list and has a route of its own, which is what
        // audits it. Sending it here made the server reject the entire save with
        // "field cannot be updated here: quota_bytes" -- so no edit of any kind
        // could go through, not even a one-character typo fix. That is also why
        // this read as a form that insists on every field being retyped: the
        // only thing it ever did was fail, and the obvious way to respond to a
        // failure is to try harder with the form.
        const patch = {}
        if (formData.username !== user.username) patch.username = formData.username
        if (formData.email !== user.email) patch.email = formData.email
        if (formData.display_name !== (user.display_name || '')) {
          patch.display_name = formData.display_name
        }
        if (formData.role !== user.role) patch.role = formData.role
        if (formData.home_path !== (user.home_path || '')) patch.home_path = formData.home_path
        // An empty password must never blank the hash.
        if (formData.password) patch.password = formData.password

        const quotaBytes = formData.quota_gb * 1073741824
        const quotaChanged = quotaBytes !== initialQuotaBytes

        if (!Object.keys(patch).length && !quotaChanged) {
          toast('No changes to save')
          return
        }

        if (Object.keys(patch).length) await adminApi.updateUser(user.id, patch)
        if (quotaChanged) await adminApi.updateUserQuota(user.id, quotaBytes)
        toast.success('User updated')
      } else {
        await adminApi.createUser({
          username: formData.username,
          email: formData.email,
          password: formData.password,
          display_name: formData.display_name,
          role: formData.role,
          quota_bytes: formData.quota_gb * 1073741824,
          home_path: formData.home_path,
        })
        toast.success('User created')
      }
      onSuccess()
      onClose()
    } catch (err) {
      setError(err.response?.data?.error || 'Failed to save user')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal sheet user-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2 className="modal-title">{editing ? 'Edit user' : 'New user'}</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close"><X size={15} /></button>
        </div>

        <form onSubmit={handleSubmit} noValidate>
          <div className="modal-body">
            <div className="user-field">
              <input className="login-input login-input-plain" placeholder="Username" value={formData.username} onChange={set('username')} autoComplete="off" autoFocus />
            </div>
            <div className="user-field">
              <input type="email" className="login-input login-input-plain" placeholder="Email" value={formData.email} onChange={set('email')} autoComplete="off" />
            </div>
            <div className="user-field">
              <input className="login-input login-input-plain" placeholder="Display name" value={formData.display_name} onChange={set('display_name')} autoComplete="off" />
            </div>
            <div className="user-field">
              <input
                type="password"
                className="login-input login-input-plain"
                placeholder={editing ? 'New password (optional)' : 'Password'}
                value={formData.password}
                onChange={set('password')}
                autoComplete="new-password"
              />
            </div>

            <div className="user-row">
              <select className="login-input login-input-plain" value={formData.role} onChange={set('role')} aria-label="Role">
                <option value="user">User</option>
                <option value="admin">Admin</option>
              </select>
              <input
                type="number"
                className="login-input login-input-plain"
                placeholder="Quota (GB)"
                value={formData.quota_gb}
                onChange={set('quota_gb')}
                min={0}
                aria-label="Quota in gigabytes"
              />
            </div>

            <div className="user-field">
              <select className="login-input login-input-plain" value={formData.home_path} onChange={set('home_path')} aria-label="Root folder">
                <option value="">All files (storage root)</option>
                {folderOptions.map((f) => (
                  <option key={f.path} value={f.path}>
                    {f.path}{f.missing ? ' (folder missing)' : ''}
                  </option>
                ))}
              </select>
              {missingHome && (
                <p style={{ fontSize: 11, color: 'var(--accent-warning)', marginTop: 5 }}>
                  This user is homed in a folder that no longer exists. Pick another
                  directory, or the assignment will keep failing to save.
                </p>
              )}
            </div>

            {/* Reserved line so an error never nudges the fields. */}
            <p className="sheet-notice" data-filled={Boolean(error)} role="alert" aria-live="polite">
              {error && (
                <>
                  <AlertCircle size={13} />
                  <span>{error}</span>
                </>
              )}
            </p>
          </div>

          <div className="modal-footer">
            <button type="button" className="btn btn-secondary" onClick={onClose}>Cancel</button>
            <button type="submit" className="btn btn-primary" disabled={loading || !canSubmit}>
              {loading ? 'Saving…' : editing ? 'Save changes' : 'Create user'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function ChunkedConfigTab({ config }) {
  // The recommendation ("5–10 MB is best", "1–10, 3 is good") is drawn inside
  // the capsule as an overlay, not a placeholder, so it stays visible while a
  // value is typed. The fields therefore no longer need to start empty.
  const [formData, setFormData] = useState({
    max_chunk_size_mb: 10,
    max_file_size_gb: 10,
    max_concurrent_uploads: 3,
    cleanup_interval: '1h',
  })
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (config) {
      setFormData({
        max_chunk_size_mb: Math.round(config.max_chunk_size / 1048576),
        max_file_size_gb: Math.round(config.max_file_size / 1073741824),
        max_concurrent_uploads: config.max_concurrent_uploads ?? 3,
        cleanup_interval: cleanupOptionValue(config.cleanup_interval),
      })
    }
  }, [config])

  const handleSave = useCallback(async () => {
    // The form starts on defaults while the fetch is in flight. Saving then
    // wrote 10 MB / 10 GB / 1h over whatever the server actually had.
    if (!config) return
    if (loading) return
    setLoading(true)
    try {
      const chunkMb = formData.max_chunk_size_mb === '' ? config.max_chunk_size / 1048576 : Number(formData.max_chunk_size_mb)
      const parallel = formData.max_concurrent_uploads === '' ? config.max_concurrent_uploads : Number(formData.max_concurrent_uploads)
      await adminApi.updateChunkedConfig({
        max_chunk_size: chunkMb * 1048576,
        max_file_size: Number(formData.max_file_size_gb) * 1073741824,
        max_concurrent_uploads: parallel,
        cleanup_interval: formData.cleanup_interval,
      })
      toast.success('Chunked upload configuration saved')
      // The uploader caches the server's chunk size and concurrency; without
      // this it would keep using the old values until the page was reloaded.
      resetUploadLimits()
    } catch (error) {
      toast.error(error.response?.data?.error || 'Failed to save configuration')
    } finally {
      setLoading(false)
    }
  }, [config, loading, formData])

  const handleSaveRef = useRef(handleSave)
  useEffect(() => { handleSaveRef.current = handleSave }, [handleSave])
  // The header capsule fires this event instead of owning the save action —
  // the form state lives here, so only this component may commit it.
  useEffect(() => {
    const onExternalSave = () => handleSaveRef.current()
    window.addEventListener('yuarcloud:save-chunked-form', onExternalSave)
    return () => window.removeEventListener('yuarcloud:save-chunked-form', onExternalSave)
  }, [])

  return (
    /* No card of its own: the page shell is already the white rounded surface,
       so an inner one would read as a box floating inside a box. */
    <div style={{ maxWidth: 600 }}>
      {/* Two rows of two, at every width. Four fields in one row would wrap
          every label; four rows made a short form look long on a phone. */}
      <div className="settings-grid">
        <div>
          <label className="field-label" htmlFor="chunk-size">Max Chunk Size (MB)</label>
          <div className="input-wrap">
            <input
              id="chunk-size"
              type="number"
              className="input input-capsule input-inline-hint"
              value={formData.max_chunk_size_mb}
              onChange={(e) => setFormData({ ...formData, max_chunk_size_mb: e.target.value })}
              min={1}
              max={100}
            />
            <span className="input-hint" aria-hidden="true">5–10 MB is best</span>
          </div>
        </div>

        <div>
          <label className="field-label" htmlFor="file-size">Max File Size (GB)</label>
          <input
            id="file-size"
            type="number"
            className="input input-capsule"
            value={formData.max_file_size_gb}
            onChange={(e) => setFormData({ ...formData, max_file_size_gb: parseInt(e.target.value) || 10 })}
            min={1}
          />
        </div>

        <div>
          <label className="field-label" htmlFor="concurrent-uploads">Parallel Uploads</label>
          <div className="input-wrap">
            <input
              id="concurrent-uploads"
              type="number"
              className="input input-capsule input-inline-hint"
              value={formData.max_concurrent_uploads}
              onChange={(e) => setFormData({ ...formData, max_concurrent_uploads: e.target.value })}
              min={1}
              max={10}
            />
            <span className="input-hint" aria-hidden="true">1–10, 3 is good</span>
          </div>
        </div>

        <div>
          <label className="field-label" htmlFor="cleanup-interval">Chunk cleanup after</label>
          <select
            id="cleanup-interval"
            className="input input-capsule"
            value={formData.cleanup_interval}
            onChange={(e) => setFormData({ ...formData, cleanup_interval: e.target.value })}
          >
            {CLEANUP_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
            {/* A length the server reports that is not one of the four above —
                a hand-edited config, say. It stays visible and selectable
                rather than being quietly replaced by the first option. */}
            {!CLEANUP_OPTIONS.some((o) => o.value === formData.cleanup_interval) && (
              <option value={formData.cleanup_interval}>{formData.cleanup_interval}</option>
            )}
          </select>
        </div>
      </div>
    </div>
  )
}

/**
 * Statistics.
 *
 * One card, because there is one thing worth looking at. Storage leads: it is
 * the only figure that tells an operator whether to free space, and files,
 * folders, users and share links are all detail hanging off the same bytes. They
 * used to be spread across three surfaces — a meter and two tiles — which spent
 * more of the screen on borders and padding than on numbers.
 *
 * There is no "active users" figure. It counted the is_active flag, which is the
 * same one the Users tab toggles, and UpdateUser strips that field from the
 * request — so nothing on this instance can ever clear it. The number could not
 * differ from the user count, which makes it look meaningful while being unable
 * to say anything.
 */
function StatsTab({ stats }) {
  if (!stats) return <div className="loading-spinner">Loading stats...</div>
  return <StorageMeter stats={stats.storage} users={stats.users} />
}

/**
 * Every share link on the instance, newest first.
 *
 * This tab used to carry a note that it "replaces the audit-log table: server
 * events are read from the container log". That was false, and it is why the
 * audit table is not here.
 *
 * Events are not read from `docker logs yuarcloud`. They are rows. Every logAudit
 * call writes to the audit_logs table and adminApi.getAuditLogs fetches them
 * from GET /api/admin/audit-logs. A throwaway instance wrote seven rows, returned
 * all seven through that endpoint, still had all seven after a restart, and put
 * none of them in the application log — the only lines there are gin's access
 * log for requests to the endpoint itself.
 *
 * So the endpoint is live and the data is there; what is missing is a table to
 * put it in. adminApi.getAuditLogs is already wired and correct. Reinstating the
 * tab is a frontend change, and it does not require anything on the server.
 */
function SharesTab({ shares, sharesError, onReload }) {
  const [copied, setCopied] = useState(null)
  const [busyId, setBusyId] = useState(null)
  const [pendingDelete, setPendingDelete] = useState(null)

  const copy = async (share) => {
    try {
      await navigator.clipboard.writeText(share.url)
      setCopied(share.id)
      toast.success('Link copied to clipboard')
      setTimeout(() => setCopied(null), 2000)
    } catch {
      if (await copyText(share.url)) {
        setCopied(share.id)
        toast.success('Link copied')
        setTimeout(() => setCopied(null), 2000)
      } else {
        toast.error('Could not copy — your browser blocked clipboard access')
      }
    }
  }

  const confirmDelete = async () => {
    const share = pendingDelete
    setPendingDelete(null)
    setBusyId(share.id)
    try {
      await adminApi.deleteShare(share.id)
      toast.success('Share link deleted')
      onReload()
    } catch (error) {
      toast.error(error?.response?.data?.error || 'Failed to delete share link')
    } finally {
      setBusyId(null)
    }
  }

  // Not a status column — every row in this list is a link that works, and
  // saying so on each one was a column of the same word. What is worth the
  // space is the exception the owner still has to act on: a link switched off,
  // or one that has used up its views. Expired links are not here at all; the
  // server stops returning them the moment they lapse.
  const warning = (s) => {
    if (!s.is_active) return 'Disabled'
    if (s.max_views > 0 && s.views >= s.max_views) return 'Limit reached'
    return null
  }

  if (sharesError) {
    return (
      <div className="empty-state">
        <AlertCircle size={28} color="var(--accent-danger)" />
        <p>{sharesError}</p>
        <button className="btn btn-secondary" onClick={onReload}>Try again</button>
      </div>
    )
  }

  if (!shares.length) {
    return (
      <div style={{ padding: '48px 0', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
        No share links yet.
      </div>
    )
  }

  // Same two buttons in both layouts, so they are written once.
  const actions = (share) => (
    <>
      <button
        type="button"
        className="btn btn-ghost btn-icon btn-sm"
        style={{ color: 'var(--accent-danger)' }}
        onClick={() => setPendingDelete(share)}
        disabled={busyId === share.id}
        title="Delete link"
        aria-label={`Delete link for ${share.file_name}`}
      >
        {busyId === share.id ? <span style={{ fontSize: 12, color: 'inherit' }}>…</span> : <TrashGlyph size={16} />}
      </button>
      <button
        type="button"
        className="btn btn-ghost btn-icon btn-sm"
        onClick={() => copy(share)}
        title={copied === share.id ? 'Copied!' : 'Copy link'}
        aria-label={`Copy link for ${share.file_name}`}
        style={copied === share.id ? { color: 'var(--accent-success)' } : undefined}
      >
        {copied === share.id ? <CheckCircle size={14} /> : <Copy size={14} />}
      </button>
    </>
  )

  return (
    <div>
      <div className="admin-list">
        {shares.map((share) => {
          const warn = warning(share)
          const perm = share.permission === 'write' ? 'Edit' : 'View'
          const views = `${share.views}${share.max_views > 0 ? ` / ${share.max_views}` : ''} view${share.views === 1 ? '' : 's'}`
          const expiry = share.expires_at
            ? `Expires ${new Date(share.expires_at).toLocaleDateString()}`
            : 'Never expires'

          return (
            <div key={share.id} className="admin-item">
              <div className="admin-item-head">
                <span className="admin-item-title" title={share.url}>{share.file_name}</span>
                <span className="admin-item-actions">{actions(share)}</span>
              </div>
              <div className="admin-item-meta">
                {warn && <span className="admin-badge admin-badge-warn">{warn}</span>}
                {share.owner || '—'}{' · '}
                <span className="admin-perm">
                  {perm}
                  {share.has_password && <Lock size={11} className="admin-lock" role="img" aria-label="Password protected" />}
                </span>
                {' · '}{views}{' · '}{expiry}
              </div>
            </div>
          )
        })}
      </div>

      {pendingDelete && (
        <ConfirmModal
          simple
          danger
          title={`Delete the share link for "${pendingDelete.file_name}"?`}
          confirmLabel="Delete"
          onConfirm={confirmDelete}
          onCancel={() => setPendingDelete(null)}
        />
      )}
    </div>
  )
}

export default Admin