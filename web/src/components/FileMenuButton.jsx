import React from 'react'
import { MoreVertical } from 'lucide-react'
import { useContextMenu } from './ContextMenuProvider'

/*
 * One way into the file context menu. On a touch screen it stays visible
 * beside the file; on desktop the surrounding card/row reveals it as part of
 * its hover animation, so the resting layout stays quiet.
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
