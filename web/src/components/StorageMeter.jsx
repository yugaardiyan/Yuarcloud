import React from 'react'
import { HardDrive } from 'lucide-react'
import { formatBytes } from '../utils/helpers'

/**
 * Storage, and everything else this page knows.
 *
 * This is one card because there is one thing to look at. How full the box is
 * is the only figure an operator acts on, and files, folders, users and share
 * links are all detail hanging off it — the same bytes, counted different ways.
 * They used to be spread across three separate surfaces, which spent more of the
 * screen on borders and padding than on numbers.
 *
 * So the layout is one figure, one bar, one line of detail. The percentage sits
 * opposite the figure because that corner is where a glance lands first, and its
 * colour turns with the threshold — an operator deciding whether to free space
 * reads 94% in amber, not 94% in grey.
 */
export default function StorageMeter({ stats, users = {} }) {
  if (!stats) return null

  const used = stats.total_size || 0
  const quota = stats.total_quota || 0
  const percent = quota > 0 ? Math.min(100, (used / quota) * 100) : 0
  const free = Math.max(0, quota - used)

  const tone = percent >= 90 ? 'critical' : percent >= 70 ? 'warning' : 'normal'

  // "1 shares" is the sort of thing that makes a page look unfinished, and on a
  // self-hosted instance these counts are small — exactly where pluralising is
  // wrong most of the time.
  const plural = (n, one, many) => `${(n ?? 0).toLocaleString()} ${n === 1 ? one : many}`

  const detail = [
    { text: plural(stats.total_files, 'file', 'files') },
    { text: plural(stats.total_folders, 'folder', 'folders') },
    { text: plural(users.total_users, 'user', 'users') },
    { text: plural(users.total_shares, 'share link', 'share links') },
  ]

  return (
    <section className="card storage-meter" aria-label="Storage">
      <div className="storage-meter-head">
        <div className="storage-meter-figure">
          <h2 className="storage-meter-title">
            <HardDrive size={13} />
            Storage
          </h2>
          <div className="storage-meter-amount">
            <span className="storage-meter-used">{formatBytes(used)}</span>
            <span className="storage-meter-quota">of {formatBytes(quota)}</span>
          </div>
        </div>

        <div className={`storage-meter-pct is-${tone}`}>
          <span className="storage-meter-pct-value">{percent.toFixed(percent < 10 ? 1 : 0)}%</span>
          <span className="storage-meter-pct-label">used</span>
        </div>
      </div>

      <div
        className={`progress storage-meter-bar is-${tone}`}
        role="progressbar"
        aria-valuenow={Math.round(percent)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label="Storage used"
      >
        <div className="progress-bar" style={{ width: `${percent}%` }} />
      </div>

      {/* One quiet line, no rule above it: the bar is already the separator, and
          a hairline here would split the card into two stacked sections — the
          very thing this layout is trying not to be. */}
      <p className="storage-meter-detail">
        {detail.map(({ text }, i) => (
          <React.Fragment key={text}>
            {i > 0 && <span className="storage-meter-sep" aria-hidden="true">·</span>}
            <span>{text}</span>
          </React.Fragment>
        ))}
        <span className="storage-meter-sep" aria-hidden="true">·</span>
        <span>{formatBytes(free)} free</span>
      </p>
    </section>
  )
}