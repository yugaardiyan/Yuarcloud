import React, { useState } from 'react'
import { useViewNavigate } from '../utils/viewTransition'
import { useAuth } from '../context/AuthContext'
import { authApi, apiError } from '../utils/api'
import Logo from '../components/Logo'
import { Lock, AlertCircle, Eye, EyeOff, CheckCircle } from 'lucide-react'

/**
 * The only screen a default-credential account can reach.
 *
 * The server refuses every other route for an account still on the password
 * the repository, the example config and the container image all publish, so
 * without this the account is locked out of the panel it administers — and the
 * operator is left with a server that logs in and then 403s everything, with
 * no indication of why.
 *
 * Deliberately narrow: no navigation, no file browser, nothing else. It is not
 * a settings page that happens to be recommended; it is the whole surface.
 * The rules mirror the server's (minimum length, confirmation) so the form
 * cannot pass something the endpoint will reject.
 */
function ChangePassword() {
  const { user, refreshUser } = useAuth()
  const navigate = useViewNavigate()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [revealed, setRevealed] = useState(false)
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState(false)

  const tooShort = next.length > 0 && next.length < 8
  const mismatch = confirm.length > 0 && confirm !== next
  const canSubmit = current && next && confirm && next.length >= 8 && !mismatch && !busy

  const submit = async (e) => {
    e.preventDefault()
    if (!canSubmit) return

    setBusy(true)
    setError('')
    try {
      const response = await authApi.changePassword({ current_password: current, new_password: next })

      // The change bumps the account's token version, which kills the pair this
      // request arrived on — so the server mints a replacement and returns it.
      // Without adopting it the very next call 401s, the interceptor clears the
      // tokens and redirects, and securing your own account signs you out of
      // it. Refreshing instead cannot work either: the refresh token died in the
      // same bump.
      const tokens = response.data?.tokens
      if (tokens?.access_token) {
        localStorage.setItem('access_token', tokens.access_token)
        if (tokens.refresh_token) localStorage.setItem('refresh_token', tokens.refresh_token)
      }

      await refreshUser?.()
      setDone(true)
      setTimeout(() => navigate('/cloud'), 1200)
    } catch (err) {
      setError(apiError(err, 'Could not change the password'))
      setBusy(false)
    }
  }

  return (
    <div className="login-page">
      <div className="login-panel">
        <div className="card login-card">
          <div className="login-brand">
            <span className="login-brand-mark">
              <Logo size={40} />
            </span>
            <span className="login-brand-name">
              <span className="login-brand-lead">Yuar</span>
              <span className="login-brand-tail">cloud</span>
            </span>
          </div>

          {done ? (
            <div className="login-field">
              <p className="auth-hint">
                <CheckCircle size={16} /> Password updated. Taking you to your files…
              </p>
            </div>
          ) : (
            <>
              <div className="auth-hint auth-hint-warn">
                <AlertCircle size={16} />
                <span>
                  {user?.username ? `“${user.username}” is still on the default password` : 'This account is still on the default password'}.
                  Set a new one to finish signing in.
                </span>
              </div>

              <form onSubmit={submit} noValidate>
                <div className="login-field">
                  <div className="login-input-wrap">
                    <Lock size={16} className="login-input-icon" />
                    <input
                      type={revealed ? 'text' : 'password'}
                      className="login-input"
                      placeholder="Current password"
                      value={current}
                      onChange={(e) => setCurrent(e.target.value)}
                      autoComplete="current-password"
                      autoFocus
                    />
                  </div>
                </div>

                <div className="login-field">
                  <div className="login-input-wrap">
                    <Lock size={16} className="login-input-icon" />
                    <input
                      type={revealed ? 'text' : 'password'}
                      className="login-input"
                      placeholder="New password"
                      value={next}
                      onChange={(e) => setNext(e.target.value)}
                      autoComplete="new-password"
                    />
                    <button
                      type="button"
                      className="login-reveal"
                      onClick={() => setRevealed((v) => !v)}
                      aria-label={revealed ? 'Hide password' : 'Show password'}
                      title={revealed ? 'Hide password' : 'Show password'}
                    >
                      {revealed ? <EyeOff size={16} /> : <Eye size={16} />}
                    </button>
                  </div>
                  {tooShort && <p className="login-error">At least 8 characters.</p>}
                </div>

                <div className="login-field">
                  <div className="login-input-wrap">
                    <Lock size={16} className="login-input-icon" />
                    <input
                      type={revealed ? 'text' : 'password'}
                      className="login-input"
                      placeholder="Confirm new password"
                      value={confirm}
                      onChange={(e) => setConfirm(e.target.value)}
                      autoComplete="new-password"
                    />
                  </div>
                  {mismatch && <p className="login-error">The two passwords do not match.</p>}
                </div>

                {error && (
                  <p className="login-error login-error-banner">
                    <AlertCircle size={14} /> {error}
                  </p>
                )}

                <button type="submit" className="login-btn" disabled={!canSubmit}>
                  {busy ? 'Updating…' : 'Set new password'}
                </button>
              </form>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

export default ChangePassword