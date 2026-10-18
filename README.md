# Yuarcloud

**Modern, self-hosted cloud storage** — a lightweight alternative to Nextcloud/ownCloud, with its own visual language rather than a theme applied to someone else's.

```
┌─────────────────────────────────────────────────────────────┐
│  Yuarcloud                                                 │
│  Go 1.23 · Gin · SQLite · React 18 · Vite · Chunked Upload │
└─────────────────────────────────────────────────────────────┘
```

## Features

| | |
|---|---|
| 📁 **File ops** | copy · paste · cut · rename · delete · move · download |
| 👁️ **File viewer** | Inline preview for image · video · audio · PDF · text/code · xlsx/csv · docx — with Range streaming so video and music can be scrubbed |
| 🗂️ **Views** | Grid & List toggle (persisted) · sort by name/size/date |
| ⬆️ **Upload** | File upload · Folder upload · Drag & drop · Clipboard paste |
| ✂️ **Chunked upload** | Resumable chunks + SHA-256 verify — bypasses Cloudflare Tunnel's 100MB request limit |
| 🔗 **Share links** | Optional password · expiry in days · max view count · activate/deactivate |
| 👤 **Auth** | JWT access + refresh tokens · bcrypt (argon2-ready) · admin-provisioned accounts |
| 🛡️ **Admin** | User CRUD · per-user quota · roles · chunk-size config · audit log |
| 🌗 **Dark mode** | Follows `prefers-color-scheme` |

## Quick Start

### Docker (recommended)

```bash
cp .env.example .env

# Required. The server refuses to start without it.
#   YUAR_AUTH_JWT_SECRET is the name — dotted config keys map to underscores.
sed -i '' "s|^YUAR_AUTH_JWT_SECRET=.*|YUAR_AUTH_JWT_SECRET=$(openssl rand -base64 48)|" .env

docker compose up -d --build
```

Open <http://localhost:3103> — default login **`admin` / `admin`**.

> Change the admin password immediately after first login (Settings → Security).

#### Volumes

Two volumes, deliberately not the same thing:

| Container path | Holds | Why separate |
|---|---|---|
| `/srv` | User files | This is the only thing most people back up |
| `/config` | `config.yaml` + SQLite database | App state; small, and must survive |

Backing up `/config` alone gives you a complete restore (files + accounts) without
scanning through millions of files. Separating them also means `docker compose down -v`
can discard state without touching uploads.

Chunked-upload scratch lives at `/srv/.yuarcloud-chunks` — on the same volume as the
files it will become, so a large upload never fills the container's own writable layer.
The leading dot keeps it invisible in the browser: `LocalStorage.List` skips every
dot-prefixed entry, the same rule that hides `.yuarcloud-trash`.

Host paths are variables. For a bind mount instead of a named volume:

```bash
# .env
YUAR_SRV=/mnt/nas/yuarcloud
YUAR_CONFIG=/mnt/nas/yuarcloud-config
```

Any setting can be overridden per install without rebuilding — dotted config keys
become underscored env vars:

```bash
YUAR_STORAGE_LOCAL_ROOT=/srv        # storage.local.root
YUAR_DATABASE_SQLITE_PATH=/config/yuarcloud.db
YUAR_SERVER_PORT=3103
```

Logs go to stderr; read them with `docker logs yuarcloud`. There is no log file
and no log volume.

### ZimaOS (and other immutable-root NAS)

ZimaOS is Debian with an immutable `/` and app data under `/DATA`. Docker itself
is standard — the restrictions are in the web UI, not the engine.

`docker-compose.zimaos.yml` is ready to paste: Dashboard → **Install a customized
app** → **Import** → **Docker Compose**. It differs from the main compose in three
ways ZimaOS forces: no `${VARIABLE}` substitution (the importer has no `.env` to
read), bind mounts under `/DATA/AppData/` instead of named volumes, and an
`image:` reference rather than `build: .` — there is no source tree on the box
when the importer runs.

**No SSH, and no manual chown**, which is the part that previously made this
impossible from the dashboard. The image's entrypoint hands the volumes to
uid 1000 on startup, and ZimaOS's file manager cannot chown — so asking for it
would have closed the door on the only no-SSH route.

#### The image it pulls

One is published, so the importer has something to reference:

```
ghcr.io/yugaardiyan/yuarcloud:latest
```

Public pull, no login. Paste the compose, replace the JWT secret, install. Open
<http://<zima-box>:3103> and sign in as **`admin` / `admin`**, then change the
password immediately.

The `image:` line in `docker-compose.zimaos.yml` already points there. Change it
if you forked — the workflow publishes whatever repository it runs in.

