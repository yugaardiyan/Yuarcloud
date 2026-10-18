import React, { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../context/AuthContext'
import Logo from '../components/Logo'
import { Mail, Lock, AlertCircle, Eye, EyeOff } from 'lucide-react'

function Login() {
  const { login } = useAuth()
  const navigate = useNavigate()
  const [formData, setFormData] = useState({ username: '', password: '' })
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [revealed, setRevealed] = useState(false)

  // Nothing typed yet — no point posting a request the server will reject.
  const incomplete = !formData.username || !formData.password

  const handleSubmit = async (e) => {
    e.preventDefault()
    if (incomplete || loading) return

    setLoading(true)
    setError('')
    try {
      await login(formData.username, formData.password)
      // The destination parked by a share page is consumed by RedirectIfAuthed,
      // which fires the moment `user` becomes set and would otherwise undo a
      // navigate() made from here.
      navigate('/cloud')
    } catch (err) {
      setError(err.response?.data?.error || 'Invalid credentials')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="login-page">
      {/* Card and message travel together, so the pair stays centred. */}
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

          <form onSubmit={handleSubmit} noValidate>
            <div className="login-field">
              <div className="login-input-wrap">
                <Mail size={16} className="login-input-icon" />
                <input
                  type="text"
                  className="login-input"
                  placeholder="Email or username"
                  value={formData.username}
                  onChange={(e) => {
                    setFormData((prev) => ({ ...prev, username: e.target.value }))
                    setError('')
                  }}
                  disabled={loading}
                  autoComplete="username"
                  autoFocus
                />
              </div>
            </div>

            <div className="login-field">
              <div className="login-input-wrap">
                <Lock size={16} className="login-input-icon" />
                <input
                  type={revealed ? 'text' : 'password'}
                  className="login-input login-input-pad"
                  placeholder="Password"
                  value={formData.password}
                  onChange={(e) => {
                    setFormData((prev) => ({ ...prev, password: e.target.value }))
                    setError('')
                  }}
                  disabled={loading}
                  autoComplete="current-password"
                />
                <button
                  type="button"
                  className="login-reveal"
                  onClick={() => setRevealed((v) => !v)}
                  disabled={loading}
                  aria-label={revealed ? 'Hide password' : 'Show password'}
                  title={revealed ? 'Hide password' : 'Show password'}
                >
                  {revealed ? <EyeOff size={15} /> : <Eye size={15} />}
                </button>
              </div>
            </div>

            {/* The capsule sits on the card's foot; the card's own bottom
                radius is tuned to hug it, so both curves read as one shape. */}
            <button type="submit" className="login-btn" disabled={loading || incomplete}>
              {loading ? 'Signing in…' : 'Login'}
            </button>
          </form>
        </div>

        {/* Outside the card: a failed sign-in can never resize or shift it.
            The reserved line keeps the pair from jumping when text appears. */}
        <p className="login-notice" role="alert" aria-live="polite">
          {error && (
            <>
              <AlertCircle size={13} />
              <span>{error}</span>
            </>
          )}
        </p>
      </div>
    </div>
  )
}

export default Login