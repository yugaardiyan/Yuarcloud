/**
 * Turn a drag-and-drop DataTransfer into the list of files to upload.
 *
 * A dropped folder is not a file. The browser exposes it as a directory entry
 * on `dataTransfer.items`, and `dataTransfer.files` either omits it or hands
 * back a zero-byte placeholder named after the folder. Uploading that
 * placeholder files the folder's own name as an empty file and loses everything
 * inside it — the pick "nothing happened", and a bogus row sits in the tray.
 *
 * Walking the entries with `webkitGetAsEntry` and stamping each file with a
 * `webkitRelativePath` turns a drop into exactly the shape a folder pick from
 * `<input webkitdirectory>` already has, so `uploadFiles` rebuilds the folder
 * tree and the files land where the pointer said they would.
 */

export function filesFromDataTransfer(dataTransfer) {
  const plain = () => Array.from(dataTransfer?.files || [])
  const items = dataTransfer?.items
  if (!items || !items.length) return Promise.resolve(plain())

  // A drag can also carry text or a URL. webkitGetAsEntry is not universal, and
  // without it there is nothing to recurse into, so those drags stay as they
  // were.
  const supportsEntries = typeof items[0]?.webkitGetAsEntry === 'function'
  if (!supportsEntries) return Promise.resolve(plain())

  const pending = []
  for (const item of Array.from(items)) {
    if (item.kind !== 'file') continue
    const entry = item.webkitGetAsEntry?.()
    if (!entry) continue
    if (entry.isDirectory) {
      pending.push(walkDirectory(entry, entry.name))
    } else {
      const file = item.getAsFile()
      if (file) pending.push(Promise.resolve([withRelativePath(file, '')]))
    }
  }

  if (!pending.length) return Promise.resolve(plain())

  return Promise.all(pending).then((groups) => {
    const files = groups.flat().filter(Boolean)
    // A directory that could not be read yields nothing at all; fall back so
    // the drop is not silently ignored.
    return files.length ? files : plain()
  })
}

/** Depth-first walk of a dropped directory, tagging each file with its path. */
function walkDirectory(directoryEntry, prefix) {
  const out = []
  return new Promise((resolve) => {
    const reader = directoryEntry.createReader()
    // readEntries returns at most a batch at a time and an empty array only when
    // the directory is exhausted, so it has to be called until then.
    const readBatch = () => {
      reader.readEntries(
        async (entries) => {
          if (!entries.length) {
            resolve(out)
            return
          }
          for (const entry of entries) {
            if (entry.isDirectory) {
              out.push(...(await walkDirectory(entry, `${prefix}/${entry.name}`)))
            } else {
              const file = await fileFromEntry(entry)
              if (file) out.push(withRelativePath(file, prefix))
            }
          }
          readBatch()
        },
        () => resolve(out)
      )
    }
    readBatch()
  })
}

function fileFromEntry(entry) {
  return new Promise((resolve) => {
    entry.file(
      (file) => resolve(file),
      () => resolve(null)
    )
  })
}

/**
 * `webkitRelativePath` is read-only on a File, so it is defined on the instance.
 * A drop gives no path at all; without it every file from a folder would be
 * filed flat into the destination, which is the exact failure the tree logic in
 * uploadFiles exists to prevent. If an engine refuses the override the upload
 * still happens, only without the folder structure.
 */
function withRelativePath(file, prefix) {
  const relative = prefix ? `${prefix}/${file.name}` : ''
  try {
    Object.defineProperty(file, 'webkitRelativePath', {
      value: relative,
      configurable: true,
    })
  } catch {
    /* the pick still uploads; only the structure is lost */
  }
  return file
}
