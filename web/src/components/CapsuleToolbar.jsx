import React, { useState, useRef, useEffect } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import {
  Search, X,
} from 'lucide-react'
import {
  MyCloudGlyph, NewFolderGlyph, TrashGlyph, GridGlyph, ListGlyph,
} from './icons'
import { useAuth } from '../context/AuthContext'
import { useFiles } from '../context/FileContext'
import { fireAndForget } from '../utils/fireAndForget'
import PromptModal from './PromptModal'
import ConfirmModal from './ConfirmModal'

/**
 * The entire primary navigation lives in one pill.
 *
 * Left to right: search (expands to fill the pill), drive, trash,
 * view mode, account, and a highlighted create button on the far right.
 */
export default function CapsuleToolbar() {
  const navigate = useNavigate()
  const location = useLocation()
  const { user, isAdmin, logout } = useAuth()
  const {
    view, viewMode, setViewMode,
    searchQuery, setSearchQuery, showSearch, showDrive, showTrash,
    createFolder, uploadFiles, items, emptyTrash, loading,
  } = useFiles()

  const [searchOpen, setSearchOpen] = useState(false)
  const [menu, setMenu] = useState(null) // 'account' | 'create' | null
  const [promptNewFolder, setPromptNewFolder] = useState(false)
  const [emptyTrashOpen, setEmptyTrashOpen] = useState(false)

  const accountRef = useRef(null)
  const createRef = useRef(null)
  const searchInputRef = useRef(null)
  const fileInputRef = useRef(null)
  const folderInputRef = useRef(null)

  // to be standing in — so it is enabled everywhere, exactly like Trash. It only
  // reads as inactive somewhere because it genuinely is not the current view.


  const gotoDrive = (switchScope) => {
    switchScope()
  }

  // ---- search ------------------------------------------------------------

  const openSearch = () => {
    setMenu(null)
    setSearchOpen(true)
  }

  const closeSearch = () => {
    setSearchOpen(false)
    if (searchQuery.trim()) {
      setSearchQuery('')
      gotoDrive(showDrive)
    }
  }

  // Focus once the field is open. Calling focus() forces a style recalc, so
  // the visibility flip that comes with .is-open has already been applied —
  // no rAF or timer guesswork, which matters because rAF is throttled in
  // background tabs.
  useEffect(() => {
    if (searchOpen) searchInputRef.current?.focus()
  }, [searchOpen])

  const submitSearch = (e) => {
    e.preventDefault()
    if (!searchQuery.trim()) return
    gotoDrive(() => showSearch(searchQuery.trim()))
    searchInputRef.current?.blur()
  }

  // Escape closes search first, then any open menu.
  useEffect(() => {
    const onKey = (e) => {
      if (e.key !== 'Escape') return
      if (searchOpen) closeSearch()
      else setMenu(null)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [searchOpen, searchQuery])

  // "/" focuses search from anywhere, as in most file managers.
  useEffect(() => {
    const onKey = (e) => {
      if (['INPUT', 'TEXTAREA'].includes(e.target.tagName)) return
      if (e.key === '/') {
        e.preventDefault()
        openSearch()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])

  // ---- menus -------------------------------------------------------------

  useEffect(() => {
    const onDown = (e) => {
      if (!accountRef.current?.contains(e.target) && !createRef.current?.contains(e.target)) {
        setMenu(null)
      }
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [])

  const go = (fn) => {
    fn()
    setMenu(null)
  }

  // Settings is a surface of its own, not a folder. The drive context is left
  // untouched while it is open (there is no folder to list), so `view.scope`
  // stays 'drive' and the My Cloud tab kept claiming to be the current place.
  // The marker follows the route instead: while Settings is up it belongs to
  // the account button, which is what opened it.
  const onSettings = location.pathname === '/admin' || location.pathname.startsWith('/admin/')
  const driveTabActive = view.scope === 'drive' && !onSettings

  const handlePickFiles = (e) => {
    const picked = Array.from(e.target.files || [])
    if (picked.length) fireAndForget(uploadFiles(picked))
    e.target.value = ''
    setMenu(null)
  }

  const handlePickFolder = (e) => {
    const picked = Array.from(e.target.files || [])
    if (picked.length) fireAndForget(uploadFiles(picked))
    e.target.value = ''
    setMenu(null)
  }

  // ---- pill --------------------------------------------------------------

  return (
    <div className="capsule" data-open={searchOpen || undefined}>
      {/*
        Nav buttons. This is the part that scrolls sideways on narrow phones —
        the pill keeps its shape, and Account/Create stay pinned outside the
        scroller so their dropdown menus are never clipped.
      */}
      <div className={`capsule-row ${searchOpen ? 'capsule-collapsed' : ''}`}>
        <button
          className="capsule-btn"
          onClick={openSearch}
          title="Search ( / )"
          aria-label="Search"
        >
          <Search size={19} />
        </button>

        <button
          className="capsule-btn"
          data-active={driveTabActive || undefined}
          onClick={() => go(() => gotoDrive(showDrive))}
          title="My Cloud"
          aria-label="My Cloud"
          aria-pressed={driveTabActive}
        >
          <MyCloudGlyph />
        </button>



        <button
          className="capsule-btn"
          data-active={view.scope === 'trash' || undefined}
          onClick={() => go(() => gotoDrive(showTrash))}
          title="Trash"
          aria-label="Trash"
          aria-pressed={view.scope === 'trash'}
        >
          <TrashGlyph />
        </button>

        <button
          className="capsule-btn"
          onClick={() => setViewMode(viewMode === 'grid' ? 'list' : 'grid')}
          title={`Switch to ${viewMode === 'grid' ? 'list' : 'grid'} view (${viewMode === 'grid' ? '⌘2' : '⌘1'})`}
          aria-label={`Switch to ${viewMode === 'grid' ? 'list' : 'grid'} view`}
        >
          {viewMode === 'grid' ? <ListGlyph /> : <GridGlyph />}
        </button>
      </div>

      {/* Account */}
      <div className={`capsule-dropdown ${searchOpen ? 'capsule-collapsed' : ''}`} ref={accountRef}>
          <button
            className="capsule-btn"
            onClick={() => {
              // Settings lives in its own chunk. Opening this menu is the
              // earliest point at which it may be wanted, and fetching it then
              // keeps the first click from showing the card vanish while the
              // chunk arrives.
              if (isAdmin) import('../pages/Admin')
              setMenu(menu === 'account' ? null : 'account')
            }}
            title={user?.display_name || user?.username}
            aria-label="Account"
            aria-expanded={menu === 'account'}
            aria-haspopup="true"
            data-active={onSettings || undefined}
            aria-current={onSettings ? 'page' : undefined}
          >
            <span className="capsule-avatar">
              {(user?.display_name || user?.username || 'U')[0].toUpperCase()}
            </span>
          </button>

          {menu === 'account' && (
            <div className="capsule-menu" role="menu">
              <div className="capsule-menu-head">
                <div className="capsule-menu-name">{user?.display_name || user?.username}</div>
              </div>

              {/* Settings is an admin-only surface. */}
              {isAdmin && (
                <button
                  className="capsule-menu-item"
                  role="menuitem"
                  onClick={() => go(() => navigate('/admin'))}
                >
                  Settings
                </button>
              )}

              <button
                className="capsule-menu-item danger"
                role="menuitem"
                onClick={() => { setMenu(null); logout() }}
              >
                Sign out
              </button>
            </div>
          )}
        </div>

        {/* Create / Empty trash — on the far right.
            In the regular scope this is a "+" menu with New folder +
            Upload actions. In the trash scope the create actions are
            hidden (you can't create inside the trash) and the button
            itself becomes a single "Empty trash" action.

            marked, not a place you can put something, so "upload" here had
            no destination to mean: the upload quietly landed in My Cloud's
            root, which is a guess wearing a control's clothes. Trash set
            the precedent in this very slot — a view that is not a location
            does not offer to create inside itself.

            The button stays put either way and only goes inert when there
            is nothing to empty — hiding it made the right end of the pill
            jump sideways on every scope switch, and on an empty trash it
            left a bare gap where a control used to be.

            Emptying is permanent and there is no undo, so it asks first —
            same guard as every other destructive action in the app. */}
        {onSettings ? (
          // Settings has no folder of its own, so there is nothing here to
          // create inside. Before this, the segment still worked and filed the
          // folder into whichever folder was last open — the same silent
          // wrong-destination the trash segment already refuses. It keeps its
          // slot, inert, rather than vanishing and shifting the pill.
          <button
            className="capsule-btn capsule-btn-primary"
            disabled
            title="Files are created in My Cloud"
            aria-label="Create new — available in My Cloud"
          >
            <NewFolderGlyph />
          </button>
        ) : view.scope === 'trash' ? (
          <button
            className="capsule-btn capsule-btn-danger"
            onClick={() => go(() => setEmptyTrashOpen(true))}
            disabled={loading || items.length === 0}
            title={items.length === 0 ? 'Trash is already empty' : 'Empty trash'}
            aria-label="Empty trash"
          >
            <TrashGlyph />
          </button>
        ) : (
          <div className={`capsule-dropdown ${searchOpen ? 'capsule-collapsed' : ''}`} ref={createRef}>
            <button
              className="capsule-btn capsule-btn-primary"
              onClick={() => setMenu(menu === 'create' ? null : 'create')}
              title="New folder / upload"
              aria-label="Create new"
              aria-expanded={menu === 'create'}
              aria-haspopup="true"
            >
              <NewFolderGlyph />
            </button>

            {menu === 'create' && (
              <div className="capsule-menu" role="menu">
                <button
                  className="capsule-menu-item"
                  role="menuitem"
                  onClick={() => { setMenu(null); setPromptNewFolder(true) }}
                >
                  New folder
                </button>
                <div className="capsule-menu-divider" />
                <button className="capsule-menu-item" role="menuitem" onClick={() => fileInputRef.current?.click()}>
                  Upload files
                </button>
                <button className="capsule-menu-item" role="menuitem" onClick={() => folderInputRef.current?.click()}>
                  Upload folder
                </button>
              </div>
            )}
          </div>
        )}

        {/* Search field — expands to fill the pill. */}
      <form className={`capsule-search ${searchOpen ? 'is-open' : ''}`} onSubmit={submitSearch}>
        {/*
          Close sits on the left, next to where the caret lands, so it can be
          hit without dragging the cursor across the field.
        */}
        <button
          type="button"
          className="capsule-search-clear"
          onClick={closeSearch}
          aria-label="Close search"
          tabIndex={searchOpen ? 0 : -1}
        >
          <X size={15} />
        </button>
        <input
          ref={searchInputRef}
          type="search"
          placeholder="Search files"
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          aria-label="Search files"
          tabIndex={searchOpen ? 0 : -1}
        />
        <Search size={17} className="capsule-search-icon" />
      </form>

      <input ref={fileInputRef} type="file" multiple onChange={handlePickFiles} style={{ display: 'none' }} />
      <input
        ref={folderInputRef}
        type="file"
        webkitdirectory=""
        directory=""
        multiple
        onChange={handlePickFolder}
        style={{ display: 'none' }}
      />

      {/* Asked only from a view with no "here". The answer then flows into the
          prompt or the file pick, so nothing is ever filed somewhere the user
          did not choose and did not see. */}

      {promptNewFolder && (
        <PromptModal
          title="New folder"
          label="Folder name"
          placeholder="e.g. Documents"
          confirmLabel="Create"
          onConfirm={createFolder}
          onCancel={() => setPromptNewFolder(false)}
        />
      )}

      {emptyTrashOpen && (
        <ConfirmModal
          simple
          title="Empty trash?"
          confirmLabel="Empty trash"
          danger
          onConfirm={() => { setEmptyTrashOpen(false); emptyTrash() }}
          onCancel={() => setEmptyTrashOpen(false)}
        />
      )}
    </div>
  )
}
