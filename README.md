# Yuarcloud

Self-hosted cloud storage. Upload, organise, preview and share files from the
browser.

- Chunked upload with resume — large files survive a dropped connection
- Share links with password, expiry date and a view limit
- Per-user accounts, quota, and roles
- Audit log of who did what, kept 180 days
- Dark mode

## First sign-in

```
username: yuarcloud
password: yuarcloud
```

On first sign-in the server asks for a new password before it will show you
anything else — that is deliberate, since the password above is published in
this repository. Change it before putting the port on the internet.

After that, **Settings** in the account menu is where an administrator edits a
user's username, password, role and quota.

## Run it

### Docker

```bash
cp .env.example .env
sed -i '' "s|^YUAR_AUTH_JWT_SECRET=.*|YUAR_AUTH_JWT_SECRET=$(openssl rand -base64 48)|" .env
docker compose up -d --build
```

Open <http://localhost:3103>.

`YUAR_AUTH_JWT_SECRET` is required — it signs your login tokens, and the server
will not start without one. Generating your own is the only step that matters
here; the file's default value is a placeholder, not a usable key.

### ZimaOS and other immutable-root NAS

`docker-compose.zimaos.yml` is ready to paste into **Dashboard → Install a
customized app → Import → Docker Compose**. It pulls the published image and
mounts under `/DATA/AppData/`, which is where ZimaOS keeps app data.

It differs from the main compose because the importer has no `.env` to read and
no source tree to build from, so nothing is substituted and nothing is compiled.
Replace the placeholder secret in it before installing.

### Without Docker

```bash
make init          # create configs/config.yaml and the data directories
make install       # go mod download + npm install
make dev-backend   # API on :3103
make dev-frontend  # Vite dev server, proxies /api
```

## Configuration

`configs/config.yaml` drives a bare-metal install; `configs/config.docker.yaml`
is the one baked into the image and is the same file with absolute `/srv` and
`/config` paths, because a relative path inside the container would resolve
against the config's own directory and put your files on the state volume.

Any setting can be overridden by environment variable — dotted keys become
underscored ones:

```
storage.local.root   ->   YUAR_STORAGE_LOCAL_ROOT
auth.jwt_secret      ->   YUAR_AUTH_JWT_SECRET
```

`.env.example` lists all of them.

### Volumes

| Path | Holds |
|---|---|
| `/srv` | Your files. This is the one to back up. |
| `/config` | `config.yaml` and the SQLite database. Small, must survive. |

SQLite runs in write-ahead logging mode, which is what lets several people
upload at once without the database locking up. While the server is *running*
that means recent writes sit in a sibling `y.db-wal` file, so a copy of `y.db`
on its own can be a little behind. Two ways to be safe:

```
docker compose stop yuarcloud && cp …/y.db …      # stop first — one file is enough
cp …/y.db* …                                      # or copy y.db, y.db-wal and y.db-shm
```

A cleanly stopped server checkpoints on the way out and leaves `y.db` complete
on its own. If the host is NFS or SMB, WAL is skipped automatically and this
does not apply.

Chunked-upload scratch lives at `<storage root>/.yuarcloud-chunks`, on the same
volume as the files it will become, so a large upload never fills the
container's own writable layer.

Logs go to stderr — `docker logs yuarcloud`. There is no log file.

### Custom storage location

`/srv` is only the default. The simplest way to use another folder is to point
the `/srv` bind mount at it — the container path stays `/srv`, and nothing else
changes:

```yaml
    volumes:
      - type: bind
        source: /DATA/Media/Yuarcloud   # any absolute path on the host
        target: /srv
```

If the folder has to live inside a larger mount, mount that instead and name
the root explicitly. The entrypoint fixes ownership of whichever directory the
environment names — a root set only inside `/config/config.yaml` is invisible
to it, so use `YUAR_STORAGE_LOCAL_ROOT` in the compose file (or chown the
folder yourself):

```yaml
    environment:
      - YUAR_STORAGE_LOCAL_ROOT=/library
    volumes:
      - type: bind
        source: /DATA/Media
        target: /library
```

Pointing it at an existing library works like FileBrowser: when a directory is
opened, files and folders already on disk are adopted into the database on the
spot, so they appear in the listing and can be shared, moved, downloaded or
moved to the trash. Nothing on disk is rewritten by that first listing.

Things to know:

- **Adoption is lazy, one directory level at a time.** Opening a folder adopts
  its direct children; opening a child adopts the next level. This avoids a
  full-disk scan on first start. Search and the move/copy folder tree only know
  items that have been adopted, so a deep item can be missing from those views
  until its folder has been browsed once.
- **Adopted bytes do not count toward the account quota.** They were already
  there before the account existed, and charging them retroactively could lock
  the account out of uploading. New uploads, copies and share uploads are
  charged normally. Deleting an adopted file therefore does not change
  `used_bytes`.
- **Symbolic links are skipped.** Following a link could leave the storage root
  and expose arbitrary host files, so the importer never turns one into a
  browsable entry. Hard links and ordinary files are unaffected.
- The entrypoint chowns the named root and the chunk directory, but **not
  recursively**. A folder that already contains another app's files keeps
  their ownership. Reading usually works, but moving or deleting a nested item
  needs write permission on its parent directory for uid 1000. If those
  operations fail with "permission denied", fix the host permissions or run on
  a dedicated folder.
- The database is still the index. A file dropped into an already-open folder
  appears on the next refresh; a file edited outside the app has its size and
  MIME type refreshed the next time its folder is listed.

The scratch directory follows the root (`<root>/.yuarcloud-chunks`) when
`storage.chunked.temp_dir` is empty, which is what the Docker image ships. Set
`YUAR_STORAGE_CHUNKED_TEMP_DIR` only if it needs to live somewhere else.

## Layout

```
cmd/server/       entry point, routing
internal/         auth, config, database, handler, middleware,
                  model, scope, share, storage
web/src/          React frontend
configs/          config.yaml.example (bare metal), config.docker.yaml (image)
```

## Development

```bash
make test         # Go tests
make lint         # gofmt + go vet
cd web && npm run lint && npm run build
```

## License

MIT — see [LICENSE](LICENSE).
