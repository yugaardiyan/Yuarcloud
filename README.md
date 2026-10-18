# Yuarcloud

Self-hosted cloud storage. Upload, organise, preview and share files from the
browser.

- Chunked upload with resume — large files survive a dropped connection
- Share links with password, expiry date and a view limit
- Per-user accounts, quota, and roles
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

Chunked-upload scratch lives at `/srv/.yuarcloud-chunks`, on the same volume as
the files it will become, so a large upload never fills the container's own
writable layer.

Logs go to stderr — `docker logs yuarcloud`. There is no log file.

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
