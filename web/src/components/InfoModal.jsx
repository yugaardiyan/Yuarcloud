import React, { useEffect } from 'react'
import { createPortal } from 'react-dom'
import { X } from 'lucide-react'
import { FileIcon, iconClassFor } from './FileIcon'
import { formatBytes, formatDate } from '../utils/helpers'
import { claimKeyboard } from '../utils/keyboardOwner'

/**
 * File metadata sheet.
 *
 * Replaces the window.alert() that used to answer "Get info": an unstyleable
 * block of padded text with no icon, no columns and no way to copy a single
 * value out of it.
 *
 * Kept deliberately thin. The name is the title, so repeating it as a row is
 * and the header's ✕ already closes it, so a full-width Close button below is
 * noise. What is left is the part that only exists here.
 */
export default function InfoModal({ file, onClose }) {

  // Register as an open surface so document-level shortcuts stand down.
  // See utils/keyboardOwner — a shortcut asking "is anything open?" cannot be
  // defeated by a new overlay class it does not know about.
  useEffect(() => {
    const release = claimKeyboard('info-modal')
    return release
  }, [])

  const rows = [
    ['Type', file.is_dir ? 'Folder' : file.mime_type || 'unknown'],
    ['Size', file.is_dir ? '—' : formatBytes(file.size)],
    ['Location', '/' + (file.path || '')],
  ]
  if (file.updated_at) rows.push(['Modified', formatDate(file.updated_at)])
  if (file.created_at) rows.push(['Created', formatDate(file.created_at)])
  if (file.original_path) rows.push(['Moved from', '/' + file.original_path])
  // Last, and only when there is one: 64 hex characters is a lot of modal to
  // spend on a value nobody reads, and it is on the row that scrolls.
  if (file.checksum) rows.push(['SHA-256', file.checksum])

  return createPortal(
    <div className="modal-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-labelledby="info-title">
      <div className="modal info-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header info-head">
          <span className={`file-icon ${iconClassFor(file)} info-glyph`}>
            <FileIcon item={file} />
          </span>
          <h2 className="modal-title info-title" id="info-title" title={file.name}>
            {file.name}
          </h2>
          <button type="button" className="modal-close" onClick={onClose} aria-label="Close">
            <X size={15} />
          </button>
        </div>

        <dl className="info-rows">
          {rows.map(([k, v]) => (
            <div className="info-row" key={k}>
              <dt>{k}</dt>
              <dd title={String(v)}>{String(v ?? '—')}</dd>
            </div>
          ))}
        </dl>
      </div>
    </div>,
    document.body
  )
}