It is `latest` because the ZimaOS importer is a paste box, not a version picker.
If you would rather pin, use the newest release tag —
`ghcr.io/yugaardiyan/yuarcloud:v1.0` at the time of writing — and each `v*` tag
keeps its own copy.

Check what a tag actually holds before pinning to it:

```bash
curl -s "https://ghcr.io/token?scope=repository:yugaardiyan/yuarcloud:pull&service=ghcr.io" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])" > /tmp/tok
curl -s -H "Authorization: Bearer $(cat /tmp/tok)" \
  https://ghcr.io/v2/yugaardiyan/yuarcloud/tags/list
```

A higher number is not a newer build. `v1.2` was published before the repository
was emptied and redeployed; it predates the chunked-upload data-loss fix and the
bundle work. `v1.0` is the tag that carries the current code.

#### Publishing your own

Tag a release and GitHub Actions builds a multi-arch image to ghcr.io under your
own repository:

```bash
git tag v1.1 && git push origin v1.1
```

Multi-arch is not optional: a ZimaOS box is usually x86_64 while the machine
publishing is usually arm64, and a single-arch tag builds fine then fails on the
box with `exec format error`, which reads like a corrupt image rather than a
mismatch.

To try it without publishing anything, build on the box over SSH instead and
point `image:` at `yuarcloud:local`.

Three ZimaOS-specific things worth knowing:

- **The web UI does not preserve volume flags.** `:ro`, `:shared` and friends are
  stripped or rewritten by the GUI even though Docker honours them over SSH. This
  compose uses plain bind mounts, so nothing is lost, but manage anything more
  exotic from a Compose file over SSH.
- **`/` cannot be bind-mounted.** `/DATA` and mounted drives only. This is the
  reason the volumes are not `/app` or `/opt`.
- **Named volumes are a poor fit for backups.** They live inside Docker's own
  directory. `/DATA/AppData/yuarcloud/srv` can be handed straight to rsync,
  Syncthing or a snapshot job; that is the whole point of choosing binds here.

### Local development

Requires **Go 1.23+** and **Node 20+**.

```bash
make init          # create configs/config.yaml + data dirs
make install       # go mod download + npm install
make dev-backend   # API on :3103
make dev-frontend  # Vite dev server on :3000 (proxies /api)
```

### Production build

```bash
make build          # builds bin/yuarcloud + web/dist
./bin/yuarcloud configs/config.yaml
```

## Cloudflare Tunnel

Chunked upload keeps each request well under Tunnel's 100MB ceiling, so large files work
over a Tunnel without extra config.

```yaml
# cloudflared tunnel config
ingress:
  - hostname: files.example.com
    service: http://localhost:3103
    originRequest:
      chunkedEncoding: true
```

Tune `storage.chunked.max_chunk_size` in the admin panel (**Admin → Chunked Upload**).
These three settings apply immediately and are re-applied on restart, so the YAML
values below are only the first-boot seeds:
- **10 MB** — Cloudflare Tunnel (recommended)
- **50 MB** — direct/VPN access

There is deliberately no concurrent-upload cap. On a self-hosted box the real
ceilings are disk and bandwidth, both of which the operator already controls at
the host level, and a global counter would let one user's uploads lock out
everyone else's. Per-request and per-file limits are enforced; per-user fairness
is what the reverse proxy or `rate_limit` middleware is for.

## Configuration

All settings support env override with the `YUAR_` prefix, e.g. `YUAR_SERVER_PORT=9000`.

| Key | Default | Description |
|---|---|---|
| `server.host` / `server.port` | `0.0.0.0` / `3103` | Listen address |
| `storage.local.root` | `./data/files` | File storage root |
| `storage.chunked.max_chunk_size` | `10485760` (10MB) | Max bytes per chunk request |
| `storage.chunked.max_file_size` | `10737418240` (10GB) | Max upload size; `0` = no ceiling |
| `storage.chunked.cleanup_interval` | `1h` | Silence timeout before a stalled upload's chunks are reclaimed |
| `auth.jwt_secret` | — | **Change this** (32+ chars) |
| `auth.token_ttl` / `auth.refresh_ttl` | `24h` / `168h` | Token lifetimes |
| `share.default_expiry_days` | `7` | Default share expiry |
| `share.max_expiry_days` | `365` | Max allowed expiry |
| `share.require_password` | `false` | Force password on all shares |
| `rate_limit.requests_per_minute` | `120` | Per-IP rate limit |

## API

