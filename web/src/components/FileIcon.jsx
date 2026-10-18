import React from 'react'

/**
 * File and folder glyphs.
 *
 * Two drawing styles live side by side here. The folder is a filled
 * silhouette; most files are stroked outlines. Where that mix is visible,
 * it is because each shape was chosen for how it reads rather than to follow
 * one rule — so the paths are grouped by style below instead of pretending
 * everything matches.
 *
 * Colour is always currentColor, never a literal #000000. The coloured tile
 * that used to sit behind each glyph is gone, so the colour comes from the
 * container instead; hard-coding black here would throw that away.
 */

const svgProps = {
  viewBox: '0 0 24 24',
  fill: 'currentColor',
  'aria-hidden': true,
}

/** Outlined glyphs: no fill, a 1.5 stroke in the inherited colour. */
const strokeProps = {
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
  'aria-hidden': true,
}

/** Same, but miter joins — the music glyph's beam is drawn that way. */
const strokePropsMiter = {
  ...strokeProps,
  strokeLinejoin: undefined,
}

/* The document body: folded corner plus the sheet outline. Shared by both
   document glyphs below, with or without the text lines on top. */
const DOC_BODY =
  'M20.25 8.568v8.568a4.251 4.251 0 0 1-1.362 2.97 4.283 4.283 0 0 1-3.072 1.14h-7.59a4.294 4.294 0 0 1-3.1-1.124 4.265 4.265 0 0 1-1.376-2.986V6.862a4.25 4.25 0 0 1 1.362-2.97 4.283 4.283 0 0 1 3.072-1.14h5.714a3.503 3.503 0 0 1 2.361.905l2.96 2.722a2.971 2.971 0 0 1 1.031 2.189'

/* Three rules across the page, appended to the body for documents that hold
   readable text. */
const DOC_TEXT_LINES = 'M7.647 7.647h3.265M7.647 12h8.706m-8.706 4.353h8.706'

/**
 * A sheet of paper with writing on it: PDF, spreadsheets, Word, plain text.
 *
 * `lines` is what separates this from the blank sheet below — the two SVGs
 * differ by exactly those three rules and nothing else.
 */
function DocIcon() {
  return (
    <svg {...strokeProps}>
      <path d="M14.186 2.753v3.596c0 .487.194.955.54 1.3a1.85 1.85 0 0 0 1.306.539h4.125" />
      <path d={`${DOC_BODY}${DOC_TEXT_LINES}`} />
    </svg>
  )
}

/**
 * A blank sheet — the fallback for files whose type we do not recognise
 * (firmware.bin and friends). No text lines, so it reads as "a file" rather
 * than "a document someone wrote".
 */
function BlankDocIcon() {
  return (
    <svg {...strokeProps}>
      <path d="M14.186 2.753v3.596c0 .487.194.955.54 1.3a1.85 1.85 0 0 0 1.306.539h4.125" />
      <path d={DOC_BODY} />
    </svg>
  )
}

/**
 * Archives: a storage box with a lid band and a recessed handle.
 *
 * Drawn to match the supplied artwork rather than borrowed from an icon set —
 * same 24 grid, the same 1.5 stroke with round caps and joins, and a 3-unit
 * corner radius, which is what the sheet, camera and frame glyphs all use.
 * Kept as an outline rather than a filled silhouette because the lid and the
 * handle are the parts that make it read as "archive" at all, and filling
 * them turns the box into a solid rectangle.
 */
function ArchiveIcon() {
  return (
    <svg {...strokeProps}>
      {/* 17 x 18 inside a 24 grid — narrower and taller than the first draft,
          which came out square while every sheet in the family is portrait. */}
      <path d="M6.5 3.5h11a3 3 0 0 1 3 3v12a3 3 0 0 1-3 3h-11a3 3 0 0 1-3-3v-12a3 3 0 0 1 3-3" />
      <path d="M4 9.25h16" />
      <path d="M9 13.75h6" />
    </svg>
  )
}

/** Plain text: a sheet with a pen laid across it. */
function TxtIcon() {
  return (
    <svg {...strokeProps}>
      <path d="M21.25 6.861v6.342a2.057 2.057 0 0 1-.606 1.459l-5.982 5.982a2.055 2.055 0 0 1-1.46.606h-6.34a4.111 4.111 0 0 1-4.112-4.111V6.86a4.111 4.111 0 0 1 4.111-4.11H17.14a4.111 4.111 0 0 1 4.111 4.111" />
      <path d="m14.056 21.075-.514-4.11a3.082 3.082 0 0 1 3.443-3.444l4.11.514M6.861 7.889h9.25M6.861 12h4.111m-4.111 4.111h3.083" />
    </svg>
  )
}

