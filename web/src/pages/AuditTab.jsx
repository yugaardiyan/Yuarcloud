import React, { useState, useEffect, useCallback } from 'react'
import { adminApi, apiError } from '../utils/api'
import { useMediaQuery } from '../hooks/useMediaQuery'
import { AlertCircle, ChevronLeft, ChevronRight, RefreshCw } from 'lucide-react'

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
  // Five columns need 700px. On a phone the old answer was a horizontal
  // scroll, which put the user and the address off-screen to the right; the
  // rows stack instead, one line of headline and two of detail.
  const phone = useMediaQuery('(max-width: 560px)')

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

  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  const goto = (p) => { if (p >= 1 && p <= pages) { setPage(p); load(p, filter) } }

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16, flexWrap: 'wrap' }}>
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

        <button
          className="btn btn-secondary btn-sm"
          onClick={() => load(page, filter)}
          disabled={loading}
          title="Reload"
        >
          <RefreshCw size={14} />
        </button>
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
          {/* table-layout:auto, not fixed.

              Fixed pins columns to percentages, and the sum of what these five
              actually need is wider than a narrow window: measured at a 630px
              viewport, 574px of table, the timestamp wanted 104px and was given
              86 — so it clipped to "05:04:3…". SharesTab uses fixed and works
              because its columns were tuned for it; this table was copied from
              there and inherited arithmetic that does not transfer.

              Auto sizes each column to its content and wraps the rest. minWidth
              stops it collapsing into slivers in a narrow desktop window, and
              the wrapper scrolls rather than clipping. A phone does not get this
              table at all — see the stacked list above the ternary. */}
          {phone ? (
            <div className="admin-list">
              {logs.map((log) => (
                <div key={log.id} className="admin-item">
                  <div className="admin-item-head">
                    <span className="admin-item-action" title={log.action}>{log.action}</span>
                    <span className="admin-item-stamp" title={log.created_at}>{when(log.created_at)}</span>
                  </div>
                  {/* The detail is the only field with no fixed shape — a whole
                      sentence for a rename, empty for a sign-in. It gets its own
                      wrapping line rather than being cut to an ellipsis. */}
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
          ) : (
          <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', minWidth: 700, borderCollapse: 'collapse', fontSize: 13 }}>
            <thead style={{ background: 'var(--bg-secondary)' }}>
              <tr>
                {['When', 'Action', 'Detail', 'From', 'User'].map((h) => (
                  <th key={h} style={{ padding: 12, textAlign: 'left', fontSize: 11, textTransform: 'uppercase', color: 'var(--text-tertiary)' }}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {logs.map((log) => (
                <tr key={log.id} style={{ borderTop: '1px solid var(--border-color)' }}>
                  {/* table-layout:fixed pins the column widths, so every cell has
                      to clip its own text. Without overflow:hidden here the
                      timestamp spills across the next column and lands on top of
                      the action name — which is what it did. */}
                  <td style={{ padding: 12, whiteSpace: 'nowrap', color: 'var(--text-secondary)' }} title={log.created_at}>
                    {when(log.created_at)}
                  </td>
                  <td style={{ padding: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={log.action}>
                    <span style={{ fontSize: 12, color: 'var(--accent-primary)' }}>{log.action}</span>
                  </td>
                  <td style={{ padding: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={log.details}>
                    {log.details || '—'}
                  </td>
                  <td style={{ padding: 12, color: 'var(--text-secondary)', fontFamily: 'ui-monospace, Menlo, monospace', fontSize: 12 }}>
                    {log.ip_address || '—'}
                  </td>
                  <td style={{ padding: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'var(--text-secondary)' }} title={log.user_id || ''}>
                    {log.username || '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          </div>
          )}

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
