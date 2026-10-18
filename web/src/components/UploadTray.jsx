import { X, CheckCircle, AlertCircle, FolderUp, FileUp } from 'lucide-react'
import { useFiles } from '../context/FileContext'
import { formatBytes } from '../utils/helpers'
import { formatRate, formatEta } from '../utils/uploadSpeed'

/** Floating panel showing in-flight uploads with per-file progress. */
export default function UploadTray() {
  const { uploads, clearUploads } = useFiles()

  if (!uploads.length) return null

  const active = uploads.filter((u) => u.status === 'uploading' || u.status === 'pending')
  const failed = uploads.filter((u) => u.status === 'error')
  const done = uploads.filter((u) => u.status === 'done')

  // The header names whatever the panel is currently doing. "Uploading" while
  // anything is moving, then what ended up happening once nothing is — a count
  // of failures matters more than a count of successes, because it is the one
  // that needs acting on.
  const heading = () => {
    if (active.length) return `Uploading (${active.length} in progress)`
    if (failed.length) {
      return done.length
        ? `Upload finished — ${failed.length} failed, ${done.length} uploaded`
        : `Upload finished — ${failed.length} failed`
    }
    if (done.length) {
      return done.length === 1 ? '1 file uploaded' : `${done.length} files uploaded`
    }
    return 'Uploads'
  }

  // What this row can honestly add to the percentage: a rate once there is
  // enough history to measure one, and a remaining time once there is a rate
  // to divide by. Either can be absent on its own.
  const inFlight = (job) => {
    const parts = []
    const rate = formatRate(job.bytesPerSec, formatBytes)
    if (rate) parts.push(rate)
    const eta = formatEta(job.etaSec)
    if (eta) parts.push(`${eta} left`)
    return parts
  }

  return (
    <div className="upload-tray" role="region" aria-label="Uploads">
      <div className="upload-tray-header">
        <span>{heading()}</span>
        {/* `.icon-close`, not `.btn-icon.btn-sm`: those carry 8px and 12px
            padding, which rendered this as a 39x27 oval next to a row of
            circular Xs elsewhere in the app. */}
        <button className="icon-close" onClick={clearUploads} aria-label="Dismiss">
          <X size={15} />
        </button>
      </div>

      <div className="upload-tray-body">
        {uploads.map((job) => (
          <div key={job.id} className={`upload-row${job.status === 'done' ? ' is-done' : ''}`}>
            <div className={`upload-row-icon ${job.status}`}>
              {job.status === 'done' && <CheckCircle size={15} />}
              {job.status === 'error' && <AlertCircle size={15} />}
              {/* The row already shows a progress bar and a percentage, so the
                  status icon stays static rather than spinning. */}
              {job.status === 'uploading' && <FolderUp size={15} />}
              {job.status === 'pending' && <FileUp size={15} />}
            </div>

            <div className="upload-row-main">
              <div className="upload-row-top">
                <span className="upload-row-name truncate" title={job.name}>{job.name}</span>
                <span className="upload-row-size">{formatBytes(job.size)}</span>
              </div>
              {/* A finished row has nothing left to measure, so the bar goes
                  rather than sitting at a permanent 100%. The label below is
                  the receipt; a full bar next to it says nothing more. */}
              {job.status !== 'done' && (
                <div className="upload-bar">
                  <div className={`upload-bar-fill ${job.status}`} style={{ width: `${job.progress}%` }} />
                </div>
              )}
              <div className="upload-row-status">
                {job.status === 'pending' && 'Waiting…'}
                {job.status === 'uploading' && (
                  <>
                    {job.progress}%
                    {/* Speed and remaining time, each appearing only once it is
                        knowable. Early in a transfer there is not enough history
                        to say anything honest, and a confident "0 B/s" or a
                        wildly wrong ETA is worse than silence. */}
                    {inFlight(job).length > 0 && (
                      <span className="upload-row-detail"> · {inFlight(job).join(' · ')}</span>
                    )}
                  </>
                )}
                {job.status === 'done' && 'Upload completed'}
                {job.status === 'error' && (job.error || 'Failed')}
              </div>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

