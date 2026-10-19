import React, { Suspense, lazy } from 'react'
import { Routes, Route, Navigate, useParams } from 'react-router-dom'
import { useAuth } from './context/AuthContext'
import Layout from './components/Layout'
import Login from './pages/Login'
import ChangePassword from './pages/ChangePassword'
import Drive from './pages/Drive'
import { POST_LOGIN_REDIRECT } from './utils/postLoginRedirect'

/*
 * Split by who actually arrives on each one.
 *
 * Admin is the largest page in the app and only an administrator ever opens
 * it; ShareView is for someone following a link, who has no session and never
 * sees a drive. Both were static imports, so a visitor opening a share link
 * downloaded the settings, the user table and the chunked-upload form along
 * with it. Login, ChangePassword and Drive stay eager: they are where
 * everyone lands, and a deferred chunk there would only add a round trip to
 * the common path.
 */
const Admin = lazy(() => import('./pages/Admin'))
const ShareView = lazy(() => import('./pages/ShareView'))
const NotFound = lazy(() => import('./pages/NotFound'))

/** Sends an old /s/<token> share link to its /yc/<token> equivalent. */
function LegacyShareRedirect() {
  const { token } = useParams()
  return <Navigate to={`/yc/${token}`} replace />
}

function RequireAuth({ children, adminOnly = false }) {
  const { user, loading, initialized, isAdmin } = useAuth()

  // Nothing is drawn while the session resolves. This used to be a splash with
  // the logo and "Loading Yuarcloud…", which flashed for a few frames on every
  // start and said nothing the browser had not already shown. The app's own
  // background is up by then, and the shell enters as soon as it is known.
  if (loading || !initialized) return null
  if (!user) return <Navigate to="/login" replace />
  if (adminOnly && !isAdmin) return <Navigate to="/cloud" replace />

  return children
}

/**
 * Confines an account still on the default password to the one screen that
 * fixes it.
 *
 * The server already refuses every other route for such an account, so without
 * this the user saw a drive full of failed requests instead of a form: the app
 * loaded, the session was valid, and nothing else worked. Routing to the
 * change-password page makes the reason visible, and makes the confinement
 * look deliberate rather than broken.
 */
function RequirePasswordRotation({ children }) {
  const { user, loading, initialized } = useAuth()

  if (loading || !initialized) return null
  if (!user) return <Navigate to="/login" replace />
  if (user.must_rotate_password) return <Navigate to="/change-password" replace />

  return children
}

/**
 * Keeps signed-in visitors off /login. A share page can park a destination
 * (see POST_LOGIN_REDIRECT) before sending an anonymous visitor here; this
 * guard is the one that consumes it, because it runs the instant `user`
 * becomes set — any navigate() from the login form itself would race it.
 */
function RedirectIfAuthed({ children }) {
  const { user, loading, initialized } = useAuth()
  if (loading || !initialized) return null
  if (user) {
    // A parked share destination must not override the rotation: that account
    // cannot reach any file route yet, so sending it to one would land on a
    // redirect loop back to this same page.
    if (user.must_rotate_password) return <Navigate to="/change-password" replace />
    const target = sessionStorage.getItem(POST_LOGIN_REDIRECT)
    if (target) sessionStorage.removeItem(POST_LOGIN_REDIRECT)
    return <Navigate to={target || '/cloud'} replace />
  }
  return children
}

export default function App() {
  return (
    // A deferred page (Settings, a share link) holds the frame it is arriving
    // into rather than announcing itself: the guards above already return
    // nothing while a session resolves, so there is no splash to match, and a
    // chunk that takes a moment shows as the shell it belongs to.
    <Suspense fallback={null}>
      <Routes>
        <Route path="/login" element={<RedirectIfAuthed><Login /></RedirectIfAuthed>} />

        {/* Reachable with or without a session, so an operator who is locked out
            is not locked out of the page that explains why. */}
        <Route path="/change-password" element={<ChangePassword />} />

        {/* Authenticated shell. Nested paths below /cloud map to folder paths.
            Trash and search are routes of their own rather than a field in React
            state: the browser only replays the URL on a refresh, so anything that
            is not in the address bar cannot come back after one. */}
        <Route element={<RequireAuth><RequirePasswordRotation><Layout /></RequirePasswordRotation></RequireAuth>}>
          <Route index element={<Navigate to="/cloud" replace />} />
          <Route path="cloud" element={<Drive />} />
          <Route path="cloud/*" element={<Drive />} />
          <Route path="trash" element={<Drive />} />
          <Route path="search" element={<Drive />} />
          <Route path="admin" element={<RequireAuth adminOnly><Admin /></RequireAuth>} />
          <Route path="admin/*" element={<RequireAuth adminOnly><Admin /></RequireAuth>} />
        </Route>

        {/* Public */}
        <Route path="yc/:token" element={<ShareView />} />
        {/* Share links used to be /s/<token>. Redirect rather than drop them:
            links already handed out still have to land on the share page. */}
        <Route path="s/:token" element={<LegacyShareRedirect />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </Suspense>
  )
}
