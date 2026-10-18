/**
 * What kind of thing is this file, and can the browser show it?
 *
 * The MIME type the server stored is the primary signal, but it cannot be
 * trusted alone: uploads carry whatever the client claimed, a lot of files
 * arrive as application/octet-stream, and Go's table knows nothing about
 * .docx or .tsx. The extension is the fallback, and where the two disagree
 * about *renderability* — an .xls claiming to be text/plain — the extension
 * wins, because guessing wrong means handing the browser a file it cannot
 * parse and showing the user an empty frame.
 */

export const KIND = {
  IMAGE: 'image',
  VIDEO: 'video',
  AUDIO: 'audio',
  PDF: 'pdf',
  TEXT: 'text',
  SHEET: 'sheet',
  DOC: 'doc',
  ARCHIVE: 'archive',
  BINARY: 'binary',
}

const CODE_EXT = new Set([
  'js', 'mjs', 'cjs', 'jsx', 'ts', 'tsx', 'json', 'jsonc', 'map',
  'html', 'htm', 'css', 'scss', 'sass', 'less',
  'py', 'rb', 'go', 'rs', 'java', 'kt', 'c', 'h', 'cpp', 'hpp', 'cs', 'php', 'swift',
  'sh', 'bash', 'zsh', 'fish', 'sql', 'lua', 'r', 'pl', 'dart', 'ex', 'exs',
  'yml', 'yaml', 'toml', 'ini', 'cfg', 'conf', 'env', 'gitignore', 'dockerfile',
  'makefile', 'gradle', 'cmake', 'xml', 'svg', 'graphql', 'proto', 'diff', 'patch',
])

const TEXT_EXT = new Set(['txt', 'log', 'text', 'rtf', 'nfo', 'readme', 'license', 'md', 'markdown'])

const ARCHIVE_EXT = new Set([
  'zip', 'rar', '7z', 'tar', 'gz', 'tgz', 'bz2', 'xz', 'zst', 'iso', 'dmg', 'jar', 'war',
])

// Legacy Office binaries. Excel and Word cannot read them, so they must not
// be routed to the xlsx/docx parsers, which would fail in confusing ways.
const LEGACY_OFFICE_EXT = new Set(['xls', 'doc', 'ppt', 'ods', 'odt'])

const IMAGE_EXT = new Set([
  'jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'bmp', 'ico', 'tif', 'tiff', 'heic', 'heif', 'jfif',
])

const VIDEO_EXT = new Set([
  'mp4', 'm4v', 'webm', 'ogv', 'mov', 'avi', 'mkv', 'mpg', 'mpeg', '3gp', 'wmv', 'flv',
])

const AUDIO_EXT = new Set([
  'mp3', 'wav', 'ogg', 'oga', 'opus', 'm4a', 'aac', 'flac', 'wma', 'aiff', 'aif', 'mid', 'midi',
])

export function extOf(name = '') {
  const base = String(name).split('/').pop()
  const dot = base.lastIndexOf('.')
  if (dot <= 0) return '' // a dotfile like ".env" has no extension
  return base.slice(dot + 1).toLowerCase()
}

/**
 * How much of `name` a rename should pre-select: the length of its stem.
 *
 * "Dokumen.pdf" gives 8, so the dialog opens with "Dokumen" highlighted and
 * ".pdf" left alone — typing replaces the name and keeps the format, which is
 * what renaming a file is almost always for. Selecting the whole string meant
 * every rename also had to re-type the extension, and the fastest way to lose
 * one is to retype it.
 *
 * Three cases where the answer is "all of it":
 *   - a folder, which has no extension. A folder called "2024.Photos" cut at
 *     the dot would lose ".Photos" to the first keystroke.
 *   - no dot at all ("README").
 *   - a leading dot, which is a dotfile rather than an extension: ".env" is the
 *     whole name. Same rule `extOf` already applies, hence the `dot <= 0`.
 */
export function stemLength(name = '', isDir = false) {
  const base = String(name).split('/').pop()
  if (isDir) return base.length
  const dot = base.lastIndexOf('.')
  if (dot <= 0) return base.length
  return dot
}

function baseMime(mime = '') {
  return String(mime).split(';')[0].trim().toLowerCase()
}

/**
 * Classify a file record. `item` is anything with `name` and `mime_type`;
 * the shape the API returns works directly.
 */
