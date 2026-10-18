import { MyCloudGlyph, TrashGlyph } from './icons'

/**
 * The empty view, as one unit.
 *
 * Both the grid and the list used to assemble this inline, and they drifted
 * apart: each picked the icon itself, and one rendered a title while the other
 * rendered a hint — so "This folder is empty" came out 13px and light while
 * "Trash is empty" came out 16px and semibold, on the same screen, in the same
 * corner. Neither looked wrong on its own. They only read as inconsistent
 * because they were.
 *
 * One component, one type scale, one position. Whatever glyph a scope asks for
 * lands in the same box at the same size.
 */

/* Each scope carries the icon it is about, matching the tab that selected it —
   so an empty Trash shows the same mark as the trash tab, and an empty My Cloud
   the same mark as the cloud tab, rather than a generic folder for both. */
const SCOPES = {
  drive: { Glyph: MyCloudGlyph, message: 'This folder is empty' },
  trash: { Glyph: TrashGlyph, message: 'Trash is empty' },
  search: { Glyph: MyCloudGlyph, message: 'No files match your search' },
}

export default function EmptyState({ scope, minHeight = 320 }) {
  const { Glyph, message } = SCOPES[scope] || SCOPES.drive

  return (
    <div className="empty-state" style={{ minHeight }}>
      {/* The glyph itself takes no className: `attrs()` in icons.jsx builds the
          SVG's attributes from size and stroke options only, so a className
          passed to it is dropped on the floor. The wrapper carries the styling
          and the glyph inherits `currentColor` from it. */}
      <div className="empty-state-icon">
        <Glyph size={64} />
      </div>
      <h3 className="empty-state-title">{message}</h3>
    </div>
  )
}