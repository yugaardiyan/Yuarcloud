import React from 'react'
import { MoreVertical } from 'lucide-react'
import { useContextMenu } from './ContextMenuProvider'
import { useFiles } from '../context/FileContext'

/*
 * One way into the file context menu. On a touch screen it stays visible
 * beside the file; on desktop the surrounding card/row reveals it as part of
 * its hover animation, so the resting layout stays quiet.
 *
 * This button always targets its own item, even when several are marked in
 * select mode — like Drive's per-row "⋮". Acting on the whole selection is
 * what right-clicking a selected row is for; the ⋯ must stay predictable
 * ("the actions for this file"), not silently widen to the batch.
 *
 * Because the focus moves to this one item, opening the menu also leaves
 * select mode and drops every mark. That must hold no matter how the batch was
 * built — "Select" in the menu, Select all, Ctrl/Cmd+A, or Ctrl/Cmd-click — so
 * re-selecting is always an explicit new start.
 */
export default function FileMenuButton({ item }) {
  const { openMenu } = useContextMenu()
  const { exitSelectMode, clearSelection } = useFiles()

  const handleClick = (e) => {
    // The ⋯ is a single-item action: any batch marks are dropped first, then
    // the menu opens for this row only.
    exitSelectMode()
    clearSelection()
    openMenu(e, item.id)
  }

  return (
    <button
      type="button"
      className="file-menu-btn"
      aria-label={`More actions for ${item.name}`}
      title="More actions"
      draggable={false}
      onClick={handleClick}
      onDoubleClick={(e) => e.stopPropagation()}
    >
      <MoreVertical size={16} strokeWidth={2.2} />
    </button>
  )
}
