import { useState, useEffect, useCallback, useMemo } from 'react'
import { createPortal } from 'react-dom'
import {
  X, Download, ChevronLeft, ChevronRight, FileWarning,
} from 'lucide-react'
import { KIND, kindOf, canPreview, formatSize, previewGroupOf, GROUP_LABEL, GROUP_SINGULAR } from '../utils/fileType'

const SINGULAR_GROUP_LABEL = GROUP_SINGULAR
import { fileApi } from '../utils/api'
import { claimKeyboard } from '../utils/keyboardOwner'
import ImageViewer from './preview/ImageViewer'
import VideoPlayer from './preview/VideoPlayer'
import { TextViewer, SheetViewer, DocViewer, UnsupportedPreview } from './preview/ParsedViewers'

/**
 * The viewer every file opens in.
 *
 * Three things it has to get right:
 *
 *   - A file the browser cannot render must still land somewhere useful, so
 *     there is no dead end: the fallback states explain why and keep the
 *     download button in reach.
 *   - Navigation must not fight the browser. Escape, the arrow keys and the
 *     media player's own shortcuts all live in the same document, so keys are
 *     swallowed only when the viewer is open and only when nothing inside
 *     wants them (a text selection, a focused control).
 *   - The media URL is rebuilt whenever the item changes rather than cached,
 *     because the access token behind it can be rotated at any moment.
 */

// Moved above the component: it is read inside a useMemo, so the runtime was
// fine either way, but a module-level table belongs with the module's other
// tables rather than 230 lines below its first reader.
const KIND_LABEL = {
  [KIND.IMAGE]: 'Image',
  [KIND.VIDEO]: 'Video',
  [KIND.AUDIO]: 'Audio',
  [KIND.PDF]: 'PDF document',
  [KIND.TEXT]: 'Text',
  [KIND.SHEET]: 'Spreadsheet',
  [KIND.DOC]: 'Document',
  [KIND.ARCHIVE]: 'Archive',
  [KIND.BINARY]: 'File',
}

