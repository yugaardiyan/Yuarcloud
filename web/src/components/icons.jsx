import React from 'react'

/**
 * Glyphs drawn for this app rather than pulled from lucide.
 *
 * All of them share one idiom — a 24-unit grid, a 1.5 stroke, round caps and
 * joins — so they sit next to each other without one looking borrowed. Each is
 * painted in `currentColor` and never a literal black: the coloured tile that
 * used to sit behind each icon is gone, so colour comes from the container.
 *
 * Every component takes `size` rather than baking one in, because the same
 * glyph is used at 19px on a capsule button and at 15-16px on a menu row, and
 * the menu layers pass `size` the way they pass it to lucide.
 */

/** Shared attribute set. `extra` wins, for the per-glyph join settings. */
function attrs(size, extra) {
  return {
    viewBox: '0 0 24 24',
    width: size,
    height: size,
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 1.5,
    strokeLinecap: 'round',
    'aria-hidden': true,
    ...extra,
  }
}

/** My Cloud: a folder with a pointer swinging up out of it. */
export function MyCloudGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeLinejoin: 'round' })}>
      <path d="M7.375 16.047h9.25m4.625-6.164v7.698a3.083 3.083 0 0 1-3.083 3.083H5.833a3.083 3.083 0 0 1-3.083-3.083V6.419a3.083 3.083 0 0 1 3.083-3.083h3.084a3.083 3.083 0 0 1 2.57 1.377l.873 1.326a1.748 1.748 0 0 0 1.449.77h4.358a3.084 3.084 0 0 1 3.083 3.074" />
      <path d="m11.517 4.723 6.927-1.11a1.531 1.531 0 0 1 1.778 1.521v2.457" />
    </svg>
  )
}

/**
 * New folder: a folder with a cross struck through its face.
 *
 * One glyph serving both the capsule button and every "New folder" row that
 * opens from it — they are the same action and used to disagree, the button
 * wearing this shape while the rows still wore lucide's FolderPlus.
 *
 * The two paths carry different join settings on purpose: a miter limit on the
 * cross, a round join on the folder. Neither is set on the svg, so the joins
 * are declared per path exactly as the artwork does.
 */
export function NewFolderGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size)}>
      <path strokeMiterlimit="10" d="M11.993 10.307v6.874m-3.43-3.437h6.874" />
      <path strokeLinejoin="round" d="M21.25 9.883v7.698a3.083 3.083 0 0 1-3.083 3.083H5.833a3.083 3.083 0 0 1-3.083-3.083V6.419a3.083 3.083 0 0 1 3.083-3.083h3.084a3.083 3.083 0 0 1 2.57 1.377l.873 1.326a1.748 1.748 0 0 0 1.449.77h4.358a3.084 3.084 0 0 1 3.083 3.074" />
    </svg>
  )
}

/** Trash: a lidded bin. */
export function TrashGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeLinejoin: 'round' })}>
      <path d="M5.47 6.015v12.514a2.72 2.72 0 0 0 2.721 2.721h7.618a2.72 2.72 0 0 0 2.72-2.72V6.014m-15.235.001h17.412" />
      <path d="M8.735 6.015V4.382a1.632 1.632 0 0 1 1.633-1.632h3.264a1.632 1.632 0 0 1 1.633 1.632v1.633" />
    </svg>
  )
}

/** Grid view: nine dots. */
export function GridGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeLinejoin: 'round' })}>
      <path d="M12 6.826A.913.913 0 1 0 12 5a.913.913 0 0 0 0 1.826m6.088 0a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826m-12.176 0a.913.913 0 1 0 0-1.826a.913.913 0 0 0 0 1.826M12 12.913a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826m6.088 0a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826m-12.176 0a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826M12 19a.913.913 0 1 0 0-1.826A.913.913 0 0 0 12 19m6.088 0a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826M5.912 19a.913.913 0 1 0 0-1.826.913.913 0 0 0 0 1.826" />
    </svg>
  )
}

/**
 * List view: three rules.
 *
 * A miter limit instead of a join, matching the artwork. It makes no visible
 * difference here — three straight segments never form a corner — but it is
 * kept so the file matches what it was drawn from.
 */
export function ListGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeMiterlimit: 10 })}>
      <path d="M4.5 12h15m-15 5.77h15M4.5 6.23h15" />
    </svg>
  )
}