### Auth
```
POST   /api/auth/login              { username, password }
POST   /api/auth/refresh            { refresh_token }
GET    /api/auth/me
POST   /api/auth/change-password
```

### Files
```
GET    /api/files?path=/&parent_id=
GET    /api/files/:id
POST   /api/files/folder            { name, parent_id }
POST   /api/files/upload            multipart: file, parent_id
GET    /api/files/:id/download
GET    /api/files/:id/preview       (also accepts a preview token, see below)
POST   /api/files/:id/preview-token
DELETE /api/files/:id
PATCH  /api/files/:id/rename        { name }
POST   /api/files/:id/move          { target_parent_id }
POST   /api/files/:id/copy          { target_parent_id }
```

### Preview

`/preview` streams a file with `Content-Disposition: inline` and honours HTTP
Range requests (`Accept-Ranges: bytes`, `206 Partial Content`). Range support
is what makes seeking work in `<video>`/`<audio>`, and it keeps a large file
from being pulled into memory before playback starts.

| Format | Rendered by |
|---|---|
| jpg, png, gif, webp, avif, … | `<img>`, with pan and zoom |
| mp4, webm, mov, mkv, … | `<video controls>` |
| mp3, wav, ogg, opus, flac, m4a, … | `<audio controls>` |
| pdf | the browser's built-in viewer |
| txt, log, md, json, yaml, js, ts, py, go, … | `<pre>`, wrap toggle, binary sniff guard |
| xlsx, xls*, csv, tsv | SheetJS (lazy-loaded) → table with sheet tabs |
| docx | mammoth (lazy-loaded) → HTML |
| anything else | a short explanation plus a download button |

\* Legacy `.xls` / `.doc` / `.ppt` are the pre-2007 binary formats. No
browser can render them, so they are deliberately routed to the download
fallback rather than to a parser that would fail.

SheetJS and mammoth are `import()`-ed on first use, so they cost nothing until
someone opens a spreadsheet or a Word document.

**Paging is scoped by media type.** The arrow keys and the drag gesture move
within one series, and the title bar names the one you are in:

| Series | Contents |
|---|---|
| **Media** | images **and** video — a folder of holiday photos and the film shot on that trip pages through as one reel |
| **Music** | tracks |
| **Documents** | PDF, docx, xlsx, csv, txt — everything you read |

Audio is deliberately kept apart: a track has its own transport and a picture
has not, so stepping between them is a jarring change of context. A folder of
mixed content no longer swaps the whole view on most presses. The arrows live
in the header rather than floating over the body, so they cannot cover a video
frame; they disappear when a series has only one member, and at either end the
arrow with nowhere to go is shown disabled rather than silently inert.

Images and video can also be **dragged sideways** to page, in the same
direction as a phone photo gallery: drag left for the next, drag right for
the previous. A zoomed-in image is the exception — there, dragging pans the
image instead, because it is larger than the viewport and there is something
off-screen to reach. On video the gesture yields to the native controls: the
control bar and the play/pause toggle are left alone, so nothing the browser
owns gets in the way.

**Why a token is in the URL, and which one.** `<img>`, `<video>`, `<audio>`
and `<iframe>` cannot set an `Authorization` header, so a media player has to
be handed a plain URL. Putting the *session* token there would be reckless —
it is valid for every endpoint and would land in every access log. Instead:

```
POST /api/files/:id/preview-token        (normal Authorization header)
  -> { token, expires_in: 300 }
GET  /api/files/:id/preview?token=...    (header optional)
```

The returned token is a separate JWT whose `purpose` claim is `preview`. It
is **pinned to that one file** and lives **five minutes**, and the API rejects
it everywhere:

| Check | Result |
|---|---|
| preview token on its own file | 200 |
| preview token on a *different* file | 401 |
| preview token on `/api/files`, `/download`, `/admin/*` | 401 |
| session access token as `?token=` | 401 |
| preview URL with no credential | 401 |

`ValidateToken` refuses any token whose purpose is `preview`, which is what
stops a leaked preview URL from becoming an account compromise. Shared links
need none of this — the share token in the path is already the credential.

### Chunked upload
```
POST   /api/files/upload/chunked/init
          { filename, total_size, chunk_size, total_chunks, checksum, parent_id }
POST   /api/files/upload/chunked/:uploadId/chunk/:chunkIndex
          multipart: chunk
POST   /api/files/upload/chunked/:uploadId/complete
GET    /api/files/upload/chunked/:uploadId/status
DELETE /api/files/upload/chunked/:uploadId
```

