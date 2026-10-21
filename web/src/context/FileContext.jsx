import React, { createContext, useContext, useState, useCallback, useEffect, useRef, useMemo } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { useAuth } from './AuthContext'
import { fileApi, apiError } from '../utils/api'
import { uploadFilesSequentially } from '../utils/upload'
import { createSpeedTracker, etaFromRate } from '../utils/uploadSpeed'
import toast from 'react-hot-toast'

const FileContext = createContext(null)

/**
 * Look up an existing folder by its path, relative to the upload destination.
 *
 * Needed when creating a folder from a picked tree returns 409 — the directory
 * is already there, which is a success for our purposes, but the files inside
 * still need its id. Without this the subtree flattens into the destination,
 * which is the exact failure the tree walk exists to prevent.
 */
async function findFolderByPath(relPath, baseParentPath) {
  try {
    const response = await fileApi.folderFlat()
    const wanted = baseParentPath ? `${baseParentPath}/${relPath}` : relPath
    return (response.data?.folders || []).find((f) => f.path === wanted) || null
  } catch {
    return null
  }
}

/**
 * A "view" is what the file browser is currently showing.
 *   - scope 'drive'   : the folder tree at currentPath
 *   - scope 'trash'   : flat list of trashed items
 *   - scope 'search'  : flat list of search hits
 *
 * "you are looking in the wrong place".
 */

/**
 * Read a view out of the URL. Every scope switch pushes a route, so the address
 * bar is the only place that decides what the browser shows — which is what
 * makes a reload land back where the user was. Before this the scope lived in
 * React state alone: a refresh on the trash sent you to the root.
 *
 * Returns null for routes the file browser does not own. The provider wraps the
 * whole app, so it is mounted on /admin and /login and /yc/:token too, and
 * anything that does not return null here gets read as a folder path — which is
 * how "directory not found" appeared on the settings page: /admin was parsed as
 * a folder called "admin" and requested from the file API.
 */
export function viewFromLocation(pathname, search) {
  const params = new URLSearchParams(search)

  if (pathname.startsWith('/trash')) return { scope: 'trash' }
  if (pathname.startsWith('/search')) {
    return { scope: 'search', query: params.get('q') || '' }
  }
  if (pathname !== '/cloud' && !pathname.startsWith('/cloud/')) return null

  // /cloud with no segment is the root, full stop. It must not fall back to
  // whatever folder was open before: that made "My Cloud" a no-op when the
  // previous view happened to be the same string, so clicking it issued no
  // request at all and left the old error on screen.
  const raw = pathname.replace(/^\/cloud\/?/, '')
  // decodeURIComponent per segment, not on the whole string: a folder may itself
  // contain a slash (%2F), and decoding the joined path would split it into two
  // levels that do not exist. An undecodable segment (a stray %) is left as-is
  // rather than throwing and blanking the browser.
  const path = raw
    .split('/')
    .map((seg) => {
      try {
        return decodeURIComponent(seg)
      } catch {
        return seg
      }
    })
    .join('/')

  return { scope: 'drive', path }
}

/** Build the route for a drive folder. */
export function driveHref(path) {
  return path ? `/cloud/${path}` : '/cloud'
}

