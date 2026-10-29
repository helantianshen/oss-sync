# OSS Sync

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/webui/assets/oss-logo-light.png">
    <img src="internal/webui/assets/oss-logo.png" alt="OSS Sync" width="240" height="95">
  </picture>
</p>

> Self-hosted Obsidian sync and share: notes, attachments, collaboration, and a public blog, deployed from a single binary.

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![Node](https://img.shields.io/badge/Node-20-339933?logo=node.js)](https://nodejs.org)
[![Obsidian](https://img.shields.io/badge/Obsidian-1.4+-7C3AED)](https://obsidian.md)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

English | [中文](./README_zh.md)

Data is served to the Obsidian client plugin: search for **OSS Sync and Share** in the Obsidian community plugin marketplace, or build it from source as described in *Build the plugin* below.

If you run into problems, please open an [issue](https://github.com/helantianshen/oss-sync/issues).

---

## Core features

- **Vault isolation**: one account can own multiple Vaults; sync revisions, file paths, members, and permissions are fully independent.
  - Supports Vault members (manager / participant), with Vault authorization granted per device.
- **Multi-device sync**: create, modify, delete, rename, and move folders.
  - Changes are distributed in real time; short polling and long polling switch by Vault policy or user preference.
- **Offline-first with a durable queue**:
  - Local edits are written to the `.oss-sync-state.json` queue before transfer.
  - After closing and reopening Obsidian, queued work resumes automatically instead of being silently lost.
- **Conflict handling**:
  - Revision-based CAS detection with keep local, keep remote, keep both, and ordered merge.
  - Markdown supports three-way merge; attachments and other binaries keep a copy of each side.
- **Device management**:
  - Every client is identified by a stable `client_id` with states pending / approved / revoked.
  - First launch asks for a device name; device approval and Vault authorization are separate on the server, so a device can be approved before any Vault exists.
- **Attachment and config sync**:
  - Images, PDFs, and other non-note files are supported; `.obsidian` config sync is off by default.
  - The web console previews Markdown, images, PDFs, HTML, and SVG; HTML and SVG run inside a script-disabled sandbox.
  - Text files can be edited in the web console without a server plugin. Saves use the sync write path and unchanged content does not create a new revision.
- **Recycle bin and file history**:
  - Deletions go to the recycle bin with restore, remaining retention time, and retention-based cleanup.
  - History supports version browsing, line diff, and restore to any version.
- **Sharing and public blog**:
  - Public links for a single note or a folder, with an allow-copy toggle.
  - Built-in `default` for shared reading and `papertrail` for public blog homepages, selected per Vault.
- **Markdown collaboration**:
  - Invite, accept, and revoke collaborators; real-time over SSE with long-polling fallback.
- **Server plugin extensions**:
  - WASM compatible, plus administrator-trusted executable plugins.
  - Plugins can register routes, hooks, middleware, admin pages, cron tasks, database migrations, dependencies, and host RPC.
  - Admin page HTML fragments automatically use the console shell and active theme; complete HTML documents remain plugin-controlled.
  - The public Go SDK includes host file read/write methods; plugin writes participate in sync revisions and file history.
- **Data and deployment**:
  - SQLite by default, PostgreSQL optional, with periodic storage reconciliation.
  - One script installs the binary and registers systemd; Docker remains available.

---

## Architecture

```
cmd/server        HTTP entry
configs/          dev / prod YAML
internal/
  auth            register, login, JWT, device auth
  syncapi         Vault revisions, upload/download, rename/delete
  vaults          Vault CRUD, members, settings
  devices         device state, Vault authorization, cursor
  collaboration   invite, accept, content write, events
  history/recycle snapshots, restore, retention
  blog            blog themes and public pages
  serverplugin    WASM and executable plugin runtime
  webui           web console and admin
pkg/ossplugin     public Go SDK
plugin/src        Obsidian plugin
```

Sync uses HTTP only. Short polling `wait=0` or long polling `wait=30` runs per Vault. Collaboration pushes per account: SSE over HTTPS (`app://obsidian.md` allowed via CORS), long polling over plain LAN HTTP.

---

## Quick start

The one-command script is recommended; Docker and manual binaries are also supported.

### Method 1: one-command script (recommended)

Downloads the official binary for the current architecture, verifies its SHA-256 and version, and registers `oss-sync.service`:

```bash
curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/oss.sh | sudo bash
```

Requirements: a running systemd, root, Python 3, curl, coreutils, util-linux (`flock`), and `useradd`/`getent`. The script does not install Docker, Go, or other dependencies. Unsupported architectures exit with guidance to build manually, and download or checksum failures never fall back to compiling from source.

Script behavior:

- Asks for the deployment directory, port (default `8080`), storage limit in GiB (`0` means unlimited), and Release source (accelerated by default, GitHub direct, or a custom HTTPS prefix).
- Installs to `/opt/oss-sync` by default; override with `OSS_INSTALL_DIR`.
- Creates the global commands `oss` and `oss-sync` in `/usr/local/bin`.
- Runs the service as a dedicated `oss-sync` system account with no extra Linux capabilities.

After installation, run `sudo oss` to open the management menu:

```text
┌──────────────────────────┐
│ OSS Sync 0.1.22          │
│ 状态：运行中             │
│ 地址：http://0.0.0.0:8080 │
│ 存储：000 KB / 不限      │
└──────────────────────────┘

1 更新    Update       pick a source: accelerated / official / custom / 0 back
2 停止    Stop         shows Start when the service is stopped
3 重启    Restart
4 修改    Modify       1 storage limit, 2 port, 0 back
5 日志    Logs
6 卸载    Uninstall    1 everything including data, 2 keep data, 0 back
0 退出    Exit
```

Every submenu supports `0` to return to the main menu. Numbers can also be passed directly:

```bash
sudo oss 1        # update
sudo oss 2        # start or stop
sudo oss 3        # restart
sudo oss 4        # modify storage limit or port
sudo oss 5        # follow logs
sudo oss status   # detailed systemd status
```

Updates verify all artifacts before stopping the service, replace the binary, and check `/readyz` plus the expected version. On failure the previous program and configuration are restored, but database migrations are not reversed, so back up before upgrading.

Non-interactive installs accept `OSS_PORT`, `OSS_STORAGE_LIMIT_GB`, `OSS_INSTALL_DIR`, `OSS_RELEASE_PROXY=official` (or an HTTPS prefix), and `OSS_VERSION` for an exact tag. See [binary and systemd deployment](docs/deployment.md).

### Method 2: Docker

Docker remains for source development and existing deployments; it is no longer used by the one-command installer:

```bash
docker compose up -d --build
docker compose logs -f backend
```

Compose exposes port `8080` and stores data in the `oss-data` named volume. Update this deployment by rebuilding or replacing the image. Do not run the binary installer over an existing Docker data directory; follow the migration guide. `docker compose down -v` deletes the data volume.

### Method 3: manual binary

Download the runtime package for your platform from [Releases](https://github.com/helantianshen/oss-sync/releases), extract it, and run from that directory:

```bash
./bin/oss-server          # Linux / macOS
./bin/oss-server.exe      # Windows
```

Set `OSS_ENV=prod` to load the bundled production config; data and SQLite resolve relative to the current directory.

### Running the backend from source

For development:

```bash
go run ./cmd/server
```

Listens on `http://localhost:8080` with data in `data/`. Health:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

Use `OSS_ENV=dev|prod` to select the config file, overridable with `OSS_SERVER_HOST`, `OSS_SERVER_PORT`, `OSS_DB_DRIVER`, `OSS_DB_DSN`, `OSS_STORAGE_DIR`, and others. For PostgreSQL:

```bash
export OSS_DB_DRIVER=postgres
export OSS_DB_DSN='postgres://user:pass@127.0.0.1:5432/oss?sslmode=disable'
go run ./cmd/server
```

---

## Usage guide

1. **Register the admin**: open `http://{server-ip}:8080`; the first registered user becomes admin. Disable registration afterwards in the admin console.
2. **Install the plugin**: install **OSS Sync and Share** from the Obsidian marketplace, or build it and copy to `<vault>/.obsidian/plugins/oss-sync/`.
3. **Set the device name**: the first time the plugin settings open, enter a device name and save it; the login form appears afterwards.
4. **Sign in**: enter a server URL that includes `http://` or `https://`, plus username and password. A missing protocol produces a specific error instead of a generic login failure.
5. **Approve the device**: approve it in the web console. Device approval and Vault authorization are separate, so approval works before any Vault exists.
6. **Bind a Vault**: pick an existing Vault in the plugin settings, or create one and run a full sync immediately. The settings page refreshes the authorized Vault list every three seconds while open.
7. **Sync**: local edits are queued durably and uploaded; changes from other devices are downloaded on the next poll.

The plugin maintains `.oss-sync-state.json` (v3) at the Vault root for baselines, the pending queue, and conflicts. It is never uploaded.

---

## Plugins, blog templates, and console themes

Plugins are the only installable extension package. They own functionality, settings, routes, hooks, data, admin pages, tasks, and integrations. A plugin may also declare blog templates and console themes in `manifest.json`.

Built-in blog templates `default` and `papertrail`, plus the built-in console theme `default`, remain visible in their selectors and cannot be deleted. When an enabled plugin declares resources, its display names are appended to the corresponding selectors. Templates and console themes are not shown as a separate plugin list and no longer have independent upload or management pages.

### Create a plugin

1. Copy [`examples/server-plugin-echo`](examples/server-plugin-echo) and change the plugin ID and handlers.
2. Build the executable next to `manifest.json`.
3. Add optional `blog_themes` and `console_themes` entries, with package directories containing `template.html` or `theme.css`.
4. Package the plugin and upload it from **Admin settings → Plugins**.

```powershell
cd examples/server-plugin-echo
go build -o plugin.exe .
Compress-Archive manifest.json,plugin.exe my-plugin.zip
```

Include declared resource directories in the ZIP, for example `Compress-Archive manifest.json,plugin.exe,blog,console my-plugin.zip` (only include existing directories). See the [blog template contract](docs/server-plugins.md#blog-template-contract) for fields, page branches, attachment paths and diagnostics.

Resource example:

```json
{
  "blog_themes": [{"id":"clean","name":"Clean reading","path":"blog/clean"}],
  "console_themes": [{"id":"clean","name":"Clean console","path":"console/clean"}]
}
```

The server runs precompiled WASM or executable plugin payloads directly. Online editing in the plugin page is limited to validated text resources; binary entrypoints are read-only. AdminPage fragments inherit the console layout and active theme; a complete HTML document remains plugin-controlled. See the [server plugin guide](docs/server-plugins.md) and the console's embedded **Plugin guide** for package layout, callbacks, authorization, host services, packaging, testing, and AI-assisted development.

---

## Configuration

| Env | Description |
|---|---|
| `OSS_ENV` | `dev` or `prod` |
| `OSS_SERVER_HOST` / `PORT` | listen address and port |
| `OSS_DB_DRIVER` / `DSN` | sqlite or postgres |
| `OSS_STORAGE_DIR` | file storage root |
| `OSS_ALLOW_ANONYMOUS_REGISTRATION` | initial registration switch |
| `OSS_WEB_SESSION_TTL_HOURS` | web session lifetime in hours; default `24` |
| `OSS_DEVICE_JWT_TTL_HOURS` | plugin device token lifetime in hours; default `720` |
| `OSS_DEVICE_STALE_DAYS` | stale device threshold |
| `OSS_RECONCILE_INTERVAL_HOURS` | storage reconciliation interval |
| `OSS_UPDATE_DOWNLOAD_SOURCE` | update source: `official`, `proxy`, or `custom` |
| `OSS_UPDATE_DOWNLOAD_PROXY` | HTTPS prefix used when the source is `custom` |

Vault settings (admin can force): `sync_mode` (`user_choice` / `short_poll` / `long_poll`), recycle bin days, storage quota, and upload size limit.

`download_source` and `download_proxy` live in the update section of `configs/config.dev.yaml` or `configs/config.prod.yaml`. The admin update panel can override them per check. The selected address is used for both release metadata and the binary download, so updates still work when the server cannot reach GitHub directly.

---

## Development

```bash
# backend
go build ./...
go vet ./...

# plugin
cd plugin
npm ci
npm exec tsc -- --noEmit
npm run lint
npm test
npm run build
```

Conventions: `gofumpt` + `golangci-lint` for Go, strict TypeScript, no emoji in the UI, styling through `console.css` tokens, and no inline styles.

---

## Deployment notes

- Put a reverse proxy with HTTPS in front of the Go service.
- Back up `data/` (SQLite file or Postgres dump) and the JWT secret stored in the database.
- Disable open registration after the initial users are created.
- Monitor `/readyz`; alert on non-200 or repeated reconciliation failures.
- An Nginx reverse-proxy example is available at [scripts/https-nginx-example.conf](scripts/https-nginx-example.conf).

---

## Security

- Passwords are bcrypt-hashed and never logged.
- JWT is HS256 with a per-deployment random secret stored in the database.
- Web sessions use 24-hour HttpOnly Secure SameSite cookies with CSRF validation; the plugin uses a 30-day device-bound Bearer JWT that is removed locally on expiry and requires a new login.
- All mutating web requests require CSRF; all sync and collaboration requests require an approved device plus Vault authorization.
- Server plugins accept WASM packages or administrator-trusted executable packages. WASM modules receive no WASI, filesystem, network, database, or environment access. Executable packages can read and write server files, access the database, use the network, read environment variables, and run system commands, with the same permissions as the server account. Plugins are uploaded only by administrators, so install only reviewed code.
- Plugins may declare host-rendered settings but cannot inject HTML or JavaScript.

---

## License

MIT, see [LICENSE](LICENSE).
