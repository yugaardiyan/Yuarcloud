import React, { useState, useEffect, useCallback, useRef } from 'react'
import { adminApi, apiError } from '../utils/api'
import { AlertCircle, ChevronLeft, ChevronRight } from 'lucide-react'

/**
 * The audit trail, newest first.
 *
 * This tab used to exist and was removed on the stated grounds that server
 * events are read from the container log. That was wrong: every logAudit call
 * writes to the audit_logs table, and GET /api/admin/audit-logs serves them
 * back. The endpoint has always worked — what was missing was a view.
 *
 * Fetched here rather than lifted into the parent, because the paging, the
 * action filter and the load error all belong to this tab and to nothing else.
 * The other tabs take their data as props because their data is shared; this
 * one's is not.
 *
 * Read-only, and deliberately so. Deleting from a record of what already
 * happened is not something a browser button should offer, and the table has
 * no retention policy behind it either — it grows until somebody decides
 * otherwise. See the note at the bottom of this file.
 */

const PAGE_SIZE = 50

// Grouped so the column reads as a sentence rather than a wall of ids. Anything
// unlisted lands in "other" instead of disappearing, so a new action added on
// the server shows up here without a matching change on this side.
const ACTION_GROUPS = {
  auth: { label: 'Sign-in', actions: ['login', 'logout', 'login_failed', 'password_change'] },
  files: { label: 'Files', actions: ['upload', 'delete', 'delete_permanent', 'trash', 'restore'] },
  folders: { label: 'Folders', actions: ['create_folder', 'rename', 'move', 'copy'] },
  shares: { label: 'Shares', actions: ['create_share', 'delete_share', 'share_download', 'share_upload', 'share_preview'] },
  admin: { label: 'Administration', actions: ['admin_create_user', 'admin_update_user', 'admin_delete_user', 'admin_update_quota', 'admin_update_role', 'admin_update_chunked_config'] },
}

// Compact on purpose. The column is fixed-width, and "Today 05:04:18 AM" does
// not fit in it — it clipped to "Today 0…", which tells nobody anything. The
// date is recoverable from the row's tooltip; what the column has to carry is
// when it happened, and "05:04:18 AM" does that in half the width.
function when(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  const today = new Date()
  if (d.toDateString() === today.toDateString()) return time
  const sameYear = d.getFullYear() === today.getFullYear()
  const date = d.toLocaleDateString(undefined, sameYear
    ? { day: '2-digit', month: 'short' }
    : { day: '2-digit', month: 'short', year: 'numeric' })
  return `${date} ${time.slice(0, -3)}`
}

function AuditTab() {
  const [logs, setLogs] = useState([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [filter, setFilter] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  const load = useCallback(async (target, action) => {
    setLoading(true)
    setError('')
    try {
      const { data } = await adminApi.getAuditLogs({
        page: target,
        page_size: PAGE_SIZE,
        ...(action ? { action } : {}),
      })
      setLogs(data.logs || [])
      setTotal(data.total ?? 0)
    } catch (err) {
      setError(apiError(err, 'Could not load the audit log'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { load(1, filter) }, [filter, load])

  // The capsule's create slot doubles as "Reload" while this tab is open. The
  // toolbar lives outside this component tree, so it announces itself through
  // a window event. The current page and filter are kept in a ref so the
  // listener can reload the exact view that is showing without re-subscribing
  // on every page change.
  const reloadRef = useRef(null)
  reloadRef.current = () => load(page, filter)
  useEffect(() => {
    const onReload = () => reloadRef.current()
    window.addEventListener('yuarcloud:reload-audit', onReload)
    return () => window.removeEventListener('yuarcloud:reload-audit', onReload)
  }, [])

  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const goto = (p) => { if (p >= 1 && p <= pages) { setPage(p); load(p, filter) } }

  return (
    <div>
      <div className="audit-header">
        <span className="users-count">
          {loading ? 'Loading…' : `${total} event${total === 1 ? '' : 's'}`}
        </span>

        <div style={{ flex: 1 }} />

        <select
          className="input"
          style={{ width: 'auto', padding: '6px 10px', fontSize: 12 }}
          value={filter}
          onChange={(e) => { setPage(1); setFilter(e.target.value) }}
          aria-label="Filter by action"
        >
          <option value="">All actions</option>
          {Object.entries(ACTION_GROUPS).map(([key, def]) => (
            <option key={key} value={def.actions[0]}>
              {def.label}
            </option>
          ))}
        </select>
      </div>

      {error ? (
        <div className="empty-state">
          <AlertCircle size={28} color="var(--accent-danger)" />
          <p>{error}</p>
          <button className="btn btn-secondary" onClick={() => load(page, filter)}>Try again</button>
        </div>
      ) : !loading && logs.length === 0 ? (
        <div style={{ padding: '48px 0', textAlign: 'center', color: 'var(--text-tertiary)', fontSize: 13 }}>
          {filter ? 'No events with that action.' : 'No events recorded yet.'}
        </div>
      ) : (
        <>
          <div className="admin-list">
            {logs.map((log) => (
              <div key={log.id} className="admin-item">
                <div className="admin-item-head">
                  <span className="admin-item-action" title={log.action}>{log.action}</span>
                  <span className="admin-item-stamp" title={log.created_at}>{when(log.created_at)}</span>
                </div>
                {log.details && (
                  <div className="admin-item-detail" title={log.details}>{log.details}</div>
                )}
                <div className="admin-item-meta">
                  <span>{log.username || '—'}</span>
                  {log.ip_address && <span className="admin-mono">{log.ip_address}</span>}
                </div>
              </div>
            ))}
          </div>

          {pages > 1 && (
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 16 }}>
              <button className="btn btn-secondary btn-sm" disabled={page <= 1} onClick={() => goto(page - 1)}>
                <ChevronLeft size={14} /> Previous
              </button>
              <span style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
                Page {page} of {pages}
              </span>
              <button className="btn btn-secondary btn-sm" disabled={page >= pages} onClick={() => goto(page + 1)}>
                Next <ChevronRight size={14} />
              </button>
            </div>
          )}
        </>
      )}
    </div>
  )
}

export default AuditTab
