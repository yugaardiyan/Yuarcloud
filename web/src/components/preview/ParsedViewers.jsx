import { useState, useEffect, useMemo } from 'react'
import { FileText, FileSpreadsheet, FileType, Archive } from 'lucide-react'
import { KIND, kindOf, parseLimitFor } from '../../utils/fileType'

import { sanitizeDocHtml } from './sanitizeDocHtml'
/**
 * Previewers for the formats the browser cannot play but can be handed the
 * bytes for: plain text, spreadsheets and Word documents.
 *
 * All three fetch the whole file with the Authorization header rather than
 * pointing a tag at the streaming URL, because each needs the complete body
 * before it can produce anything. That is why they all carry a size ceiling —
 * past it the answer is a download, not a frozen tab.
 */

function useParsedFile(url, limit) {
  const [state, setState] = useState({ status: 'loading', data: null, error: null, tooBig: false })

  useEffect(() => {
    let cancelled = false
    const controller = new AbortController()

    setState({ status: 'loading', data: null, error: null, tooBig: false })

    ;(async () => {
      try {
        const token = localStorage.getItem('access_token')
        const res = await fetch(url, {
          signal: controller.signal,
          headers: token ? { Authorization: `Bearer ${token}` } : {},
        })

        if (!res.ok) {
          // 413 is the server refusing an oversized inline read; anything
          // else is a real error worth surfacing.
          if (res.status === 413) {
            if (!cancelled) setState({ status: 'done', data: null, error: null, tooBig: true })
            return
          }
          throw new Error(`HTTP ${res.status}`)
        }

        const declared = Number(res.headers.get('Content-Length') || 0)
        if (declared && declared > limit) {
          if (!cancelled) setState({ status: 'done', data: null, error: null, tooBig: true })
          return
        }

        const buffer = await res.arrayBuffer()
        if (cancelled) return
        if (buffer.byteLength > limit) {
          setState({ status: 'done', data: null, error: null, tooBig: true })
          return
        }
        setState({ status: 'done', data: buffer, error: null, tooBig: false })
      } catch (err) {
        if (cancelled || err.name === 'AbortError') return
        setState({ status: 'done', data: null, error: err.message, tooBig: false })
      }
    })()

    return () => {
      cancelled = true
      controller.abort()
    }
  }, [url, limit])

  return state
}

/* ------------------------------------------------------------------ text -- */

const BINARY_PROBE = 8000

export function TextViewer({ url }) {
  const { status, data, error, tooBig } = useParsedFile(url, parseLimitFor(KIND.TEXT))
  const [wrap, setWrap] = useState(true)

  if (tooBig) return <TooBig />
  if (status === 'loading') return <div className="preview-loading">Loading…</div>
  if (error) return <div className="preview-fallback">Could not read this file ({error}).</div>

  // Decode leniently: a stray byte in a .log or .csv should not blank the view.
  let text
  try {
    text = new TextDecoder('utf-8', { fatal: false }).decode(data)
  } catch {
    return <div className="preview-fallback">This file is not valid UTF-8 text.</div>
  }

  // If the first slice is full of control bytes it is probably binary that
  // slipped past the extension check. Showing replacement characters would be
  // worse than saying so.
  const probe = text.slice(0, BINARY_PROBE)
  let control = 0
  for (const ch of probe) {
    const code = ch.codePointAt(0)
    if (code === 9 || code === 10 || code === 13) continue
    if (code < 32 || code === 127) control++
  }
  if (probe.length > 0 && control / probe.length > 0.1) {
    return <div className="preview-fallback">This file looks binary rather than text.</div>
  }

  const lines = text.split('\n').length
  return (
    <div className="preview-text">
      <div className="preview-text-bar">
        <span className="preview-text-meta">{lines.toLocaleString()} lines</span>
        <label className="preview-toggle">
          <input type="checkbox" checked={wrap} onChange={(e) => setWrap(e.target.checked)} />
          Wrap lines
        </label>
      </div>
      <pre className={wrap ? 'is-wrapped' : ''}>{text}</pre>
    </div>
  )
}

