/**
 * Drag payload for moving items between folders.
 *
 * Items are carried in a private MIME type rather than as text or as files.
 * That distinction matters because the drive already treats a drop as an
 * upload: a dragged card carries no `dataTransfer.files`, and without a
 * marker of its own the two cases are indistinguishable to a drop handler.
 */

/* Custom types must be lowercased to survive the drag, which lowercases them. */
export const DRAG_MIME = 'application/x-yuarcloud-items'

/** Read the dragged ids, or null when this is not one of our own drags. */
export function readDragIds(dataTransfer) {
  if (!dataTransfer) return null
  const raw = dataTransfer.getData(DRAG_MIME) || dataTransfer.getData(DRAG_MIME.toLowerCase())
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) && parsed.length ? parsed : null
  } catch {
    return null
  }
}

/**
 * Whether an in-app move drag is in flight — readable at any point in the drag.
 *
 * This must not go through getData(). A DataTransfer runs in "protected mode"
 * for dragenter and dragover, where every getData() call returns an empty
 * string so a page cannot sniff what is being dragged past it. The types list
 * is exempt and still reports the custom type. Testing with getData there is
 * why the folder tiles never became drop targets and no move ever happened.
 */
export function hasInternalDrag(dataTransfer) {
  if (!dataTransfer) return false
  return Array.from(dataTransfer.types || []).some(
    (t) => t.toLowerCase() === DRAG_MIME
  )
}

export function setDragPayload(event, ids) {
  const dt = event.dataTransfer
  if (!dt) return
  dt.setData(DRAG_MIME, JSON.stringify(ids))
  dt.setData('text/plain', ids.join('\n'))
  dt.effectAllowed = 'move'
}