export function kindOf(item) {
  if (!item) return KIND.BINARY
  if (item.is_dir) return KIND.BINARY

  const ext = extOf(item.name)
  const mime = baseMime(item.mime_type || item.mimeType)

  // Office formats are decided by extension alone — see LEGACY_OFFICE_EXT.
  if (ext === 'csv' || ext === 'tsv' || mime === 'text/csv' ||
      mime === 'application/vnd.ms-excel' ||
      mime === 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' ||
      mime === 'application/vnd.oasis.opendocument.spreadsheet') {
    // .xls/.doc era binaries are not parseable here; they stay downloads.
    if (LEGACY_OFFICE_EXT.has(ext)) return KIND.BINARY
    return KIND.SHEET
  }

  if (ext === 'docx' || mime === 'application/vnd.openxmlformats-officedocument.wordprocessingml.document') {
    if (LEGACY_OFFICE_EXT.has(ext)) return KIND.BINARY
    return KIND.DOC
  }

  if (mime === 'application/pdf' || ext === 'pdf') return KIND.PDF

  if (mime.startsWith('image/') || IMAGE_EXT.has(ext)) return KIND.IMAGE
  if (mime.startsWith('video/') || VIDEO_EXT.has(ext)) return KIND.VIDEO
  if (mime.startsWith('audio/') || AUDIO_EXT.has(ext)) return KIND.AUDIO

  if (ARCHIVE_EXT.has(ext) || mime === 'application/zip' || mime === 'application/x-tar' ||
      mime === 'application/gzip' || mime === 'application/x-7z-compressed') {
    return KIND.ARCHIVE
  }

  if (CODE_EXT.has(ext) || TEXT_EXT.has(ext)) return KIND.TEXT
  // text/* with no useful extension still previews, unless it is one of the
  // XML-ish types that browsers render as a download anyway.
  if (mime.startsWith('text/') && mime !== 'text/rtf') return KIND.TEXT

  return KIND.BINARY
}

/** Kinds the browser can render without help. */
const VIEWABLE = new Set([KIND.IMAGE, KIND.VIDEO, KIND.AUDIO, KIND.PDF, KIND.TEXT, KIND.SHEET, KIND.DOC])

export function canPreview(item) {
  return VIEWABLE.has(kindOf(item))
}

/**
 * Ceilings for the fetch-then-parse kinds. Streaming them is pointless —
 * the parser wants the whole thing — but rendering a 400 MB log or workbook
 * as a DOM node is not, so those fall back to download.
 */
export const PARSE_LIMITS = {
  [KIND.TEXT]: 2 * 1024 * 1024, // 2 MiB
  [KIND.SHEET]: 25 * 1024 * 1024, // 25 MiB
  [KIND.DOC]: 10 * 1024 * 1024, // 10 MiB
}

export function parseLimitFor(kind) {
  return PARSE_LIMITS[kind] ?? 0
}

/**
 * Which "series" a file belongs to when paging with the arrow keys.
 *
 * Stepping through a mixed folder with one pair of arrows means most presses
 * swap an image for a spreadsheet and the viewer reloads for no reason. The
 * arrows now move within a single series.
 *
 * Images and video are deliberately ONE series, not two. They used to be
 * split because the arrows were drawn over the body and covered part of a
 * video frame — the very reason the user kept them apart. Those arrows now
 * live in the header and can overlap nothing, so the split no longer earns
 * its keep; a folder of holiday photos and the film shot on that trip should
 * page through as one continuous reel.
 *
 * Audio is still separate: a track has its own transport and a picture has
 * not, so stepping between them is a jarring change of context. Everything
 * you *read* shares a series too — a PDF next to a report next to a CSV is one
 * continuous reading session.
 *
 * `null` means "do not page at all" — an archive or an unopenable binary has
 * no siblings worth offering.
 */
export const GROUP = {
  MEDIA: 'media',
  AUDIO: 'audio',
  DOCUMENT: 'document',
}

const GROUP_OF_KIND = {
  [KIND.IMAGE]: GROUP.MEDIA,
  [KIND.VIDEO]: GROUP.MEDIA,
  [KIND.AUDIO]: GROUP.AUDIO,
  [KIND.PDF]: GROUP.DOCUMENT,
  [KIND.DOC]: GROUP.DOCUMENT,
  [KIND.SHEET]: GROUP.DOCUMENT,
  [KIND.TEXT]: GROUP.DOCUMENT,
}

export function previewGroupOf(item) {
  if (!item || item.is_dir) return null
  return GROUP_OF_KIND[kindOf(item)] ?? null
}

/** Plural label for the viewer's title bar. */
export const GROUP_LABEL = {
  [GROUP.MEDIA]: 'Media',
  [GROUP.AUDIO]: 'Music',
  [GROUP.DOCUMENT]: 'Documents',
}

/**
 * Singular noun for tooltips, so they read "Next track" rather than
 * "Next tracks". The media series is a mix of both kinds, so its tooltip uses
 * the neutral "file" instead of naming one that may be wrong.
 */
export const GROUP_SINGULAR = {
  [GROUP.MEDIA]: 'file',
  [GROUP.AUDIO]: 'track',
  [GROUP.DOCUMENT]: 'document',
}

export function formatSize(bytes) {
  const n = Number(bytes) || 0
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`
}