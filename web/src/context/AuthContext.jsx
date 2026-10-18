import React, { createContext, useContext, useState, useEffect, useCallback } from 'react'
import { authApi } from '../utils/api'

const AuthContext = createContext(null)

export function AuthProvider({ children }) {
  const [user, setUser] = useState(null)
  const [loading, setLoading] = useState(true)
  const [initialized, setInitialized] = useState(false)

  const fetchUser = useCallback(async () => {
    // A visitor on a public share link has no token and no reason to probe
    // /api/auth/me — skip the request to avoid a pointless error toast.
    if (!localStorage.getItem('access_token')) {
      setUser(null)
      setLoading(false)
      setInitialized(true)
      return
    }
    try {
      const response = await authApi.me()
      setUser(response.data.user)
    } catch (error) {
      setUser(null)
      localStorage.removeItem('access_token')
      localStorage.removeItem('refresh_token')
    } finally {
      setLoading(false)
      setInitialized(true)
    }
  }, [])

  useEffect(() => {
    const token = localStorage.getItem('access_token')
    if (token) {
      fetchUser()
    } else {
      setLoading(false)
      setInitialized(true)
    }
  }, [fetchUser])

  const login = useCallback(async (username, password) => {
    const response = await authApi.login(username, password)
    const { user, tokens } = response.data
    localStorage.setItem('access_token', tokens.access_token)
    localStorage.setItem('refresh_token', tokens.refresh_token)
    setUser(user)
    return user
  }, [])

  const logout = useCallback(async () => {
    try {
      await authApi.logout()
    } catch (error) {
      // Ignore logout errors
    }
    localStorage.removeItem('access_token')
    localStorage.removeItem('refresh_token')
    setUser(null)
  }, [])

  const updateUser = useCallback((updates) => {
    setUser(prev => prev ? { ...prev, ...updates } : null)
  }, [])

  // Re-reads the session from the server.
  //
  // Two callers. After a password change, which bumps the account's token
  // version so the access token already in localStorage is refused from that
  // moment on: without re-reading, the success screen would be immediately
  // followed by 401s and a redirect to /login — the user's own correct action
  // appearing to have signed them out.
  //
  // And after a change to stored files. used_bytes is a counter the server
  // maintains on the user row, and the status bar renders the copy this context
  // holds, so without a re-read it went stale on the first upload and stayed
  // wrong until the page was reloaded.
  const refreshUser = useCallback(async () => {
    try {
      const response = await authApi.me()
      setUser(response.data.user)
      return response.data.user
    } catch {
      // Leave the session alone: the caller decides what a failure means, and
      // clearing here would sign the user out for a transient network blip.
      return null
    }
  }, [])

  const value = {
    user,
    loading,
    initialized,
    login,
    logout,
    updateUser,
    refreshUser,
    isAuthenticated: !!user,
    isAdmin: user?.role === 'admin',
  }

  return (
    <AuthContext.Provider value={value}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  const context = useContext(AuthContext)
  if (!context) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return context
}