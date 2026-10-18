import { Toaster } from 'react-hot-toast'

/**
 * Where notifications appear, and how they look.
 *
 * Mounted inside `.app` in Layout rather than at the React root. That is what
 * lets the container sit above the status bar: `--below-card-h` is set per
 * route on `.app` and differs between the drive (40px, a status bar under the
 * card) and settings (12px, no bar), and react-hot-toast renders its container
 * in place rather than through a portal — so the variable reaches it. At the
 * root it inherited nothing and the offset had to be hardcoded for one page.
 *
 * All of the appearance is CSS. `style` here would land as an inline style on
 * the container and lose to the library's own inline `bottom`, so the few
 * things that cannot be done from a stylesheet are handled by `visibleDuration`
 * and the icon themes instead.
 */
export default function ToastHost() {
  return (
    <Toaster
      position="bottom-right"
      // Short. A notification that lingers is a notification that eventually
      // covers something; the tray and the status bar both live down here.
      duration={3200}
      toastOptions={{
        // One line, no wrapping into a ragged block against the card.
        className: 'toast-card',
        style: { padding: 0 },
        success: {
          iconTheme: { primary: 'var(--accent-success)', secondary: 'var(--bg-primary)' },
        },
        error: {
          iconTheme: { primary: 'var(--accent-danger)', secondary: 'var(--bg-primary)' },
        },
      }}
      containerStyle={{ pointerEvents: 'none' }}
    />
  )
}