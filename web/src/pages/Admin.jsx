import React, { useState, useEffect, useRef, useCallback } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import {
  Users, BarChart3, Plus, Edit2, Trash2, X,
  HardDrive, Share2, Folder, AlertCircle, Copy, ScrollText, Lock
} from 'lucide-react'
import { adminApi, fileApi, apiError } from '../utils/api'
import { useMediaQuery } from '../hooks/useMediaQuery'
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
  { value: '15m', seconds: 900, label: 'Every 15 minutes' },
  { value: '1h', seconds: 3600, label: 'Every hour' },
  { value: '6h', seconds: 21600, label: 'Every 6 hours' },
  { value: '24h', seconds: 86400, label: 'Every day' },
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
      const params = new URLSearchParams()
      if (page > 1) params.append('page', page)
      if (pageSize !== 20) params.append('page_size', pageSize)

      const response = await adminApi.getUsers(params.toString())
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
            <h1 style={{ fontSize: 22, fontWeight: 700, color: 'var(--text-primary)', marginBottom: 4 }}>
              Settings
            </h1>
            <p style={{ fontSize: 13, color: 'var(--text-secondary)' }}>
              Administration for this Yuarcloud instance.
            </p>
          </header>

          <div className="tabs" style={{ marginBottom: 24 }}>
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
  // Three columns are too many once the card is 320px wide: the name column
  // fell to 98px and "Administrator" clipped to "Adminis…", with the storage
  // figure squeezed onto two lines. Below the stylesheet's phone breakpoint the
  // row becomes a block, same as the share list.
  const phone = useMediaQuery('(max-width: 560px)')

  const userActions = (user) => (
    <div className="user-actions">
      <button
        className="btn btn-ghost btn-icon btn-sm"
        onClick={() => setShowEditUser(user)}
        title="Edit"
      >
        <Edit2 size={13} />
      </button>
      {user.id !== currentUser?.id && (
        <button
          className="btn btn-ghost btn-icon btn-sm"
          onClick={() => onDelete(user)}
          title="Delete"
        >
          <Trash2 size={13} color="var(--accent-danger)" />
        </button>
      )}
    </div>
  )

  return (
    <div>
      {loading ? (
        <div className="loading-spinner">Loading users…</div>
      ) : loadError ? (
        // An empty table under a "No users yet" message reads as "the account
        // list is empty" — and an admin who then deletes something through
        // another tab has just deleted it for no reason. A failed load has to
        // look different from an empty result.
        <div className="empty-state">
          <AlertCircle size={28} color="var(--accent-danger)" />
          <p>{loadError}</p>
          <button className="btn btn-secondary" onClick={loadUsers}>Try again</button>
        </div>
      ) : (
        <>
          <div className="users-header">
            <div>
              <h2>Users</h2>
              <span className="users-count">{users.length} of {usersTotal}</span>
            </div>
            <button
              className="btn btn-primary btn-pill"
              onClick={() => setShowCreateUser(true)}
              style={{ flexShrink: 0 }}
            >
              <Plus size={14} /> New user
            </button>
          </div>

          {phone ? (
            <div className="admin-list">
              {users.map((user) => (
                <div key={user.id} className="admin-item">
                  <div className="admin-item-head">
                    <div className="user-cell">
                      <div className="user-avatar">
                        {user.display_name?.[0]?.toUpperCase() ||
                          user.username?.[0]?.toUpperCase() ||
                          'U'}
                      </div>
                      {/* The name and the address each get the whole line. A
                          role pill after the name was tried first: at 320px it
                          dropped to a line of its own whenever the name filled
                          the width, one 18px line holding a 35px chip. It
                          leads the facts line instead — same place the share
                          list puts its warning chip. */}
                      <div>
                        <div className="user-name">
                          {user.display_name || user.username}
                          {/* Coloured text, not a chip: the same marker the
                              table uses, so the two modes agree. */}
                          <span
                            className="user-role-text"
                            style={{ color: user.role === 'admin' ? '#7048e8' : 'var(--text-secondary)' }}
                          >
                            {user.role}
                          </span>
                        </div>
                        <div className="user-email">{user.email}</div>
                      </div>
                    </div>
                    {userActions(user)}
                  </div>
                  <div className="admin-item-meta">
                    {formatBytes(user.used_bytes || 0)} / {formatBytes(user.quota_bytes || 0)}
                    {' · '}
                    {/* The path is the one detail that can be longer than the
                        card, so it stays inline text and wraps at any point. */}
                    <Folder size={10} style={{ marginRight: 3, verticalAlign: -1 }} />
                    <span className="user-home-path">{user.home_path || 'All files'}</span>
                  </div>
                </div>
              ))}
            </div>
          ) : (
          <div className="users-wrap">
            <table className="users-table">
              <thead>
                <tr>
                  <th>User</th>
                  <th style={{ textAlign: 'right' }}>Storage</th>
                  <th style={{ textAlign: 'right' }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {users.map((user) => (
                  <tr key={user.id}>
                    <td>
                      <div className="user-cell">
                        <div className="user-avatar">
                          {user.display_name?.[0]?.toUpperCase() ||
                            user.username?.[0]?.toUpperCase() ||
                            'U'}
                        </div>
                        <div>
                          <div className="user-name">
                            {user.display_name || user.username}
                            <span
                              className="user-role-text"
                              style={{ color: user.role === 'admin' ? '#7048e8' : 'var(--text-secondary)' }}
                            >
                              {user.role}
                            </span>
                          </div>
                          <div className="user-email">{user.email}</div>
                          <div className="user-home">
                            <Folder size={10} />
                            <span>{user.home_path || 'All files'}</span>
                          </div>
                        </div>
                      </div>
                    </td>
                    <td style={{ textAlign: 'right' }}>
                      <div className="user-storage">
                        {/* An explicit space: a newline after the slash is
                            dropped by JSX, which rendered "0 B /10 GB". */}
                        {formatBytes(user.used_bytes || 0)}{' / '}
                        {formatBytes(user.quota_bytes || 0)}
                      </div>
                    </td>
                    <td style={{ textAlign: 'right' }}>
                      {userActions(user)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          )}

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
  const [formData, setFormData] = useState({ max_chunk_size_mb: 10, max_file_size_gb: 10, cleanup_interval: '1h' })
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (config) {
      setFormData({
        max_chunk_size_mb: Math.round(config.max_chunk_size / 1048576),
        max_file_size_gb: Math.round(config.max_file_size / 1073741824),
        cleanup_interval: cleanupOptionValue(config.cleanup_interval),
      })
    }
  }, [config])

  const handleSave = async () => {
    setLoading(true)
    try {
      await adminApi.updateChunkedConfig({
        max_chunk_size: formData.max_chunk_size_mb * 1048576,
        max_file_size: formData.max_file_size_gb * 1073741824,
        cleanup_interval: formData.cleanup_interval,
      })
      toast.success('Chunked upload configuration saved')
      // The uploader caches the server's chunk size; without this it would
      // keep using the old ceiling until the page was reloaded.
      resetUploadLimits()
    } catch (error) {
      toast.error(error.response?.data?.error || 'Failed to save configuration')
    } finally {
      setLoading(false)
    }
  }

  return (
    /* No card of its own: the page shell is already the white rounded surface,
       so an inner one would read as a box floating inside a box. */
    <div style={{ maxWidth: 600 }}>
      <h3 style={{ fontSize: 16, fontWeight: 600, marginBottom: 12, color: 'var(--text-primary)' }}>
        Chunked Upload Settings
      </h3>

      {/* One row. The three fields are independent of each other and none of
          them needs the width — they were stacked because a form was written
          as a column, not because anything about them was sequential. */}
      <div className="settings-grid">
        <div>
          <label className="field-label" htmlFor="chunk-size">Max Chunk Size (MB)</label>
          <input
            id="chunk-size"
            type="number"
            className="input input-capsule"
            value={formData.max_chunk_size_mb}
            onChange={(e) => setFormData({ ...formData, max_chunk_size_mb: parseInt(e.target.value) || 10 })}
            min={1}
            max={100}
          />
        </div>

        <div>
          <label className="field-label" htmlFor="file-size">Max File Upload (GB)</label>
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
          <label className="field-label" htmlFor="cleanup-interval">Cleanup Interval</label>
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

      {/* One note instead of a paragraph above and one under each field. The
          Cloudflare limit was being stated twice, and the part that is not
          obvious from the label is the second sentence: this is an idle
          timeout, not a cap on how long a transfer may run. */}
      <p className="settings-note">
        5–10 MB per chunk works best behind Cloudflare Tunnel, which caps requests at
        100 MB. Every chunk received resets the cleanup clock, so this is how long an
        abandoned upload's chunks sit on disk before being reclaimed — not a limit on
        how long a transfer may run.
      </p>

      <button className="btn btn-primary btn-pill" style={{ marginTop: 16 }} onClick={handleSave} disabled={loading}>
        {loading ? 'Saving...' : 'Save Configuration'}
      </button>
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

/* Pin the action buttons to a size that fits the fixed Actions column, rather
   than letting .btn-sm decide and then overflowing whatever width it lands on. */
const ACTION_BTN = {
  width: 30,
  minWidth: 30,
  height: 30,
  padding: 0,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
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
  // Seven columns do not fit on a phone. At 390px the file column held "In…",
  // the owner "A…", the expiry date clipped to "10/28/2…" and the status pill
  // sat on top of the copy button. Below the stylesheet's phone breakpoint the
  // table is replaced by stacked rows rather than squeezed.
  const phone = useMediaQuery('(max-width: 560px)')

  const copy = async (share) => {
    if (await copyText(share.url)) {
      setCopied(share.id)
      setTimeout(() => setCopied(null), 1500)
    } else {
      toast.error('Could not copy — your browser blocked clipboard access')
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
        <button className="btn btn-secondary" onClick={loadShares}>Try again</button>
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
        style={ACTION_BTN}
        onClick={() => copy(share)}
        title={copied === share.id ? 'Copied' : 'Copy link'}
        aria-label={`Copy link for ${share.file_name}`}
      >
        {copied === share.id ? '✓' : <Copy size={14} />}
      </button>
      <button
        type="button"
        className="btn btn-ghost btn-icon btn-sm"
        style={{ ...ACTION_BTN, marginLeft: 2, color: 'var(--danger, #e03131)' }}
        onClick={() => setPendingDelete(share)}
        disabled={busyId === share.id}
        title="Delete link"
        aria-label={`Delete link for ${share.file_name}`}
      >
        {busyId === share.id ? '…' : <Trash2 size={14} />}
      </button>
    </>
  )

  if (phone) {
    return (
      <div>
        <div className="admin-list">
          {shares.map((share) => {
            const warn = warning(share)
            // One line of facts, in the order the table read them left to
            // right, written as inline text rather than stacked nodes so it
            // wraps as a sentence at a space. The padlock is not a fact of its
            // own — it qualifies the access beside it, which is where the
            // table puts it too (in the Access column).
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

  return (
    <div>
      {/* Fixed layout, and the actions column is sized in px rather than percent.
          The two buttons inside it are px-wide, so a percentage column
          shrinks them out of the card on a narrow window — present in the DOM,
          unreachable without a scrollbar. Percentages then cover only the
          text columns, and are trimmed to leave room for it. */}
      <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13, tableLayout: 'fixed' }}>
        <colgroup>
          {/* No Status column: the shares that need attention carry a marker
              beside their own name instead. */}
          {[
            ['28%', 'File'],
            ['20%', 'Owner'],
            ['12%', 'Access'],
            ['10%', 'Views'],
            ['16%', 'Expires'],
            ['74px', 'Actions'],
          ].map(([w, label]) => (
            <col key={label} style={{ width: w }} />
          ))}
        </colgroup>
        <thead style={{ background: 'var(--bg-secondary)' }}>
          <tr>
            {['File', 'Owner', 'Access', 'Views', 'Expires', ''].map((h, i) => (
              <th key={h || i} style={{ padding: 12, textAlign: 'left', fontSize: 11, textTransform: 'uppercase', color: 'var(--text-tertiary)' }}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {shares.map(share => {
            const warn = warning(share)
            return (
              <tr key={share.id} style={{ borderTop: '1px solid var(--border-color)' }}>
                <td style={{ padding: 12, whiteSpace: 'nowrap' }} title={share.url}>
                  {/* No link column — the full URL is in this tooltip and is
                      what Copy puts on the clipboard. */}
                  {/* flexWrap so the marker drops to a second line rather than
                      squeezing the name to "A…" on a narrow window. */}
                  <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '2px 6px', minWidth: 0 }}>
                    <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', fontSize: 12, color: 'var(--text-tertiary)', fontFamily: 'ui-monospace, Menlo, monospace' }}>{share.file_name}</span>
                    {warn && <span className="admin-badge admin-badge-warn">{warn}</span>}
                  </div>
                </td>
                <td style={{ padding: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'var(--text-secondary)' }} title={share.owner}>{share.owner || '—'}</td>
                <td style={{ padding: 12, whiteSpace: 'nowrap' }}>
                  {share.permission === 'write' ? 'Edit' : 'View'}
                  {share.has_password && (
                    <Lock size={11} className="admin-lock" role="img" aria-label="Password protected" />
                  )}
                </td>
                <td style={{ padding: 12, whiteSpace: 'nowrap' }}>
                  {share.views}{share.max_views > 0 ? ` / ${share.max_views}` : ''}
                </td>
                <td style={{ padding: 12, whiteSpace: 'nowrap', color: 'var(--text-tertiary)' }}>
                  {share.expires_at ? new Date(share.expires_at).toLocaleDateString() : 'Never'}
                </td>
                <td style={{ padding: '12px 6px', whiteSpace: 'nowrap', textAlign: 'right' }}>
                  {actions(share)}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      {pendingDelete && (
        // The same compact dialog the file list uses for "Move to trash",
        // rather than window.confirm's unstyleable browser block.
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