/**
 * Whether anything on screen currently owns the keyboard.
 *
 * A document-level shortcut that skips this guard does not just read as a
 * conflict: it acts. Drive's Delete/Backspace handler was written against a
 * two-class selector, and the preview viewer is its own class at a higher
 * z-index, so it was never matched. Selecting two files, opening a preview of
 * one and pressing Delete offered "move 2 items to trash?" on top of the
 * viewer.
 *
 * The list of overlay class names is the thing that rots — every new surface
 * is one more way for a shortcut to fire through it. So each overlay marks the
 * document while it is mounted, and this asks that rather than naming classes.
 */
const OWNERS = new Set()

/** Marks the document as keyboard-owned. Returns an undo function. */
export function claimKeyboard(claimId) {
  OWNERS.add(claimId)
  return () => OWNERS.delete(claimId)
}

/** True when a modal, menu or viewer is open. */
export function keyboardIsOwned() {
  return OWNERS.size > 0
}

/** True when the event came from somewhere the user is typing. */
export function isTypingTarget(target) {
  if (!target) return false
  if (target.isContentEditable) return true
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}