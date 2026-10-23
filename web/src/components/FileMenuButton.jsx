import React from 'react'
import { MoreVertical } from 'lucide-react'
import { useContextMenu } from './ContextMenuProvider'

/*
 * A visible way into the file context menu on a touch screen, where a
 * right-click does not exist. The button is hidden by CSS above the phone
 * breakpoint so the desktop card and row keep their current layout.
 */
export default function FileMenuButton({ item, multi = false }) {
  const { openMenu } = useContextMenu()

  return (
    <button
      type="button"
      className="file-menu-btn"
      aria-label={`More actions for ${item.name}`}
      title="More actions"
      draggable={false}
      onClick={(e) => openMenu(e, item.id, multi)}
      onDoubleClick={(e) => e.stopPropagation()}
    >
      <MoreVertical size={16} strokeWidth={2.2} />
    </button>
  )
}