### Shares
```
POST   /api/shares                  { file_id, password?, expiry_days?, max_views? }
GET    /api/shares
DELETE /api/shares/:id
PATCH  /api/shares/:id/toggle       { active }

GET    /api/shares/:token           public (password via ?password=)
GET    /api/shares/:token/download  public
```

### Admin
```
GET    /api/admin/stats
GET    /api/admin/users             ?page=&search=&role=
POST   /api/admin/users
PATCH  /api/admin/users/:id
DELETE /api/admin/users/:id
PATCH  /api/admin/users/:id/active  { active }
PATCH  /api/admin/users/:id/quota   { quota_bytes }
PATCH  /api/admin/users/:id/role    { role }
GET    /api/admin/settings
POST   /api/admin/settings
GET    /api/admin/audit-logs
GET    /api/admin/chunked-config
POST   /api/admin/chunked-config
```

## Project structure

```
yuarcloud/
├── cmd/server/main.go              # entrypoint, routes, graceful shutdown
├── internal/
│   ├── config/                     # viper config + env override
│   ├── database/                   # gorm init, migrations, admin seed
│   ├── model/                      # User, File, ShareLink, UploadSession, AuditLog
│   ├── auth/                       # JWT pair, bcrypt
│   ├── middleware/                 # auth, admin-only, rate limit
│   ├── storage/                    # StorageBackend iface + local impl + chunks
│   ├── share/                      # share token service
│   ├── admin/                      # user mgmt, stats, audit
│   └── handler/                    # HTTP handlers
├── web/                            # React + Vite frontend
│   └── src/{components,context,pages,utils,styles}
├── configs/config.yaml.example     # bare-metal: relative paths
├── configs/config.docker.yaml      # container: absolute /srv + /config
├── Dockerfile                      # multi-stage: node → go → alpine
├── docker-entrypoint.sh            # hands /srv + /config to uid 1000, then drops
├── docker-compose.yml              # two volumes: /srv and /config
├── docker-compose.zimaos.yml       # immutable-root NAS: binds under /DATA
└── .github/workflows/              # builds and publishes the image to ghcr.io
```

## Architecture

```
Browser ──JWT──► Gin Router ──► Middleware ──► Handlers ──► Services ──► Storage
                  │                │                │            │
            /api/*  routes   auth · admin ·   File · Share   local FS +
                              ratelimit        · Admin       chunks + SQLite
```

Chunked upload flow:

```
1. POST /init            → uploadId, chunkSize, totalChunks
2. POST /:id/chunk/:i    × N   (each ≤ max_chunk_size)
3. POST /:id/complete    → concatenate → SHA-256 verify → atomic move → cleanup temp
                            ↓
                     expired sessions purged by cleanupRoutine
```

## Design

Yuarcloud's UI is its own, and the decisions behind it are worth writing down
because they are not derivable from the markup.

**One card, one pane.** The file area is a single rounded surface inset from the
window, not rows floating on the page grey. The insets are deliberately
asymmetric — more air at the top, almost none underneath — because a card with
even margins reads as parked in the middle rather than filling the space it has.

**One pill.** Every primary action lives in a single capsule. Search expands it
and the icon row collapses away, so the field appears to fill the pill rather
than open beside it. This is why there is no second toolbar row.

**Floating surfaces are seated, not parked.** The upload tray and toasts sit
flush against the card's bottom-right corner, on the card's own edges and radius,
so they read as part of the card rather than as panels hovering near it. The
three numbers that place them are CSS variables derived from the card's own
margins, so moving the card moves them.

**Shape is chosen, not inherited.** Dialog buttons are capsules because a dialog
is a temporary surface that needs to separate itself from what is behind it;
buttons in the app proper stay rectangular, because the toolbar is already one
pill and a second inside it reads as a control inside a control. Notifications
are 16px-radius cards rather than capsules — a capsule's radius is its own
height, so the shape would change every time a message wrapped to two lines.

**Colour carries meaning, not decoration.** Icons carry the file type and the
accent carries state: green for a finished upload, red for a failure, amber for
quota past warning. An error notification needs no red text to be understood,
because its icon already says so.

**Darkness is restrained.** The page grey, the white card and one faint border
are the whole structural palette. Depth comes from a single soft shadow rather
than a stack of grey steps.

## Security notes

- Set a strong `auth.jwt_secret` before deploying.
- Change the default admin password.
- Put Yuarcloud behind HTTPS (Cloudflare Tunnel terminates TLS).
- Mount `data/` on encrypted storage at rest if needed.
- There is no sign-up endpoint. Accounts are created only by an admin from
  **Settings → Users**, so access is fully closed by default.

## License

MIT
