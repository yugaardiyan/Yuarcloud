import React from 'react'
import { useViewNavigate } from '../utils/viewTransition'
import CapsuleToolbar from './CapsuleToolbar'
import Logo from './Logo'

function Header() {
  const navigate = useViewNavigate()

  return (
    <header className="app-header">
      <div className="header-inner">
        <button className="brand" onClick={() => navigate('/cloud')} aria-label="Yuarcloud home">
          <span className="brand-mark">
            <Logo size={34} />
          </span>
          <span className="brand-name">
            <span className="brand-name-lead">Yuar</span>
            <span className="brand-name-tail">cloud</span>
          </span>
        </button>

        <div className="header-spacer" />

        {/* Every primary action lives in a single pill. */}
        <CapsuleToolbar />
      </div>
    </header>
  )
}

export default Header
