# OSS Sync

> Self-hosted Obsidian sync & share — Markdown, attachments and collaboration in one binary.

[![Go](https://img.shields.io/badge/Go-1.25-%2300ADD8?logo=go)](https://go.dev)
[![Node](https://img.shields.io/badge/Node-20-%23339933?logo=node.js)](https://nodejs.org)
[![Obsidian](https://img.shields.io/badge/Obsidian-1.4+-7C3AED)](https://obsidian.md)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[English](#) | [中文](./README_zh.md)

## Overview

OSS Sync is a self-hosted alternative to Obsidian Sync. It consists of a Go (Gin) backend and a TypeScript Obsidian plugin. Data stays on your own server: files, versions, shares and collaboration are all managed by you.

- **Vault-based**: one account can own multiple Vaults.
- **Device-aware**: each Obsidian client has a stable `client_id` with pending / approved / revoked states.
- **Offline-first**: local edits are queued, merged with three-way merge, and synced with revision-based CAS.

## Features

- Markdown, attachments and optional `.obsidian` config sync
- Create / modify / delete / rename with full and incremental manifest checks
- Revision-based conflict detection with “keep local / keep remote / keep both / ordered merge”
- Recycle bin with restore / permanent delete / retention
- File history: gzip snapshot, line diff, restore to any version
- Sharing: single file or folder, public URL, allow-copy toggle, GFM + wikilinks
- Blog: two built-in themes (`default`, `papertrail`), public index `/`, per-vault `/b/:vaultId`
- Collaboration on Markdown: invite / accept / revoke, real-time via SSE (fallback to long polling)
- Vault-scoped sync strategy: `user_choice` / `short_poll` / `long_poll`
- Console themes and blog themes as ZIP uploads
- SQLite by default, PostgreSQL optional; periodic storage reconciliation

## Architecture

```
cmd/server        # HTTP entry
configs/          # dev / prod YAML
internal/
  auth            # register, login, JWT, device auth
  syncapi         # Vault revision, upload/download, rename/delete
  vaults          # Vault CRUD, members, settings
  devices         # device state, vault authorization, cursor
  collaboration   # invite, accept, content write, events
  history/recycle # snapshots, restore, retention
  blog            # themes, public pages
  webui           # console pages, admin
plugin/src        # Obsidian plugin
```

Sync uses only HTTP. Short polling `wait=0` or long polling `wait=30` per Vault. Collaboration uses an account-level channel: SSE over HTTPS (or `app://obsidian.md` with CORS), long polling over plain LAN HTTP.

## Quick Start

### Prerequisites

- Go 1.25+
- Node 20+, npm
- Obsidian 1.4+

### Run backend

```bash
go run ./cmd/server
```

First registered user automatically becomes admin. Afterwards use that admin account to create others.


Listens on `http://localhost:8080` by default, data in `data/`. Health:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

Config via `OSS_ENV=dev|prod` → `configs/config.dev.yaml` / `configs/config.prod.yaml`, overridable by env: `OSS_SERVER_HOST`, `OSS_SERVER_PORT`, `OSS_DB_DRIVER`, `OSS_DB_DSN`, `OSS_STORAGE_DIR`, etc.

Postgres example:

```bash
export OSS_DB_DRIVER=postgres
export OSS_DB_DSN='postgres://user:pass@127.0.0.1:5432/oss?sslmode=disable'
go run ./cmd/server
```

### Docker

One-command install or upgrade on a Linux server:

```bash
curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/install.sh | sudo bash
```

The official bootstrap script asks for the host port, GitHub Release source, deployment path, and a total project storage limit. The source can be the accelerated URL, GitHub official, or a custom HTTPS URL prefix. It downloads both the latest amd64/arm64 container archive and its `checksums.txt` through the selected source, verifies SHA-256, and imports it with `docker load`. It detects Docker and offers to install it with Docker's official installer. Leave the port blank to choose one randomly from `10000-25565` while skipping common service ports. New installations keep persistent data under `/opt/oss-sync/data`; the application-wide storage limit rejects further sync writes when reached, and `0` means unlimited.

After installation, run the global `oss` or `oss-sync` command to update or uninstall OSS Sync, inspect runtime and storage usage, start, stop or restart it, and change the project capacity or mapped port. Each update asks which source to use, so the accelerated URL can be changed without reinstalling. Uninstalling removes the container and commands while retaining project data by default.

Running the same install command again downloads the latest Release and recreates the container while reusing its port, deployment path, and capacity setting. Legacy `oss-data` volumes remain in place and are not migrated automatically. Non-interactive installs can set `OSS_PORT`, `OSS_RELEASE_PROXY=official` (or a custom HTTPS URL prefix), `OSS_INSTALL_DIR`, `OSS_STORAGE_LIMIT_GB`, and `OSS_INSTALL_DOCKER=1`. For the global manager, set `OSS_RELEASE_SOURCE=official`, `OSS_RELEASE_SOURCE=proxy`, or `OSS_RELEASE_PROXY=https://example.com/` to select the source for an update. `OSS_IMAGE` remains an advanced override for a complete registry image such as `ghcr.io/helantianshen/oss-sync-server:0.1.12`.

The default SQLite installation does not pull PostgreSQL. If Docker Hub dependencies are added manually, a complete 1Panel mirror reference such as `docker.1panel.live/library/postgres:17` can be used without changing the Release download source or the Docker daemon configuration.

Source development can still build through Docker Compose:

Build and start a complete SQLite-backed environment with Docker Compose:

```bash
docker compose up -d --build
docker compose logs -f backend
```

The service is available at `http://localhost:8080`, and persistent data is stored in the `oss-data` named volume. Set `OSS_PORT=9090` to change the host port and `OSS_STORAGE_MAX_TOTAL_SIZE_MB` to apply an application-wide data-directory limit.

To build and run only the backend image:

```bash
docker build -t oss-sync-backend .
docker run --rm -p 8080:8080 \
  -v oss-data:/app/data \
  oss-sync-backend
```


Container deployments should be upgraded by rebuilding or replacing the image, not by mutating the running container binary. Removing the container keeps the named volume; `docker compose down -v` deletes its data and must be used with care.

### Build plugin

Regular users can install **OSS Sync and Share** directly from Obsidian Community Plugins. The following steps are only for source development:

```bash
cd plugin
npm ci
npm run build
# outputs plugin/manifest.json, main.js, styles.css
# copy to vault: <vault>/.obsidian/plugins/oss-sync/
```

Reload Obsidian → Enable *Obsidian Sync & Share* → Fill server URL, username/password → Create or bind a Vault. The plugin keeps a local ` .oss-sync-state.json` (v3) at vault root; it never uploads.

## Configuration

| Env | Description |
|---|---|
| `OSS_ENV` | `dev` or `prod` |
| `OSS_SERVER_HOST` / `PORT` | listen address |
| `OSS_DB_DRIVER` / `DSN` | sqlite or postgres |
| `OSS_STORAGE_DIR` | file storage root |
| `OSS_ALLOW_ANONYMOUS_REGISTRATION` | initial register switch |
| `OSS_WEB_SESSION_TTL_HOURS` | web console session lifetime in hours; default `24` |
| `OSS_DEVICE_JWT_TTL_HOURS` | plugin device token lifetime in hours; default `720` (30 days) |
| `OSS_DEVICE_STALE_DAYS` | stale device threshold |
| `OSS_RECONCILE_INTERVAL_HOURS` | storage check interval |
| `OSS_UPDATE_DOWNLOAD_SOURCE` | server update source: `official`, `proxy`, or `custom` |
| `OSS_UPDATE_DOWNLOAD_PROXY` | HTTPS URL prefix used when the source is `custom` |


Vault settings (per Vault, admin can force):

- `sync_mode`: `user_choice` | `short_poll` | `long_poll`
- recycle bin days, storage quota, upload size

Server updates use `download_source` and `download_proxy` from the update section in `configs/config.dev.yaml` or `configs/config.prod.yaml`. The Admin → System → Server update panel can override them for the current check and update. The selected source is used for both release metadata and the binary download, which allows updates when the server cannot reach GitHub directly.

## Development

```bash
# backend
go test ./...
go test -race ./...
go vet ./...

# plugin
cd plugin
npm exec tsc -- --noEmit
npm test
npm run build
```

Project conventions: Go with `gofumpt` + `golangci-lint`, TypeScript strict, `uv`/`pnpm` not required, no emoji in UI, CSS via `console.css` tokens, no inline styles.

## Deployment

- Put a reverse proxy with HTTPS in front of the Go binary.
- Back up `data/` (SQLite file or Postgres dump) and the JWT secret stored in DB.
- After initial users are created, turn off open registration in *Admin → System*.
- Monitor `/readyz`; alert on non-200 or repeated reconcile failures.

## Security

- Passwords are bcrypt-hashed, never logged.
- JWT is HS256 with per-deployment random secret.
- Sessions: web uses 24-hour HttpOnly Secure SameSite cookies + CSRF; plugin uses a 30-day device-bound Bearer JWT. Expired plugin tokens are removed locally and require a new login.
- All mutating web requests require CSRF; all sync/collab requests require approved device + vault authorization.

## License

MIT — see [LICENSE](LICENSE).