export default function PreviewModal({
  item, items, onNavigate, onClose, onDownload,
  // Overrides so the viewer can be pointed at a source other than the signed-in
  // user's own files — the share page passes the public /api/shares/... URLs,
  // which need no token because the share token in the path is the credential.
  streamUrlFor = fileApi.previewUrl,
  fetchUrlFor = fileApi.previewBlobUrl,
  // Share the page skips the token exchange entirely.
  needsPreviewToken = true,
}) {
  const kind = kindOf(item)

  const prev = useCallback(() => onNavigate(-1), [onNavigate])
  const next = useCallback(() => onNavigate(1), [onNavigate])

  // Media elements need a real URL in the src attribute, but the session token
  // must not travel there. Exchange it for a five-minute, single-file preview
  // token first; until that lands there is nothing to put in the src.
  const [previewToken, setPreviewToken] = useState(null)
  const [tokenError, setTokenError] = useState(null)

  // Claim the keyboard for as long as the viewer is on screen.
  //
  // Document-level shortcuts elsewhere guard against acting while a surface is
  // open, and they used to do it by querying a hardcoded list of overlay class
  // names. This one was never in that list, so Drive's Delete key fired through
  // the viewer and offered to trash the current selection. Claiming on mount
  // means a shortcut asks "is anything open?" rather than knowing what.
  useEffect(() => {
    const release = claimKeyboard('preview')
    return release
  }, [])

  useEffect(() => {
    if (!needsPreviewToken || !item?.id) {
      setPreviewToken(null)
      setTokenError(null)
      return undefined
    }

    let cancelled = false
    setPreviewToken(null)
    setTokenError(null)

    fileApi.previewToken(item.id)
      .then((res) => {
        if (!cancelled) setPreviewToken(res.data.token)
      })
      .catch((err) => {
        if (cancelled) return
        setTokenError(err.response?.data?.error || 'Could not open this file for preview')
      })

    return () => { cancelled = true }
  }, [item?.id, needsPreviewToken])

  const meta = useMemo(() => ({
    name: item?.name,
    size: formatSize(item?.size),
    kindLabel: KIND_LABEL[kind] || 'File',
    ext: item?.is_dir ? '' : (item?.name || '').split('.').pop()?.toUpperCase() || '',
  }), [item?.name, item?.size, item?.is_dir, kind])

  // `items` is already narrowed to the open file's series by the caller, so
  // the arrows only ever move within one kind. With a single member there is
  // nowhere to go and the counter is dropped along with them.
  const position = items?.findIndex((f) => f.id === item?.id) ?? -1
  const series = useMemo(() => {
    if (!items || items.length < 2 || position < 0) return null
    const group = previewGroupOf(item)
    if (!group) return null
    return {
      label: GROUP_LABEL[group],
      // Tooltips read "Next image", not "Next images".
      singular: SINGULAR_GROUP_LABEL[group] || GROUP_LABEL[group],
      total: items.length,
      at: position + 1,
    }
  }, [items, position, item])

  const seriesLabel = series ? `${series.label} ${series.at} of ${series.total}` : null

  // The signed-in viewer waits for the short-lived preview token because the
  // session token must not travel in a src attribute. A caller that already
  // has a credential-free URL (the share page) says so with
  // needsPreviewToken=false; its source is ready immediately. Before this,
  // that caller still waited for a token that never arrived, so images, PDFs,
  // video and audio sat on "Loading…" forever.
  const streamUrl = !item
    ? null
    : needsPreviewToken
      ? (previewToken ? streamUrlFor(item, previewToken) : null)
      : streamUrlFor(item)
  const blobUrl = item ? fetchUrlFor(item) : null

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onClose()
        return
      }
      // Never steal keys from a focused control inside the viewer.
      const tag = e.target?.tagName
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || e.target?.isContentEditable) {
        if (e.key !== 'Escape') return
      }

      if (e.key === 'ArrowLeft') { e.preventDefault(); prev() }
      else if (e.key === 'ArrowRight') { e.preventDefault(); next() }
    }

    // Capture phase so the Drive's own arrow/Enter shortcuts do not fire.
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [onClose, prev, next])

  // A directory has nothing to render, and a preview URL for one would 400.
  // Drive already filters these out of the navigation list; this is the
  // backstop for any other caller.
  if (!item || item.is_dir) return null

  return (
    createPortal(
      <div className="preview-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-label={item.name}>
        <div className="preview-shell" onClick={(e) => e.stopPropagation()}>

          <header className="preview-header">
            <div className="preview-title">
              <span className="preview-title-name" title={item.name}>{item.name}</span>
              <span className="preview-title-meta">
                {meta.kindLabel}{meta.ext && meta.ext !== meta.kindLabel ? ` · ${meta.ext}` : ''} · {meta.size}
                {/* Only worth showing when the arrows are actually there. */}
                {seriesLabel && <span className="preview-title-series"> · {seriesLabel}</span>}
              </span>
            </div>

            <div className="preview-actions">
              {/* Navigation lives in the header, not floating over the body.
                  A video fills the whole area edge to edge, so overlaid arrows
                  sat on top of the picture itself. Here they cannot overlap
                  anything, for any format. */}
              {series && (
                <div className="preview-pager">
                  <button
                    className="preview-nav"
                    onClick={prev}
                    title={series.at > 1 ? `Previous ${series.singular}` : 'First in this series'}
                    aria-label={`Previous ${series.singular}`}
                    disabled={series.at <= 1}
                  >
                    <ChevronLeft size={17} />
                  </button>
                  <button
                    className="preview-nav"
                    onClick={next}
                    title={series.at < series.total ? `Next ${series.singular}` : 'Last in this series'}
                    aria-label={`Next ${series.singular}`}
                    disabled={series.at >= series.total}
                  >
                    <ChevronRight size={17} />
                  </button>
                </div>
              )}

              <button className="preview-action" onClick={() => onDownload(item)} title="Download">
                <Download size={16} />
                <span>Download</span>
              </button>
              <button className="preview-action preview-action-close" onClick={onClose} title="Close" aria-label="Close preview">
                <X size={18} />
              </button>
            </div>
          </header>

          <div className="preview-body">
            {tokenError ? (
              <div className="preview-fallback">
                <FileWarning size={28} />
                <p>{tokenError}</p>
              </div>
            ) : (
              <PreviewBody
                item={item}
                kind={kind}
                streamUrl={streamUrl}
                blobUrl={blobUrl}
                viewable={canPreview(item)}
                streamPending={!streamUrl}
                onPage={onNavigate}
                canPage={!!series}
              />
            )}
          </div>
        </div>
      </div>,
      document.body
    )
  )
}

function PreviewBody({ item, kind, streamUrl, blobUrl, viewable, streamPending, onPage, canPage }) {
  if (!viewable) return <UnsupportedPreview item={item} />

  // Only the media elements need the tokenised URL, so only they wait for it.
  // The parsed viewers fetch with a header and can start right away.
  switch (kind) {
    case KIND.IMAGE:
      if (streamPending) return <div className="preview-loading">Loading…</div>
      return <ImageViewer src={streamUrl} alt={item.name} canPage={canPage} onPage={onPage} />

    case KIND.VIDEO:
      if (streamPending) return <div className="preview-loading">Loading…</div>
      return <VideoPlayer src={streamUrl} title={item.name} canPage={canPage} onPage={onPage} />;

    case KIND.AUDIO:
      if (streamPending) return <div className="preview-loading">Loading…</div>
      return (
        <div className="preview-audio">
          <div className="preview-audio-disc" aria-hidden="true" />
          <p className="preview-audio-name">{item.name}</p>
          <audio className="preview-audio-el" src={streamUrl} controls preload="metadata" />
        </div>
      )

    case KIND.PDF:
      if (streamPending) return <div className="preview-loading">Loading…</div>
      // The browser's own PDF viewer, fed from the Range-capable endpoint.
      return <iframe className="preview-pdf" src={streamUrl} title={item.name} />;

    case KIND.TEXT:
      return <TextViewer url={blobUrl} />;

    case KIND.SHEET:
      return <SheetViewer url={blobUrl} />;

    case KIND.DOC:
      return <DocViewer url={blobUrl} />;

    default:
      return <UnsupportedPreview item={item} />
  }
}