/* ----------------------------------------------------------------- sheet -- */

export function SheetViewer({ url }) {
  const { status, data, error, tooBig } = useParsedFile(url, parseLimitFor(KIND.SHEET))
  const [active, setActive] = useState(0)
  // xlsx is ~1 MB and mammoth ~1 MB; both load on demand so the initial
  // bundle stays the same size for users who only ever open images.
  const [XLSX, setXLSX] = useState(null)

  useEffect(() => {
    if (status !== 'done' || !data || data.byteLength === 0) return
    if (XLSX) return
    import('xlsx').then((mod) => setXLSX(mod.default || mod)).catch(() => {})
  }, [status, data, XLSX])

  // Parsed once per file, not once per render.
  //
  // XLSX.read used to sit in the render body, so every render re-read the whole
  // workbook: clicking a sheet tab re-parsed the file to show a different sheet
  // of something already in memory, and any unrelated re-render — a resize,
  // the parent re-rendering — did it again on the main thread. A workbook with
  // a few thousand rows made every tab click a visible stall.
  //
  // Both memos sit above the early returns on purpose. A hook below one runs
  // conditionally, the hook count changes between renders, and React tears the
  // component down; so these compute a value instead of short-circuiting.
  const parsed = useMemo(() => {
    if (!XLSX || !data) return { workbook: null, parseError: null }
    try {
      return { workbook: XLSX.read(data, { type: 'array' }), parseError: null }
    } catch (err) {
      return { workbook: null, parseError: err.message || String(err) }
    }
  }, [XLSX, data])

  const { workbook, parseError } = parsed

  const sheetName =
    workbook && !parseError && workbook.SheetNames.length
      ? workbook.SheetNames[Math.min(active, workbook.SheetNames.length - 1)]
      : null

  // Same reason: converting a sheet to rows is not cheap on a large one, and
  // it depends on nothing but which sheet is showing.
  const rows = useMemo(() => {
    if (!XLSX || !workbook || !sheetName) return []
    // Header row: 1 so column letters do not appear above the first data row.
    return XLSX.utils.sheet_to_json(workbook.Sheets[sheetName], { header: 1, blankrows: false, defval: '' })
  }, [XLSX, workbook, sheetName])

  if (tooBig) return <TooBig />
  if (status === 'loading') return <div className="preview-loading">Loading…</div>
  if (error) return <div className="preview-fallback">Could not read this workbook ({error}).</div>

  if (parseError) return <div className="preview-fallback">This workbook could not be parsed ({parseError}).</div>
  if (!XLSX) return <div className="preview-loading">Preparing the spreadsheet reader…</div>
  if (!workbook || workbook.SheetNames.length === 0) {
    return <div className="preview-fallback">This workbook has no sheets.</div>
  }

  const MAX_ROWS = 500
  const MAX_COLS = 80
  const shown = rows.slice(0, MAX_ROWS)
  const width = Math.min(shown.reduce((max, r) => Math.max(max, r.length), 0), MAX_COLS)

  return (
    <div className="preview-sheet">
      {workbook.SheetNames.length > 1 && (
        <div className="preview-sheet-tabs">
          {workbook.SheetNames.map((name, i) => (
            <button
              key={name}
              className={`preview-sheet-tab ${i === active ? 'is-active' : ''}`}
              onClick={() => setActive(i)}
            >
              {name}
            </button>
          ))}
        </div>
      )}

      <div className="preview-sheet-scroll">
        <table>
          <tbody>
            {shown.map((row, ri) => (
              <tr key={ri} className={ri === 0 ? 'is-header' : ''}>
                {Array.from({ length: width }).map((_, ci) => {
                  const cell = row[ci]
                  const isFormula = cell && typeof cell === 'object' && 'f' in cell
                  const value = isFormula ? cell.w ?? '' : cell ?? ''
                  return (
                    <td key={ci} title={String(value) || undefined}>
                      {String(value)}
                    </td>
                  )
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="preview-sheet-foot">
        Showing {Math.min(rows.length, MAX_ROWS).toLocaleString()} of {rows.length.toLocaleString()} rows
        {width === MAX_COLS && `, first ${MAX_COLS} columns`}
        {rows.length > MAX_ROWS && ' — download for the full sheet'}
      </div>
    </div>
  )
}

/* ------------------------------------------------------------------ docx -- */

export function DocViewer({ url }) {
  const { status, data, error, tooBig } = useParsedFile(url, parseLimitFor(KIND.DOC))
  const [html, setHtml] = useState(null)
  const [messages, setMessages] = useState([])
  const [parseError, setParseError] = useState(null)

  useEffect(() => {
    if (status !== 'done' || !data) return
    let cancelled = false

    import('mammoth/mammoth.browser')
      .then((mammoth) =>
        mammoth.convertToHtml({ arrayBuffer: data }, {
          // Word carries far more than "text": styling, headers, comments,
          // tracked changes. Asking for the semantic subset keeps the output
          // readable instead of emitting a wall of empty inline styles.
          styleMap: [
            "p[style-name='Title'] => h1:fresh",
            "p[style-name='Heading 1'] => h2:fresh",
            "p[style-name='Heading 2'] => h3:fresh",
            "p[style-name='Heading 3'] => h4:fresh",
            "p[style-name='Quote'] => blockquote:fresh",
          ],
        })
      )
      .then((result) => {
        if (cancelled) return
        setHtml(result.value)
        setMessages(result.messages || [])
      })
      .catch((err) => {
        if (!cancelled) setParseError(err.message)
      })

    return () => { cancelled = true }
  }, [status, data])

  if (tooBig) return <TooBig />
  if (status === 'loading') return <div className="preview-loading">Loading…</div>
  if (error) return <div className="preview-fallback">Could not read this document ({error}).</div>
  if (parseError) return <div className="preview-fallback">This document could not be parsed ({parseError}).</div>
  if (html === null) return <div className="preview-loading">Converting…</div>

  return (
    <div className="preview-doc">
      {/* Sanitised below, not by mammoth. Mammoth emits the hyperlink target
          from the document's relationship table verbatim and does no URL
          filtering at all, so a .docx can carry href="javascript:…" straight
          into the DOM. This is reachable from the *public* share page, where
          anyone holding a write link can plant the file and a visitor opens it
          — the injected script then runs on this origin and can read the
          session token out of localStorage. */}
      <div className="preview-doc-body" dangerouslySetInnerHTML={{ __html: sanitizeDocHtml(html) }} />
      {messages.length > 0 && (
        <div className="preview-doc-notes">
          {messages.length} formatting feature{messages.length === 1 ? '' : 's'} could not be
          represented in the browser. Download the file to see the original layout.
        </div>
      )}
    </div>
  )
}

/* --------------------------------------------------------------- fallback -- */

export function TooBig() {
  return (
    <div className="preview-fallback">
      <Archive size={28} />
      <p>This file is too large to render here. Download it to open it in a full app.</p>
    </div>
  )
}

/** Shown for anything the browser has no renderer for. */
export function UnsupportedPreview({ item }) {
  const icon =
    kindOf(item) === KIND.SHEET ? <FileSpreadsheet size={30} />
      : kindOf(item) === KIND.DOC ? <FileType size={30} />
        : kindOf(item) === KIND.TEXT ? <FileText size={30} />
          : <Archive size={30} />

  return (
    <div className="preview-fallback">
      {icon}
      <p>{unsupportedMessage(item)}</p>
    </div>
  )
}

function unsupportedMessage(item) {
  const ext = (item?.name || '').split('.').pop()?.toUpperCase()
  if (ext === 'XLS' || ext === 'DOC' || ext === 'PPT') {
    return `${ext} is the pre-2007 Office format and cannot be shown in a browser. Download it and open it in an office app — modern .${ext.toLowerCase()}x files open fine here.`
  }
  if (ext === 'ZIP' || ext === 'RAR' || ext === '7Z') {
    return 'Archives cannot be shown inline. Download the file to extract it.'
  }
  return 'No preview is available for this file type. Download it to open it.'
}