/**
 * Spreadsheet: the document sheet with a cross struck through it.
 *
 * The sheet outline is the same one DocIcon draws, so a .xlsx and a .docx
 * still read as "a page"; only the mark in the middle tells them apart.
 */
function SheetIcon() {
  return (
    <svg {...strokeProps}>
      <path d="M14.186 2.753v3.596c0 .487.194.955.54 1.3a1.85 1.85 0 0 0 1.306.539h4.125" />
      <path d={DOC_BODY} />
      <path strokeMiterlimit="10" d="m14.51 11.513-5.03 5.032m-.001-5.021 5.032 5.032" />
    </svg>
  )
}

/**
 * PDF: a filled page whose text lines are cut out of it.
 *
 * Those line sub-paths run opposite to the page outline, so nonzero winding
 * punches them through as holes rather than painting them — reversing their
 * direction would turn the whole thing into a red blob. Painted in the
 * inherited colour, not the literal #000000 the source SVG carried.
 */
function PdfIcon() {
  return (
    <svg {...svgProps}>
      <path d="M20.57 3.42A4.82 4.82 0 0 0 17.13 2H6.89a4.87 4.87 0 0 0-4.86 4.86v10.27A4.88 4.88 0 0 0 6.89 22h6.35c.37-.003.737-.078 1.08-.22l.16-.08.08-.05a2.44 2.44 0 0 0 .67-.48l6-6c.203-.2.372-.434.5-.69l.08-.17c.004-.04.063-.12.059-.16.107-.296.102-.566.1-.88V6.93a4.82 4.82 0 0 0-1.4-3.51M9.94 16.86H6.85a.75.75 0 1 1 0-1.5h3.09a.75.75 0 0 1 0 1.5m1-4.11H6.82a.75.75 0 0 1 0-1.5h4.12a.75.75 0 0 1 0 1.5M6.82 8.63a.75.75 0 1 1 0-1.5h9.25a.75.75 0 0 1 0 1.5zm7.77 11-.34-2.75a2.265 2.265 0 0 1 .11-1 2.22 2.22 0 0 1 .55-.89 2.28 2.28 0 0 1 .9-.56 2.32 2.32 0 0 1 1.05-.11l2.75.34z" />
    </svg>
  )
}

/** CSS class that colours the glyph for a given item type. */
export function iconClassFor(item) {
  if (item.is_dir) return 'folder'
  const mime = item.mime_type || ''
  const name = (item.name || '').toLowerCase()

  if (mime.startsWith('image/')) return 'image'
  if (mime.startsWith('video/')) return 'video'
  if (mime.startsWith('audio/')) return 'audio'
  if (mime.includes('pdf')) return 'pdf'
  if (mime.includes('spreadsheet') || mime.includes('excel') || /\.(xls|xlsx|csv|ods|tsv)$/.test(name)) return 'sheet'
  if (mime.includes('zip') || mime.includes('archive') || /\.(zip|rar|7z|tar|gz|bz2)$/.test(name)) return 'archive'
  // Word files get their own class rather than sharing `text`: the glyph is
  // the same sheet, but Word is blue and plain text is not, and collapsing
  // the two into one colour made .docx and .txt indistinguishable.
  if (mime.includes('word') || /\.(doc|docx|odt)$/.test(name)) return 'doc'
  if (mime.startsWith('text/') || /\.(txt|md|rtf|log)$/.test(name)) return 'text'
  if (/\.(js|ts|jsx|tsx|html|css|json|py|java|cpp|c|go|rs|php|rb|sh|sql|yaml|yml|toml)$/.test(name)) return 'code'
  return 'file'
}

