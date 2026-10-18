import React from 'react'
import { Link } from 'react-router-dom'
import { HardDrive } from 'lucide-react'

function NotFound() {
  return (
    <div style={{
      minHeight: '100vh',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      padding: 24,
      background: 'var(--bg-secondary)',
    }}>
      <div style={{ textAlign: 'center' }}>
        <div style={{
          width: 72,
          height: 72,
          borderRadius: 16,
          background: 'linear-gradient(135deg, var(--accent-primary), #7048e8)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          margin: '0 auto 24px',
        }}>
          <HardDrive size={32} color="white" />
        </div>
        <h1 style={{ fontSize: 64, fontWeight: 700, color: 'var(--text-primary)', marginBottom: 8, lineHeight: 1 }}>
          404
        </h1>
        <p style={{ fontSize: 16, color: 'var(--text-secondary)', marginBottom: 8 }}>
          Page not found
        </p>
        <p style={{ fontSize: 13, color: 'var(--text-tertiary)', marginBottom: 24 }}>
          The page you're looking for doesn't exist or has been moved.
        </p>
        <Link to="/cloud" className="btn btn-primary">
          Back to My Cloud
        </Link>
      </div>
    </div>
  )
}

export default NotFound