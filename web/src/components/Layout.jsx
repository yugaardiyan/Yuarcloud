import React from 'react'
import { Outlet, useLocation } from 'react-router-dom'
import Header from './Header'
import UploadTray from './UploadTray'
import ToastHost from './ToastHost'
import ContextMenuProvider from './ContextMenuProvider'

/**
 * App shell. Every primary action lives in the header capsule, so there is no
 * sidebar — the main area gets the full width.
 */

// Routes whose body is wrapped in the rounded card. Trash and search render the
// same Drive component as /cloud, so they get the same card and the same header
// treatment; leaving them out is what left their capsules 4px proud of the card
// edge with the header's divider still drawn beneath them.
// listing page rendered by Drive, not a folder under '/cloud', so it would
// otherwise miss the card and the header alignment that every other listing
// has — visible as the page background bleeding through where the card should
// be.
const CARDED_ROUTES = ['/cloud', '/trash', '/search', '/admin']

/*
 * Pages that render the status bar. Tracked separately from CARDED_ROUTES
 * because admin has the card but not the bar, and the upload tray anchors
 * itself just above the bar — assuming one everywhere leaves it floating 40px
 * off the bottom of a viewport that has nothing down there.
 */
const STATUS_BAR_ROUTES = ['/cloud', '/trash', '/search']

function Layout() {
  // The header lines up with the card's edges on every carded page. Marking the
  // shell from the route rather than sniffing the DOM with `:has()` keeps this
  // working on browsers without `:has()` support — where the rule would be
  // dropped silently and the header would quietly sit 4px off the card with
  // nothing in the build to say so.
  const { pathname } = useLocation()
  const onCardedPage = CARDED_ROUTES.some((route) => pathname.startsWith(route))
  const hasStatusBar = STATUS_BAR_ROUTES.some((route) => pathname.startsWith(route))

  return (
    <ContextMenuProvider>
      <div
        className={`app${onCardedPage ? ' is-drive' : ''}${hasStatusBar ? ' has-status-bar' : ''}`}
      >
        <Header />

        <div className="app-body">
          <main className="app-main">
            <Outlet />
          </main>
        </div>

        <UploadTray />
        <ToastHost />
      </div>
    </ContextMenuProvider>
  )
}

export default Layout