/** Returns the icon element for an item (folder glyph or a type-specific SVG). */
export function FileIcon({ item }) {
  if (item.is_dir) {
    return (
      <svg {...svgProps}>
        <path d="M20.99 7.325v-2.15a2.29 2.29 0 0 0-1.66-2.21 2.31 2.31 0 0 0-1-.05l-6.51 1a3.67 3.67 0 0 0-1.09-.87 3.8 3.8 0 0 0-1.81-.46H5.84A3.82 3.82 0 0 0 2 6.425v11.16A3.82 3.82 0 0 0 5.84 21.415h12.33a3.82 3.82 0 0 0 2.71-1.12 3.88 3.88 0 0 0 1.12-2.71v-7.7a3.73 3.73 0 0 0-1.01-2.56m-4.34 9.76H7.4a1 1 0 0 1 0-2h9.25a1 1 0 0 1 0 2m2.88-10.71a3.78 3.78 0 0 0-1.3-.23h-4.4a1 1 0 0 1-.47-.12 1 1 0 0 1-.35-.32l-.23-.34 5.8-.93a.77.77 0 0 1 .34 0 .73.73 0 0 1 .3.16.6.6 0 0 1 .2.27.71.71 0 0 1 .07.33z" />
      </svg>
    )
  }

  const name = (item.name || '').toLowerCase()
  const mime = item.mime_type || ''

  if (mime.startsWith('image/')) {
    return (
      <svg {...strokeProps}>
        {/* A rounded frame with a ridge line and a sun. Outlined while the
            folder is filled; the shape was picked for legibility, not to
            match, so the two weights sit side by side in a mixed folder. */}
        <path d="M16.24 3.5h-8.5a5 5 0 0 0-5 5v7a5 5 0 0 0 5 5h8.5a5 5 0 0 0 5-5v-7a5 5 0 0 0-5-5" />
        <path d="m2.99 17 2.75-3.2a2.2 2.2 0 0 1 2.77-.27 2.2 2.2 0 0 0 2.77-.27l2.33-2.33a4 4 0 0 1 5.16-.43l2.49 1.93" />
        <path d="M7.99 10.17a1.66 1.66 0 1 0 0-3.32 1.66 1.66 0 0 0 0 3.32" />
      </svg>
    )
  }

  if (mime.startsWith('video/')) {
    return (
      <svg {...strokeProps}>
        {/* A camera body with a lens barrel folded out to the right. */}
        <path d="M12 5.32H6.095A3.595 3.595 0 0 0 2.5 8.923v6.162a3.595 3.595 0 0 0 3.595 3.595H12a3.595 3.595 0 0 0 3.595-3.595V8.924A3.594 3.594 0 0 0 12 5.319m9.5 4.119v5.135c0 .25-.071.496-.205.708a1.355 1.355 0 0 1-.555.493 1.27 1.27 0 0 1-.73.124 1.367 1.367 0 0 1-.677-.278l-3.225-2.588a1.376 1.376 0 0 1-.503-1.047c0-.2.045-.397.133-.575.092-.168.218-.315.37-.432l3.225-2.567a1.36 1.36 0 0 1 .678-.278c.25-.032.504.011.729.123a1.336 1.336 0 0 1 .76 1.182" />
      </svg>
    )
  }

  if (mime.startsWith('audio/')) {
    return (
      <svg {...strokePropsMiter}>
        {/* Two note heads on a beamed pair of stems. */}
        <path d="M17.985 19.703a3.265 3.265 0 1 0 0-6.53 3.265 3.265 0 0 0 0 6.53ZM6.015 20.79a3.265 3.265 0 1 0 0-6.528 3.265 3.265 0 0 0 0 6.529Zm15.235-4.352v-8.01M9.28 17.526v-6.921" />
        <path strokeLinejoin="round" d="M21.25 8.429 9.28 10.605v-3.57a2.176 2.176 0 0 1 1.784-2.176l8.902-1.632a1.088 1.088 0 0 1 1.284 1.088z" />
      </svg>
    )
  }

  // PDF gets its own filled sheet; plain text the pen; spreadsheets the cross.
  // Word keeps the ruled sheet from DocIcon.
  if (mime.includes('pdf') || name.endsWith('.pdf')) return <PdfIcon />
  if (/\.(xls|xlsx|csv|ods|tsv)$/.test(name)) return <SheetIcon />
  if (mime.startsWith('text/') || /\.(txt|md|rtf|log)$/.test(name)) return <TxtIcon />
  if (/\.(doc|docx|odt)$/.test(name) || mime.includes('word')) return <DocIcon />

  if (/\.(zip|rar|7z|tar|gz|bz2)$/.test(name)) return <ArchiveIcon />
  if (/\.(js|ts|jsx|tsx|html|css|json|py|java|cpp|c|h|go|rs|php|rb|sh|sql|yaml|yml|toml)$/.test(name)) {
    return (
      <svg {...svgProps}>
        <path d="M9.4 16.6 4.8 12l4.6-4.6L8 6l-6 6 6 6zm5.2 0 4.6-4.6-4.6-4.6L16 6l6 6-6 6z" />
      </svg>
    )
  }

  return <BlankDocIcon />
}

export default FileIcon