export function FileProvider({ children }) {
  const navigate = useNavigate()
  const location = useLocation()

  const [items, setItems] = useState([])
  const [view, setView] = useState({ scope: 'drive', path: '', folderId: null })
  const [parentFolder, setParentFolder] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)

  const [viewMode, setViewModeState] = useState(() => localStorage.getItem('viewMode') || 'grid')
  const setViewMode = useCallback((mode) => {
    localStorage.setItem('viewMode', mode)
    setViewModeState(mode)
  }, [])

  const [selected, setSelected] = useState(() => new Set())
  const [clipboard, setClipboard] = useState({ action: null, ids: [] })
  const [sort, setSort] = useState({ key: 'name', direction: 'asc' })

  const [uploads, setUploads] = useState([])
  const [searchQuery, setSearchQuery] = useState('')

  const requestId = useRef(0)

  // ---------------------------------------------------------------- loading

  const load = useCallback(async (nextView) => {
    const target = nextView ?? view
    const id = ++requestId.current

    // No session means nothing to list. Skipping the request keeps the login
    // page quiet — otherwise every anonymous visit fired a 401 that surfaced
    // as "authorization header required".
    if (!localStorage.getItem('access_token')) {
      setItems([])
      setLoading(false)
      return
    }

    setLoading(true)
    setError(null)

    try {
      let response
      switch (target.scope) {
        case 'trash':
          response = await fileApi.listTrash()
          break
        case 'search':
          response = await fileApi.search(target.query || '', 200)
          break
        default: {
          const params = { path: target.path || '/' }
          if (target.folderId) params.parent_id = target.folderId
          response = await fileApi.list(params)
        }
      }

      // A newer request already landed; drop this stale response.
      if (id !== requestId.current) return

      setItems(response.data.files || [])
      setParentFolder(response.data.parent || null)
      if (target.scope === 'drive') {
        // Take the folder id from the listing response rather than from the
        // caller. Navigation is URL-driven, so the caller often only knows the
        // path; without this the view keeps folderId null and every upload
        // lands in My Cloud instead of the folder being browsed.
        setView({
          scope: 'drive',
          path: response.data.path || '',
          folderId: response.data.parent?.id ?? null,
        })
      }
      setSelected(new Set())
    } catch (err) {
      if (id !== requestId.current) return
      // An expired or absent session is not an error worth surfacing — the
      // axios interceptor clears it and the router sends you to /login.
      if (err.response?.status === 401) {
        setItems([])
        setError(null)
        return
      }
      const message = apiError(err, 'Failed to load files')
      setError(message)
      setItems([])
      toast.error(message)
    } finally {
      if (id === requestId.current) setLoading(false)
    }
  }, [view])

  // Just the listing.
  //
  // This also fetched /files/stats and stored it in state that nothing ever
  // rendered — the storage figure in the panel comes from /admin/stats, and
  // the drive has no status bar. So every upload, move, rename, restore and
  // trash paid for a second request whose result was discarded, and the state
  // it filled was the thing that looked frozen. Removed rather than refreshed:
  // there is nothing to refresh into.
  const { refreshUser } = useAuth()

  const refresh = useCallback(async () => {
    await load()
  }, [load])

  // Re-reads the account alongside the listing.
  //
  // The status bar's "N used" comes from user.used_bytes, which AuthContext reads
  // once at login. The server keeps that counter correct -- uploads, folder
  // copies, the share upload and every trash operation all adjust it -- but the
  // copy sitting in the client was written at sign-in and nothing touched it
  // again. So uploading four megabytes left the bar reading "12 B used" until
  // the page was reloaded, which made the figure look like a bug in the quota
  // check when it was only a stale field.
  //
  // Both requests go together rather than one after the other: they are
  // independent, and the bar and the listing should settle in the same tick so
  // the two halves of the status line never disagree mid-update.
  const reloadUsage = useCallback(async () => {
    await Promise.all([load(), refreshUser()])
  }, [load, refreshUser])

  // The URL is the source of truth. Every scope switch and folder change navigates,
  // and this effect turns whatever route is on screen into the load it calls for.
  // That is what makes a refresh land back where the user was — the trash, the
  //
  // Re-entry has to reload, not just a changed URL. Leaving the drive's own
  // routes (/admin, /login) puts the view out of step with the address bar, and
  // coming back to /cloud then sees the URL it already knew — skipping that is
  // how the browser sat on the previous folder's error without asking for
  // anything. Keying the skip on the URL rather than on the view's contents also
  // keeps an empty folder from looping: loading calls setView, the view is a
  // dependency, so a guard keyed on what the view holds would re-fire forever on
  // a folder that stays empty.
  const loadedRouteRef = useRef(null)
  const onDriveRouteRef = useRef(false)

  // Holds the recent-bytes history behind each upload's speed figure. A ref
  // because it is written on every progress event and only ever read while
  // rendering the derived rate.
  const speedTrackerRef = useRef(null)
  if (speedTrackerRef.current === null) speedTrackerRef.current = createSpeedTracker()

  useEffect(() => {
    const target = viewFromLocation(location.pathname, location.search)

    // Not a file-browser route. Leave the view alone and mark that we are off
    // the drive, so the next entry reloads whatever it lands on.
    if (!target) {
      onDriveRouteRef.current = false
      return
    }

    const route = location.pathname + location.search
    if (onDriveRouteRef.current && loadedRouteRef.current === route) return
    onDriveRouteRef.current = true
    loadedRouteRef.current = route

    if (target.scope === 'trash') {
      setView({ scope: 'trash' })
      load({ scope: 'trash' })
      return
    }

    if (target.scope === 'search') {
      // An empty query has nothing to ask for; fall back to the root rather than
      // firing a search that would match everything.
      if (!target.query) {
        navigate('/cloud', { replace: true })
        return
      }
      setView({ scope: 'search', query: target.query })
      setSearchQuery(target.query)
      load({ scope: 'search', query: target.query })
      return
    }

    // Drive. `folderId` is left alone on purpose: it is cached identity, and
    // re-resolving it on every path change would mean a lookup request per
    // keystroke of navigation for no gain — the path is already the key.
    const folderId = view.scope === 'drive' && view.path === target.path ? view.folderId : null
    setView({ scope: 'drive', path: target.path, folderId })
    load({ scope: 'drive', path: target.path, folderId })
  }, [location.pathname, location.search, load, navigate, view.scope, view.path, view.folderId])

  // ---------------------------------------------------------------- navigation

  const openFolder = useCallback((file) => {
    if (!file?.is_dir) return
    // Navigate rather than set state: the route is what a reload will replay, so
    // writing the view without writing the route would put the two out of step.
    navigate(driveHref(file.path))
  }, [navigate])

  const goToPath = useCallback((path) => {
    navigate(driveHref(path))
  }, [navigate])

  const showTrash = useCallback(() => {
    navigate('/trash')
  }, [navigate])

  const showSearch = useCallback((query) => {
    navigate(`/search?q=${encodeURIComponent(query)}`)
  }, [navigate])

  const showDrive = useCallback(() => {
    navigate('/cloud')
  }, [navigate])

  // ---------------------------------------------------------------- selection

  // Shift-range extends from an anchor, and the anchor has to be remembered
  // rather than inferred. It used to be read back out of the Set as
  // `prev.values().next().value`, which is the *first inserted* id, not the
  // last clicked one: click a, Ctrl-click c, Shift-click e and the range ran
  // from a to e — silently including b, which was never touched. Set insertion
  // order and click order agree exactly once, on the first click.
  const anchorRef = useRef(null)

  const select = useCallback((id, { additive = false, range = false } = {}) => {
    // Shift does not move the anchor, which is what makes repeated shift-clicks
    // extend the same window instead of walking it one row per click.
    if (!range) anchorRef.current = id

    setSelected((prev) => {
      const next = new Set(prev)
      if (range && prev.size > 0) {
        const from = items.findIndex((f) => f.id === (anchorRef.current ?? id))
        const target = items.findIndex((f) => f.id === id)
        if (from !== -1 && target !== -1) {
          const [lo, hi] = from < target ? [from, target] : [target, from]
          items.slice(lo, hi + 1).forEach((f) => next.add(f.id))
          return next
        }
      }
      if (additive) {
        if (next.has(id)) next.delete(id)
        else next.add(id)
      } else {
        next.clear()
        next.add(id)
      }
      return next
    })
  }, [items])

  const selectAll = useCallback(() => setSelected(new Set(items.map((f) => f.id))), [items])
  const clearSelection = useCallback(() => {
    // Clearing drops the anchor too. Left in place, a shift-click after an
    // Escape would range-select from something that is no longer selected.
    anchorRef.current = null
    setSelected(new Set())
  }, [])
  // Rubber-band selection commits a whole set at once. Calling select() per id
  // would re-render once per tile and, worse, run the anchor bookkeeping per
  // id — the anchor is the *last* item the pointer touched, not the last id in
  // DOM order that happened to be committed last.
  const selectMany = useCallback((ids, { additive = false } = {}) => {
    const list = Array.from(ids)
    if (!additive) anchorRef.current = list.length ? list[list.length - 1] : null
    setSelected((prev) => {
      const next = additive ? new Set(prev) : new Set()
      for (const id of list) next.add(id)
      return next
    })
  }, [])
  const isSelected = useCallback((id) => selected.has(id), [selected])
  const selectedItems = useMemo(() => items.filter((f) => selected.has(f.id)), [items, selected])

  // ---------------------------------------------------------------- sorting

  const toggleSort = useCallback((key) => {
    setSort((prev) => ({
      key,
      direction: prev.key === key && prev.direction === 'asc' ? 'desc' : 'asc',
    }))
  }, [])

  const sortedItems = useMemo(() => {
    const factor = sort.direction === 'asc' ? 1 : -1
    return [...items].sort((a, b) => {
      // Folders always lead, regardless of the sort direction.
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1
      let av = a[sort.key]
      let bv = b[sort.key]
      if (sort.key === 'name') {
        av = String(av).toLowerCase()
        bv = String(bv).toLowerCase()
      }
      if (av === bv) return a.name.localeCompare(b.name)
      return av > bv ? factor : -factor
    })
  }, [items, sort])

  // ---------------------------------------------------------------- mutations

  // `destination` is optional and exists for the views that are not locations.
  //
  // parent — the caller asks where first and passes the answer. Without it the
  // request silently landed in My Cloud, from a screen showing something else
  // entirely.
  const createFolder = useCallback(async (name, destination) => {
    const trimmed = (name || '').trim()
    if (!trimmed) return null

    // A folder can only be created inside the real drive tree, or where the
    // caller explicitly said to put it.
    //
    // The keys here are `id` and `path`, which is what the request below and
    // every other destination in this file reads. They were `parent_id` and
    // `parent_path`, so `where.id` and `where.path` were always undefined: the
    // folder was filed in My Cloud no matter which folder was on screen, and
    // the caller's own destination would have been dropped the same way.
    const where = destination || (view.scope === 'drive'
      ? { id: view.folderId, path: view.path }
      : { id: null, path: '' })

    try {
      const response = await fileApi.createFolder({
        name: trimmed,
        parent_id: where.id ?? null,
        parent_path: where.path ?? '',
      })
      // Refresh in place so the view stays on whatever the user was looking at.
      await load()
      toast.success(`Folder "${trimmed}" created`)
      return response.data
    } catch (err) {
      toast.error(apiError(err, 'Failed to create folder'))
      throw err
    }
  }, [view, load])

  // A folder pick arrives as a flat FileList whose only record of structure is
  // webkitRelativePath. Uploading those files straight into the current folder
  // flattens the whole tree — every photo from photos/2024/summer/ landed side
  // by side in My Cloud, and folders full of identically-named files silently
  // overwrote each other's meaning on the way. The path is there; it was just
  // never read.
  const uploadFiles = useCallback(async (fileList, destination) => {
    const list = Array.from(fileList || [])
    if (!list.length) return

    // Build the directory tree the pick implies, shallowest first so a child
    // never waits on a parent that does not exist yet. Each level's parents are
    // created by the iteration before it.
    const dirOf = (file) => {
      const rel = file.webkitRelativePath || ''
      const cut = rel.lastIndexOf('/')
      return cut < 0 ? '' : rel.slice(0, cut)
    }

    const dirPaths = [...new Set(list.map(dirOf).filter(Boolean))]
      .sort((a, b) => a.split('/').length - b.split('/').length || a.localeCompare(b))

    // `destination` overrides the current folder, for the views that are not
    // hands the answer over. Silently falling back to the root would file the
    // upload somewhere the user never chose and never saw.
    const baseParentId = destination
      ? destination.id ?? null
      : view.scope === 'drive' ? view.folderId : null
    const baseParentPath = destination
      ? destination.path || ''
      : view.scope === 'drive' ? view.path || '' : ''

    // dirId: absolute path (relative to the destination) -> created folder id
    const dirId = new Map([['', baseParentId]])

    for (const dirPath of dirPaths) {
      const parentPath = dirPath.includes('/') ? dirPath.slice(0, dirPath.lastIndexOf('/')) : ''
      const name = dirPath.slice(dirPath.lastIndexOf('/') + 1)
      try {
        const response = await fileApi.createFolder({
          name,
          parent_id: dirId.get(parentPath) ?? null,
          parent_path: parentPath ? `${baseParentPath}/${parentPath}` : baseParentPath,
        })
        dirId.set(dirPath, response.data?.id ?? null)
      } catch (err) {
        // A 409 here means the folder is already there, which is a success for
        // our purposes — but we need its id to file the contents into it.
        const existing = await findFolderByPath(dirPath, baseParentPath)
        if (!existing) {
          toast.error(apiError(err, `Could not create folder "${dirPath}"`))
          // Anything inside this directory would land in the wrong place.
          // Stop the tree here rather than flattening it by accident.
          dirId.set(dirPath, null)
          continue
        }
        dirId.set(dirPath, existing.id)
      }
    }

    const jobs = list.map((file, i) => ({
      // Not crypto.randomUUID(): that is [SecureContext], so it is undefined on
      // plain HTTP over a LAN address — which is exactly how a self-hosted
      // server is usually reached. The throw happened before any tray row
      // existed and no caller passes a .catch, so the upload silently did
      // nothing at all. The index keeps same-named files distinct, which is the
      // job the id was actually doing.
      id: `${file.name}-${file.size}-${file.lastModified}-${i}`,
      name: file.name,
      // Show where it is going. A folder pick with three report.doc in it is
      // otherwise three identical rows in the tray.
      label: file.webkitRelativePath || file.name,
      size: file.size,
      progress: 0,
      // Bytes actually sent, and what that works out to as a rate. Both are
      // absent rather than zero until there is enough evidence: a rate of 0
      // and "no rate yet" read very differently, and the first few hundred
      // milliseconds cannot tell them apart.
      loaded: 0,
      bytesPerSec: null,
      etaSec: null,
      status: 'pending',
      error: null,
    }))
    setUploads((prev) => [...prev, ...jobs])

    // Job identity is the index, not the name. Two files with the same basename in
    // one pick — a folder upload of a/report.doc and b/report.doc, or two
    // picks while the first is still running — matched on name, so one success
    // marked both rows done: a failed upload was shown as Complete, and the
    // cleanup timer then removed the error row along with it.
    const jobIdAt = (index) => `${list[index]?.name}-${list[index]?.size}-${list[index]?.lastModified}-${index}`

    // Sample buffers for the rate, kept in a ref because they are written on
    // every progress event and never rendered directly.
    const speed = speedTrackerRef.current
    jobs.forEach((job) => speed.reset(job.id))

    const { results, failures } = await uploadFilesSequentially(list, {
      parentIdFor: (file) => dirId.get(dirOf(file)) ?? baseParentId,
      onUpdate: ({ index, progress, loaded, status, message }) => {
        const id = jobIdAt(index)
        const job = jobs.find((j) => j.id === id)
        const bytesPerSec = status === 'uploading' && loaded != null ? speed.push(id, loaded) : null
        setUploads((prev) =>
          prev.map((j) =>
            j.id === id
              ? {
                  ...j,
                  progress,
                  loaded: loaded ?? j.loaded,
                  // Cleared on completion rather than frozen: a finished upload
                  // reporting its final speed reads as still running.
                  bytesPerSec: status === 'uploading' ? bytesPerSec : null,
                  etaSec:
                    status === 'uploading' && bytesPerSec
                      ? etaFromRate(bytesPerSec, loaded ?? j.loaded, job?.size ?? 0)
                      : null,
                  status,
                  error: message || null,
                }
              : j
          )
        )
      },
    })

    setUploads((prev) =>
      prev.map((job) =>
        results.some((r) => jobIdAt(r.index) === job.id) ? { ...job, status: 'done', progress: 100 } : job
      )
    )

    if (results.length) {
      toast.success(`Uploaded ${results.length} file${results.length > 1 ? 's' : ''}`)
    }
    for (const failure of failures) {
      toast.error(apiError(failure.error, `Failed to upload ${failure.file.name}`))
    }

    await reloadUsage()

    // Finished rows stay. They used to be swept away four seconds later, which
    // meant the tray emptied itself exactly when someone uploading a folder of
    // forty files was trying to check they had all arrived — the last few
    // landed with nobody left to see them land. Drive keeps them: the panel is
    // the receipt, and the dismiss button is how it is closed.
    return { results, failures }
  }, [view, reloadUsage])

  const clearUploads = useCallback(() => setUploads([]), [])

  const removeItems = useCallback(async (ids) => {
    const list = Array.from(ids || [])
    if (!list.length) return

    // Inside the trash the only sensible action is a permanent delete.
    const permanent = view.scope === 'trash'

    try {
      for (const id of list) {
        if (permanent) await fileApi.deletePermanent(id)
        else await fileApi.remove(id, false) // moves to trash
      }
      toast.success(
        permanent
          ? `Permanently deleted ${list.length} item${list.length > 1 ? 's' : ''}`
          : `Moved ${list.length} item${list.length > 1 ? 's' : ''} to trash`
      )
      setSelected(new Set())
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to delete'))
      throw err
    }
  }, [view.scope, reloadUsage])

  const restoreItems = useCallback(async (ids) => {
    const list = Array.from(ids || [])
    if (!list.length) return
    try {
      for (const id of list) await fileApi.restore(id)
      toast.success(`Restored ${list.length} item${list.length > 1 ? 's' : ''}`)
      setSelected(new Set())
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to restore'))
      throw err
    }
  }, [reloadUsage])

  const emptyTrash = useCallback(async () => {
    try {
      const response = await fileApi.emptyTrash()
      toast.success(`Emptied trash (${response.data.removed} rows)`)
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to empty trash'))
    }
  }, [reloadUsage])

  const renameItem = useCallback(async (id, name) => {
    const trimmed = (name || '').trim()
    if (!trimmed) return
    try {
      await fileApi.rename(id, trimmed)
      toast.success('Renamed')
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to rename'))
      throw err
    }
  }, [reloadUsage])

  const moveItems = useCallback(async (ids, destination) => {
    const list = Array.from(ids || [])
    if (!list.length) return
    try {
      for (const id of list) await fileApi.move(id, destination)
      toast.success(`Moved ${list.length} item${list.length > 1 ? 's' : ''}`)
      setSelected(new Set())
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to move'))
      throw err
    }
  }, [reloadUsage])

  const copyItems = useCallback(async (ids, destination) => {
    const list = Array.from(ids || [])
    if (!list.length) return
    try {
      for (const id of list) await fileApi.copy(id, destination)
      toast.success(`Copied ${list.length} item${list.length > 1 ? 's' : ''}`)
      await reloadUsage()
    } catch (err) {
      toast.error(apiError(err, 'Failed to copy'))
      throw err
    }
  }, [reloadUsage])


  // ---------------------------------------------------------------- clipboard

  const cutItems = useCallback((ids) => {
    setClipboard({ action: 'cut', ids: Array.from(ids) })
    toast.success(`Cut ${ids.size || ids.length} item(s)`)
  }, [])

  const copyToClipboard = useCallback((ids) => {
    setClipboard({ action: 'copy', ids: Array.from(ids) })
    toast.success(`Copied ${ids.size || ids.length} item(s)`)
  }, [])

  const clearClipboard = useCallback(() => setClipboard({ action: null, ids: [] }), [])

  const pasteInto = useCallback(async (targetFolderId = null, targetPath = '') => {
    if (!clipboard.action || !clipboard.ids.length) {
      toast('Clipboard is empty')
      return
    }
    const destination = { target_parent_id: targetFolderId, target_path: targetPath }
    if (clipboard.action === 'cut') {
      await moveItems(clipboard.ids, destination)
      clearClipboard()
    } else {
      await copyItems(clipboard.ids, destination)
    }
  }, [clipboard, moveItems, copyItems, clearClipboard])

  // ---------------------------------------------------------------- download

  const downloadItem = useCallback(async (item) => {
    if (item.is_dir) {
      triggerDownload(fileApi.downloadZipUrl(item.id), `${item.name}.zip`)
      return
    }
    try {
      const response = await fetch(fileApi.downloadUrl(item.id), {
        headers: { Authorization: `Bearer ${localStorage.getItem('access_token')}` },
      })
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      const blob = await response.blob()
      triggerDownload(URL.createObjectURL(blob), item.name, true)
    } catch {
      toast.error(`Failed to download ${item.name}`)
    }
  }, [])

  const value = {
    items: sortedItems,
    rawItems: items,
    view,
    parentFolder,
    loading,
    error,
    viewMode,
    setViewMode,
    selected,
    selectedItems,
    select,
    selectMany,
    selectAll,
    clearSelection,
    isSelected,
    clipboard,
    cutItems,
    copyToClipboard,
    clearClipboard,
    pasteInto,
    sort,
    toggleSort,
    uploads,
    clearUploads,
    searchQuery,
    setSearchQuery,
    openFolder,
    goToPath,
    showTrash,
    showSearch,
    showDrive,
    refresh,
    createFolder,
    uploadFiles,
    removeItems,
    restoreItems,
    emptyTrash,
    renameItem,
    moveItems,
    copyItems,
    downloadItem,
  }

  return <FileContext.Provider value={value}>{children}</FileContext.Provider>
}

function triggerDownload(href, filename, revoke = false) {
  const link = document.createElement('a')
  link.href = href
  link.download = filename
  document.body.appendChild(link)
  link.click()
  document.body.removeChild(link)
  if (revoke) setTimeout(() => URL.revokeObjectURL(href), 1000)
}

export function useFiles() {
  const ctx = useContext(FileContext)
  if (!ctx) throw new Error('useFiles must be used inside <FileProvider>')
  return ctx
}
