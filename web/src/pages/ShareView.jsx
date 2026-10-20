import React, { useState, useEffect, useLayoutEffect, useCallback, useRef } from 'react'
import { useParams } from 'react-router-dom'
import { shareApi, apiError } from '../utils/api'
import { withViewTransition } from '../utils/viewTransition'
import { Download, Lock, AlertCircle, CheckCircle, Copy, FolderOpen, Upload, Eye, ChevronLeft, ChevronRight } from 'lucide-react'
import { formatBytes, formatDate } from '../utils/helpers'
import { FileIcon, iconClassFor } from '../components/FileIcon'
import PreviewModal from '../components/PreviewModal'
import { canPreview } from '../utils/fileType'
import Logo from '../components/Logo'
import ToastHost from '../components/ToastHost'
import toast from 'react-hot-toast'

/** The mark and wordmark, plain. The header's bordered capsule carries a
 *  shadow and a 46px height that dwarf a single card, so the share page uses
 *  the same two-tone lockup without the chrome. */
function ShareBrand() {
  return (
    <div className="share-brand">
      <Logo size={26} />
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

/** The folder the address bar asks for, if it sits inside the shared folder.
 *
 *  A share link with `?path=` is how a visitor comes back to the folder they
 *  were in after a reload, and how a copied link carries the same place to
 *  someone else. The value is only a hint: it is checked against the shared
 *  folder's own path here, and the server checks it again against whole
 *  segments before answering, so a hand-edited path cannot step outside.
 */
function folderFromUrl(rootPath) {
  if (typeof window === 'undefined') return null
  const raw = new URLSearchParams(window.location.search).get('path')
  if (!raw) return null
  const path = raw.trim().replace(/\/+$/, '')
  if (!rootPath || path === rootPath) return null
  if (!path.startsWith(`${rootPath}/`)) return null
  return path
}

/** Whether the address bar asks for the list to be open at the shared root.
 *  A link copied while browsing the root carries `?browse=1`; a bare link is
 *  the cover, closed. */
function openFromUrl() {
  if (typeof window === 'undefined') return false
  return new URLSearchParams(window.location.search).get('browse') === '1'
}

/** The trail from the shared folder down to `path`, which is what the back
 *  button and the breadcrumb are built from. */
function trailTo(rootPath, rootName, path) {
  const entries = [{ path: rootPath, name: rootName }]
  let walked = rootPath
  for (const name of path.slice(rootPath.length + 1).split('/')) {
    walked = `${walked}/${name}`
    entries.push({ path: walked, name })
  }
  return entries
}

/** Puts the browsed folder in the address bar without adding a history entry:
 *  the back button belongs to the browser, not to the folder stack, and the
 *  page's own back control is the one that walks up. */
function rememberFolder(path, rootPath, open) {
  if (typeof window === 'undefined') return
  const url = new URL(window.location.href)
  const inFolder = !!path && path !== rootPath
  if (inFolder) url.searchParams.set('path', path)
  else url.searchParams.delete('path')
  // A bare link is the card closed: the visitor opens the list themselves. The
  // flag only appears once they have, so a reload (or a copied link) keeps the
  // list they were looking at open.
  if (!inFolder && open) url.searchParams.set('browse', '1')
  else url.searchParams.delete('browse')
  const next = `${url.pathname}${url.search}`
  if (next !== `${window.location.pathname}${window.location.search}`) {
    window.history.replaceState(window.history.state, '', next)
  }
}

/** Puts plain text on the clipboard and reports whether it landed.
 *
 *  `navigator.clipboard` needs a secure context, and a share link is routinely
 *  opened from a plain-http LAN address, where the object does not exist at all.
 *  Even on https a browser may refuse the write. Both leave the button doing
 *  nothing visible, so the selection trick has to exist, and the result has to
 *  be reported rather than assumed. */
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // The refusal is not a reason to give up; fall through to the copy command.
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.cssText = 'position:fixed;top:-1000px;left:-1000px;opacity:0'
    document.body.appendChild(area)
    area.select()
    area.setSelectionRange(0, text.length)
    const copied = document.execCommand('copy')
    document.body.removeChild(area)
    return copied
  } catch {
    return false
  }
}

/** Where a verified share password is kept for the life of the tab.
 *
 *  A refresh is the same visit, so asking again is asking for something the
 *  visitor just gave. A new tab, another browser or another device is a
 *  different visit, and `sessionStorage` is exactly that boundary: it survives
 *  a reload and dies with the tab.
 *
 *  The password is stored as typed rather than as a token because the API asks
 *  for it on every request — listing, browsing, preview, download and upload
 *  all carry it — and the server keeps no unlock session to hand back. */
