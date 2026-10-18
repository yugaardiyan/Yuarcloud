import React, { useEffect } from 'react'
import { createPortal } from 'react-dom'
import { AlertTriangle } from 'lucide-react'
import { claimKeyboard } from '../utils/keyboardOwner'

/**
 * Small, modern confirmation dialog. Replaces window.confirm(), which
 * cannot be styled and reads as a browser chrome block.
 *
 * `danger` tints the icon red for destructive actions (delete, empty
 * trash); everything else gets the neutral icon.
 *
 * `simple` renders the Apple-style compact form: a single text line
 * plus Cancel / Ok actions, no icon, no separate title. The single
 * line is taken from `title` (preferred) or `message` as a fallback.
 */
export default function ConfirmModal({
  title,
  message,
  confirmLabel = 'Ok',
  cancelLabel = 'Cancel',
  danger = false,
  simple = false,
  onConfirm,
  onCancel,
}) {
  // Register as an open surface so document-level shortcuts stand down.
  // See utils/keyboardOwner — a shortcut asking "is anything open?" cannot be
  // defeated by a new overlay class it does not know about.
  useEffect(() => {
    const release = claimKeyboard('confirm-modal')
    return release
  }, [])

  const simpleText = title || message || ''
  return createPortal(
    <div
      className="modal-overlay"
      onClick={onCancel}
      role="alertdialog"
      aria-modal="true"
      aria-labelledby={simple ? 'confirm-message' : 'confirm-title'}
      aria-describedby={simple ? undefined : 'confirm-message'}
    >
      <div
        className={`modal confirm-modal ${danger ? 'danger' : ''} ${simple ? 'simple' : ''}`}
        onClick={(e) => e.stopPropagation()}
      >
        {!simple && (
          <div className="confirm-icon">
            <AlertTriangle size={15} />
          </div>
        )}
        {!simple && (
          <h2 className="confirm-title" id="confirm-title">{title}</h2>
        )}
        <p
          className="confirm-message"
          id="confirm-message"
          style={simple ? { marginBottom: 0 } : undefined}
        >
          {simple ? simpleText : message}
        </p>
        <div className="confirm-actions">
          <button className="btn btn-secondary" onClick={onCancel}>
            {cancelLabel}
          </button>
          <button className={`btn ${danger ? 'btn-danger' : 'btn-primary'}`} onClick={onConfirm}>
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>,
    document.body
  )
}
