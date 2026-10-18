/**
 * The Yuarcloud mark.
 *
 * Uses the real artwork from /public rather than a hand-built SVG — the
 * letterforms are the source file, so nothing is re-approximated. Sizes come
 * from the Logo component; the images are pre-rendered at 256px (max display
 * is 56 CSS px, so still sharp on a 2x screen).
 */
export default function Logo({ size = 30, title = 'Yuarcloud', className = '' }) {
  return (
    <img
      src="/logo.png"
      width={size}
      height={size}
      className={`logo ${className}`.trim()}
      alt={title}
      draggable={false}
    />
  )
}