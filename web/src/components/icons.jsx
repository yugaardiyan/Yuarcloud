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

/**
 * Add user: a person with a plus, used by the capsule create slot while the
 * Users tab of Settings is open. The plus sits in the gap left by the head
 * circle, the same place the new-folder cross occupies.
 */
export function AddUserGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeLinejoin: 'round' })}>
      <path d="M12.125 14.719c-3.6 0-7.62 2.928-7.62 6.526m7.62-9.785a4.362 4.362 0 0 0 4.035-2.683a4.355 4.355 0 0 0-3.17-5.948a4.362 4.362 0 0 0-5.215 4.274a4.356 4.356 0 0 0 4.35 4.357" />
      <path strokeMiterlimit="10" d="M16.488 14.983v5.997m-2.993-2.992h6" />
    </svg>
  )
}

/**
 * Save configuration: the same folder-plus artwork the create slot wears,
 * reused while the Chunked Upload tab is open because saving that form is
 * the tab's only producing action — the icon says "commit this".
 */
export function SaveConfigGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size)}>
      <path d="M21.25 9.16v7.987a4.1 4.1 0 0 1-1.204 2.901a4.113 4.113 0 0 1-2.906 1.202H6.86a4.113 4.113 0 0 1-2.906-1.202a4.1 4.1 0 0 1-1.204-2.901V6.853a4.1 4.1 0 0 1 1.204-2.901A4.113 4.113 0 0 1 6.86 2.75h8.35a3.004 3.004 0 0 1 2.25.998l3 3.415c.501.545.783 1.256.79 1.997" />
      <path d="M7 21.22v-5.241a1.995 1.995 0 0 1 2-1.997h6a2.002 2.002 0 0 1 2 1.997v5.241M15.8 2.81v4.183a1.526 1.526 0 0 1-1.52 1.528H9.72A1.531 1.531 0 0 1 8.2 6.993V2.75m1.946 15.108h3.708" />
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

/**
 * Reload: a half-arc with an arrowhead at each end, the familiar circular
 * refresh mark.
 */
export function ReloadGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeMiterlimit: 10 })}>
      <path d="M18.024 7.043A8.374 8.374 0 0 0 3.74 12.955" />
      <path d="m17.35 2.75l.832 3.372a1.123 1.123 0 0 1-.854 1.382l-3.372.843" />
      <path d="M5.976 16.957a8.374 8.374 0 0 0 14.285-5.912" />
      <path d="m6.65 21.25l-.832-3.372a1.124 1.124 0 0 1 .855-1.382l3.371-.843" />
    </svg>
  )
}

/**
 * Unlimited: the infinity mark, used wherever a 0-byte quota reads as
 * "no limit" rather than "nothing allowed".
 */
export function UnlimitedGlyph({ size = 19 }) {
  return (
    <svg {...attrs(size, { strokeLinejoin: 'round' })}>
      <path d="M10.18 9.32C9.36 8.19 8.05 7.45 6.54 7.45C4.03 7.45 1.99 9.49 1.99 12C1.99 14.51 4.03 16.55 6.54 16.55C8.23 16.55 9.8 15.66 10.67 14.21L12 12L13.32 9.79C14.19 8.34 15.76 7.45 17.45 7.45C19.96 7.45 22 9.49 22 12C22 14.51 19.96 16.55 17.45 16.55C15.95 16.55 14.64 15.81 13.81 14.68" />
    </svg>
  )
}

