import React, { useState, useEffect, useRef } from 'react'
import { createPortal } from 'react-dom'
import { X, FolderPlus } from 'lucide-react'
import { claimKeyboard } from '../utils/keyboardOwner'

/**
 * Small text-prompt dialog used for "New folder" and "Rename".
 *
 * Replaces window.prompt(), which is suppressed in some embedded browsers and
 * cannot be styled to match the rest of the app.
 */
export default function PromptModal({
  title,
  label = 'Name',
  initialValue = '',
  confirmLabel = 'Create',
  placeholder = '',
  selectEnd,
  onConfirm,
  onCancel,
}) {
  const [value, setValue] = useState(initialValue)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const inputRef = useRef(null)

  // Register as an open surface so document-level shortcuts stand down.
  // See utils/keyboardOwner — a shortcut asking "is anything open?" cannot be
  // defeated by a new overlay class it does not know about.
  useEffect(() => {
    const release = claimKeyboard('prompt-modal')
    return release
  }, [])

  useEffect(() => {
    // Focus and pre-select so typing replaces the current name.
    //
    // `selectEnd` shortens the selection for a rename — see `stemLength`, which
    // stops it before a file's extension so ".pdf" survives the keystroke. With
    // no `selectEnd` this selects the whole field, which is what the
    // create-folder dialog wants on its empty value.
    const t = setTimeout(() => {
      const el = inputRef.current
      if (!el) return
      el.focus()
      const end = selectEnd ?? el.value.length
      el.setSelectionRange(0, Math.min(end, el.value.length))
    }, 30)
    return () => clearTimeout(t)
  }, [selectEnd])

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === 'Escape') onCancel()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onCancel])

  const submit = async (e) => {
    e?.preventDefault()
    const trimmed = value.trim()

    if (!trimmed) {
      setError('Please enter a name')
      inputRef.current?.focus()
      return
    }
    // Reject anything that would break the path structure.
    if (/[/\\]/.test(trimmed)) {
      setError('A name cannot contain / or \\')
      return
    }
    if (trimmed === '.' || trimmed === '..') {
      setError('That name is reserved')
      return
    }

    setBusy(true)
    try {
      await onConfirm(trimmed)
    } catch (err) {
      // The caller already surfaces a toast; keep the dialog open.
      setBusy(false)
      return
    }
    onCancel()
  }

  return createPortal(
    <div className="modal-overlay" onClick={onCancel} role="dialog" aria-modal="true" aria-labelledby="prompt-title">
      <div className="modal sheet compact" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <div className="sheet-head">
            <h2 className="modal-title" id="prompt-title">{title}</h2>
            {/* The error sits beside the title instead of under the field: it
                keeps the dialog the same height whether or not the name was
                rejected, and the message lands where the eye already is. */}
            <span
              className="sheet-head-notice"
              role="alert"
              aria-live="polite"
              title={error || undefined}
            >
              {error}
            </span>
          </div>
          <button className="modal-close" onClick={onCancel} aria-label="Close">
            <X size={15} />
          </button>
        </div>

        <form onSubmit={submit}>
          <div className="modal-body">
            {/* No visible label: the title already says what the field is for,
                and a second "New name" line above the input only pushed the
                rest of the dialog down. The label still names the field for
                screen readers. */}
            <input
              ref={inputRef}
              id="prompt-input"
              className={`login-input login-input-plain ${error ? 'input-error' : ''}`}
              aria-label={label}
              placeholder={placeholder}
              value={value}
              onChange={(e) => { setValue(e.target.value); setError('') }}
              autoComplete="off"
              spellCheck={false}
            />
          </div>

          <div className="modal-footer">
            <button type="button" className="btn btn-secondary" onClick={onCancel} disabled={busy}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={busy}>
              <FolderPlus size={16} />
              {busy ? 'Working…' : confirmLabel}
            </button>
          </div>
        </form>
      </div>
    </div>,
    document.body
  )
}
