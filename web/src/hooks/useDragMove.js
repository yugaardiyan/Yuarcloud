import { useCallback, useRef, useState } from 'react'
import { useFiles } from '../context/FileContext'
import { hasInternalDrag, readDragIds, setDragPayload } from '../utils/dragItems'

/**
 * Drag a file or folder onto a folder to move it there.
 *
 * Shared by the grid and the list so both behave identically — the same
 * validity rules, the same selection semantics, the same highlight.
 *
 * Validity is decided here rather than by the server so the pointer never
 * shows a move cursor over a target that will be refused. Two cases are
 * caught: a file (not a folder), and a folder that is part of the drag —
 * which is only ever a folder dropped onto itself, since every other folder
 * on screen is its sibling. The server independently refuses a folder dropped
 * into its own descendant, which cannot be seen from one directory level.
 */
export function useDragMove() {
  const { moveItems, selected, isSelected } = useFiles()
  const [hoverId, setHoverId] = useState(null)
  // Ref, not state: the drag handlers all fire within one drag and re-rendering
  // on every dragstart/dragover would be pure waste.
  const dragging = useRef([])

  const onDragStart = useCallback((event, item) => {
    // Grabbing one item of a selection grabs the lot, the way every file
    // manager does. Grabbing an item outside the selection replaces it.
    const ids =
      selected.size > 1 && isSelected(item.id) ? Array.from(selected) : [item.id]
    dragging.current = ids
    setDragPayload(event, ids)
  }, [selected, isSelected])

  const onDragEnd = useCallback(() => {
    dragging.current = []
    setHoverId(null)
  }, [])

  /** True when this folder would actually accept the current drag. */
  const accepts = useCallback((item) => {
    if (!item.is_dir) return false
    if (!dragging.current.length) return false
    return !dragging.current.includes(item.id)
  }, [])

  const makeDropProps = useCallback((item) => {
    if (!item.is_dir) return {}

    const onDragOver = (event) => {
      if (!hasInternalDrag(event.dataTransfer)) return
      if (!accepts(item)) return
      // Both are required: preventDefault marks this element as a valid drop
      // target, stopPropagation keeps the page-level upload handler from
      // claiming the drag and tinting the whole content area.
      event.preventDefault()
      event.stopPropagation()
      event.dataTransfer.dropEffect = 'move'
    }

    const onDragEnter = (event) => {
      if (!hasInternalDrag(event.dataTransfer)) return
      if (!accepts(item)) return
      event.preventDefault()
      event.stopPropagation()
      setHoverId(item.id)
    }

    const onDragLeave = (event) => {
      // Leaving a child element fires this too, so only clear when the
      // pointer has genuinely left the folder tile.
      if (event.currentTarget.contains(event.relatedTarget)) return
      setHoverId((cur) => (cur === item.id ? null : cur))
    }

    const onDrop = async (event) => {
      const ids = readDragIds(event.dataTransfer)
      event.preventDefault()
      event.stopPropagation()
      setHoverId(null)
      if (!ids || !ids.length || !accepts(item)) return
      dragging.current = []
      try {
        await moveItems(ids, { target_parent_id: item.id, target_path: item.path })
      } catch {
        /* moveItems already surfaced the failure */
      }
    }

    return {
      onDragOver,
      onDragEnter,
      onDragLeave,
      onDrop,
      className: hoverId === item.id ? 'is-drop-target' : undefined,
    }
  }, [accepts, hoverId, moveItems])

  /** Props for any item, dragged or not. */
  const itemProps = useCallback((item) => ({
    draggable: true,
    onDragStart: (event) => onDragStart(event, item),
    onDragEnd,
    ...makeDropProps(item),
  }), [makeDropProps, onDragEnd, onDragStart])

  return { itemProps, hoverId }
}