const unlockKey = (token) => `yuarcloud:share-unlock:${token}`

function rememberUnlock(token, password) {
  try {
    sessionStorage.setItem(unlockKey(token), password)
  } catch {
    // Storage can be blocked or full. The page still works; the visitor just
    // types the password again after a reload.
  }
}

function readUnlock(token) {
  try {
    return sessionStorage.getItem(unlockKey(token)) || ''
  } catch {
    return ''
  }
}

function forgetUnlock(token) {
  try {
    sessionStorage.removeItem(unlockKey(token))
  } catch {
    // Nothing was stored, or storage is unreachable. Either way there is
    // nothing to clean up.
  }
}

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
  // The token whose answer is on screen. `file` and `token` are set by
  // different updates: on a client-side move from one share link to another,
  // the token changes first and the file still belongs to the previous link
  // for a moment. The contents effect below asks for a folder path, and asking
  // this link for another link's path can only be refused.
  const [fileToken, setFileToken] = useState(null)
  const [contentsLoading, setContentsLoading] = useState(false)
  const [contentsError, setContentsError] = useState(null)
  // Closed on arrival. A folder share is opened to hand someone a single
  // thing — download it, or copy it — and the list of everything else was
  // pushing the card past the bottom of a laptop window before the visitor had
  // asked for it. The visitors who want it press Browse, and the rows are
  // already fetched by then.
  const [showContents, setShowContents] = useState(false)
  // Read by the folder fetch when it writes the address bar. The fetch is async
  // and its callback is frozen at the identity of the component at call time, so
  // it cannot see the current `showContents` directly.
  const showContentsRef = useRef(false)
  // The folder whose rows are in the list. Keying the list on it remounts the
  // list when a different folder's answer lands, which is what replays the
  // fade-in that keeps the swap from reading as a jump cut. A refresh of the
  // same folder does not bump it, so nothing re-fades for no reason.
  const [contentsPaint, setContentsPaint] = useState(0)
  const paintedPath = useRef(null)
  const contentsListRef = useRef(null)
  // Height of the list before the rows were replaced, and the element that had
  // it. Read in `openFolder` while the old folder is still on screen, spent by
  // the layout effect below once the new rows are in the document.
  const swapFrom = useRef(null)
  // The shared folder's own path, kept in a ref so the folder fetch can write
  // the address bar without taking `file` as a dependency and changing identity
  // on every unlock.
  const rootPathRef = useRef(null)
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

  // What the visitor is looking at right now. A result that lands on the same
  // view is committed without animating: a rejected password leaves the same
  // form up, and fading that form out and back in would read as a glitch.
  const shownView = useRef('loading')

  const load = useCallback(
    async (pwd = '', { spinner = true, remembered = false } = {}) => {
      const id = ++requestId.current
      if (spinner) {
        setLoading(true)
        // Entering the wait is itself a change of view. It matters when a second
        // share link is opened without a reload: the card is gone from the
        // screen, so its return has to animate rather than cut back in.
        shownView.current = 'loading'
        // Nothing from the previous link outlives the wait either. Its paths
        // belong to another share, and the contents effect below would ask this
        // token for them — a request that can only be refused.
        setShare(null)
        setFile(null)
        setContents([])
        setTrail([])
        setContentsError(null)
      }

      // The whole outcome is worked out before anything is set, so the result
      // can land in one commit — and that commit can be wrapped in a single
      // transition. Setting one state at a time would let the browser
      // photograph a half-updated page.
      let next
      try {
        const response = await shareApi.access(token, pwd)
        if (id !== requestId.current) return
        next = {
          share: response.data.share,
          file: response.data.file,
          error: null,
          needsPassword: false,
          passwordError: false,
          view: 'content',
        }
        // Keep the open password for this tab, or drop a stale one: a link that
        // is no longer protected should not leave a secret sitting in storage.
        if (response.data.share?.has_password && pwd) rememberUnlock(token, pwd)
        else forgetUnlock(token)
      } catch (err) {
        if (id !== requestId.current) return
        if (err.response?.status === 401) {
          // A remembered password that no longer works — rotated, or the link
          // was re-protected — must go, or every reload would retry it. It is
          // also not the visitor's mistake, so the form carries no error.
          forgetUnlock(token)
          next = {
            needsPassword: true,
            error: 'This link is password protected.',
            // An untouched form is not a failure; a password that was sent and
            // refused is, and the field has to say so.
            passwordError: pwd !== '' && !remembered,
            view: 'password',
          }
        } else if (err.response?.status === 410) {
          next = {
            error: 'This share link has expired.',
            needsPassword: false,
            passwordError: false,
            view: 'error',
          }
        } else if (err.response?.status === 403) {
          forgetUnlock(token)
          next = {
            error: 'This share link is no longer available.',
            needsPassword: false,
            passwordError: false,
            view: 'error',
          }
        } else {
          next = {
            error: apiError(err, 'This share link could not be opened.'),
            needsPassword: false,
            passwordError: false,
            view: 'error',
          }
        }
      }

      const commit = () => {
        if (next.share) {
          setShare(next.share)
          setFile(next.file)
          setFileToken(token)
          // Every download, preview and upload sends this password. Restoring
          // the unlock without restoring this would reopen the card and then
          // refuse the very actions it offers.
          if (pwd) setPassword(pwd)
        }
        if (next.error !== undefined) setError(next.error)
        if (next.needsPassword !== undefined) setNeedsPassword(next.needsPassword)
        setPasswordError(next.passwordError ?? false)
        // Cleared here rather than in a `finally`: the spinner has to be gone
        // in the same commit as the view it was standing in for, or the
        // transition's "after" picture is still the spinner.
        setLoading(false)
      }

      if (next.view === shownView.current) commit()
      else withViewTransition(commit)
      shownView.current = next.view
    },
    [token]
  )

  useEffect(() => {
    const remembered = readUnlock(token)
    load(remembered, { remembered: remembered !== '' })
  }, [load, token])

  const submitPassword = async (e) => {
    e.preventDefault()
    setSubmitting(true)
    try {
      // The form stays where it is while the password is checked. Replacing the
      // whole card with the loading state for one round trip was a blink, and
      // the button already says "Checking…".
      await load(password, { spinner: false })
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
        // The card is as tall as its rows, so a folder with fewer of them makes
        // it jump in the same frame the rows are swapped — the one part of the
        // swap a cross-fade cannot hide. Measure while the old rows are still
        // laid out; the effect below walks the height to its new value.
        swapFrom.current = contentsListRef.current?.getBoundingClientRect().height ?? null
        if (paintedPath.current !== path) {
          paintedPath.current = path
          setContentsPaint((n) => n + 1)
        }
        setContents(response.data.items || [])
        // The folder is on screen, so the address bar can name it: a reload
        // comes back here, and copying the link while browsing hands someone
        // the same folder rather than the top of the share.
        rememberFolder(path, rootPathRef.current, showContentsRef.current)
      } catch (err) {
        if (id !== contentsSeq.current) return
        // A folder named by the address bar can be gone — deleted since the
        // link was copied, or hand-edited. Stop pointing the URL at it so a
        // reload does not repeat a failure the visitor can only back out of.
        if (err?.response?.status === 404 && path !== rootPathRef.current) {
          rememberFolder(rootPathRef.current, rootPathRef.current, false)
        }
        setContentsError(apiError(err, 'Could not load this folder'))
      } finally {
        if (id === contentsSeq.current) setContentsLoading(false)
      }
    },
    [token, password]
  )

  // The shared root is the first folder every visitor sees. Running this on
  // the file id (not on every render) keeps a password change, which produces
  // a new file object, from re-fetching contents twice. The token guard is what
  // keeps it from firing against the wrong link while a move is in flight.
  useEffect(() => {
    if (!file?.is_dir) return
    if (fileToken !== token) return
    setContents([])
    rootPathRef.current = file.path

    // A reload, or a link copied while browsing, names a folder rather than
    // the root. Land there, with the trail already filled in so the breadcrumb
    // and the back control describe where the visitor is.
    const wanted = folderFromUrl(file.path)
    const openOnArrival = !!wanted || openFromUrl()
    showContentsRef.current = openOnArrival
    setShowContents(openOnArrival)
    if (wanted) {
      setTrail(trailTo(file.path, file.name, wanted))
      openFolder(wanted, wanted.slice(wanted.lastIndexOf('/') + 1))
      return
    }
    setTrail([])
    openFolder(file.path, file.name)
  }, [file?.id, file?.path, file?.name, file?.is_dir, fileToken, token, openFolder])

  // Walks the list from the height it had to the height its rows now need.
  // The rows themselves do not move: the list is top-aligned, so only its
  // bottom edge (and with it the upload box below) travels. Opacity alone
  // could not cover this — a card that shrinks by 84px in one frame reads as
  // a jolt even when the text inside it is fading.
  //
  // Web Animations rather than a CSS transition because the start value is
  // only knowable at runtime and the end value is `auto`; a keyframe pair of
  // two measured lengths needs neither `interpolate-size` (Chrome-only) nor a
  // temporary inline height left behind on the element.
  useLayoutEffect(() => {
    const el = contentsListRef.current
    const from = swapFrom.current
    swapFrom.current = null
    // No previous height means there was nothing on screen to move away from.
    if (!el || !from) return
    const to = el.getBoundingClientRect().height
    if (Math.abs(to - from) < 1) return
    if (!window.matchMedia('(prefers-reduced-motion: no-preference)').matches) return

    const style = getComputedStyle(el)
    // A computed custom property comes back normalised, so the token may read
    // `0.18s` rather than `180ms`. Taking it at face value hands the animation
    // a 0.18ms duration — no animation at all.
    const durationToken = style.getPropertyValue('--contents-enter-duration').trim() || '180ms'
    const amount = parseFloat(durationToken)
    const duration = Number.isFinite(amount)
      ? (durationToken.endsWith('ms') ? amount : amount * 1000)
      : 180
    const easing = style.getPropertyValue('--share-enter-ease').trim() || 'ease-out'

    // An auto-overflow list whose box is momentarily shorter than its rows
    // would grow a scrollbar mid-flight on the platforms that reserve space for
    // one. The clip is the point of the animation, so hide the overflow only
    // for its duration.
    el.style.overflowY = 'hidden'
    const animation = el.animate(
      [{ height: `${from}px` }, { height: `${to}px` }],
      { duration, easing }
    )
    const restore = () => { el.style.overflowY = '' }
    animation.onfinish = restore
    animation.oncancel = restore
  }, [contentsPaint])

  // Showing and hiding is also an address-bar change: the visitor who opened
  // the list and then reloaded expects the list, and the one who never opened
  // it does not want a link that opens it for them.
  const toggleContents = () => {
    const next = !showContents
    showContentsRef.current = next
    const rootPath = rootPathRef.current
    const current = trail.length ? trail[trail.length - 1].path : rootPath
    rememberFolder(next ? current : null, rootPath, next)
    setShowContents(next)
  }

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
    const copied = await copyToClipboard(window.location.href)
    if (copied) toast.success('Link copied')
    else toast.error('Could not copy — select the address bar instead')
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

  // The brand is the only thing that is known before the access request lands,
  // so that is all the wait shows. A spinner and the words "Loading shared
  // file…" stood below it and announced a wait that lasts a few frames — and
  // then vanished in the same breath as the card, which says the same thing by
  // arriving. Nothing is dimmed or stood in for here: the card is the view, and
  // the view transition is what carries it in.
  if (loading) {
    return (
      <div className="centered-page">
        <div className="share-shell">
          <ShareBrand />
        </div>
      </div>
    )
  }

  if (needsPassword) {
    return (
      <div className="centered-page">
        <div className="share-shell">
          <ShareBrand />
          <div key="card" className="card share-card share-view">
            <h1 className="share-state-title">This link is protected</h1>
            <p className={`share-state-text${passwordError ? ' is-error' : ''}`}>
              {passwordError
                ? 'That password is incorrect. Try again.'
                : 'Enter the password to access the shared file.'}
            </p>

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
          <div key="card" className="card share-card share-view" style={{ textAlign: 'center' }}>
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
        <div key="card" className="card share-file-card share-view">
          <div className="share-file-head">
            <div className={`file-icon share-icon ${iconClassFor(file)}`}>
              <FileIcon item={file} />
            </div>
            <div style={{ flex: 1, minWidth: 0 }}>
              <h1 className="share-file-name">
                {file.name}
              </h1>
              <div className="share-file-meta">
                {/* A folder is already a folder from its icon and the Download
                    hint, so the word said nothing the row did not. */}
                {!file.is_dir && <span>{formatBytes(file.size)}</span>}
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
                onClick={toggleContents}
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
                    ? ''
                    : `${contents.length} item${contents.length === 1 ? '' : 's'}`}
                </span>
              </div>

              {/* A swap in flight dims what is on screen instead of replacing it
                  with a loading line. The Drive does the same, and for the same
                  reason: the line is gone before it can be read, so all it adds
                  is a flash where the list used to be. */}
              <div
                className="share-contents-list"
                key={contentsPaint}
                ref={contentsListRef}
                style={{ opacity: contentsLoading ? 0.55 : 1, transition: 'opacity 140ms ease' }}
              >
                {contentsError ? (
                  <div className="share-contents-empty share-contents-error">{contentsError}</div>
                ) : contents.length === 0 ? (
                  // An empty result is only empty once a result has arrived. On
                  // a cold load the box holds nothing, rather than asserting
                  // "This folder is empty." while the folder is on its way.
                  contentsLoading ? null : (
                    <div className="share-contents-empty">This folder is empty.</div>
                  )
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

      {/* The app's notifications live inside Layout, and this page deliberately
          sits outside it. Without a host here, "Link copied" went nowhere: the
          visitor pressed Copy, nothing appeared, and the button read as broken
          — which it also was wherever the clipboard API is unavailable. */}
      <ToastHost />

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
