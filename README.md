# OSS Sync

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/webui/assets/oss-logo-light.png">
    <img src="internal/webui/assets/oss-logo.png" alt="OSS Sync" width="240" height="95">
  </picture>
</p>

> Self-hosted Obsidian sync and share: notes, attachments, collaboration, and a public blog, with a single-binary server.

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![Obsidian](https://img.shields.io/badge/Obsidian-1.4+-7C3AED)](https://obsidian.md)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

English | [中文](README_zh.md) · [Documentation and project status](docs/README.md) · [Issues](https://github.com/helantianshen/oss-sync/issues)

Use **OSS Sync and Share** from the Obsidian community plugin marketplace. The Go server embeds the web console, public reading pages, and blog assets; no separate frontend deployment is required.

## Core features

- **Vaults and device authorization**: multiple Vaults per account, isolated member roles, file paths, and revisions. Device approval and Vault authorization are separate.
- **Multi-device sync**: create, modify, delete, rename, and move files within folders. HTTP short or long polling follows Vault policy. Empty folders are not independent sync objects.
- **Durable queue and conflicts**: local work persists across restarts; revision-based CAS with keep local, keep remote, keep both, and merge. Markdown supports three-way merge; binary conflicts preserve copies.
- **Attachments and configuration**: images, PDFs, and other attachments; optional `.obsidian` configuration sync, disabled by default. Workspace state, caches, and plugin `data.json` files are excluded.
- **Web file management**: Markdown, image, PDF, HTML, and SVG previews, with scripts disabled for HTML/SVG previews; text editing through the sync write path. File history, diffs, restore, and recycle bin.
- **Sharing and public blogs**: individual note and folder links, with an allow-copy toggle. Blogs render dynamically; enabling a public blog does not publish unshared notes.
- **Markdown collaboration**: invitations, acceptance, and revocation; SSE over HTTPS with long polling for plain HTTP or when SSE is unavailable.
- **Server extensions**: WASM and trusted executables, registering routes, hooks, middleware, admin pages, tasks, migrations, dependencies, and host RPC; packages may include blog templates and console themes.
- **Storage and deployment**: SQLite by default, optional PostgreSQL, periodic storage reconciliation; Linux binary/systemd, Docker, and manual runtime packages.

## Quick start

### Linux binary and systemd (recommended)

Supports Linux amd64/arm64. Requires root, running systemd, Python 3, curl, coreutils, `flock`, and `useradd`/`getent`. The installer checks dependencies; it does not install toolchains or Docker.

```bash
curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/oss.sh -o oss.sh
# Review the downloaded script before executing it
sudo bash oss.sh
```

Defaults: `/opt/oss-sync`, port `8080`, dedicated `oss-sync` service account. Installation asks for directory, port, capacity, and download source. Manage the service with `sudo oss` afterwards.

```bash
sudo oss status   # detailed systemd status
sudo oss          # update, start/stop, restart, configuration, logs, uninstall
```

See the [deployment guide](docs/deployment.md) for dependencies, non-interactive options, rollback, backups, and migration. Failed upgrades can restore the program and configuration, but not reverse database migrations. Back up before upgrading.

### Docker

```bash
docker compose up -d --build
docker compose logs -f backend
```

Default port: `8080`; persistent data: the `oss-data` named volume. Update by rebuilding or replacing the image. `docker compose down -v` deletes data. Do not run the binary installer over an existing Docker data directory; follow the [deployment guide](docs/deployment.md).

### Manual binaries and source

Download a runtime package from [Releases](https://github.com/helantianshen/oss-sync/releases), extract it, set `OSS_ENV=prod`, and run `bin/oss-server` (`bin/oss-server.exe` on Windows) with the package root as the working directory.

For source development, run from the repository root:

```bash
go run ./cmd/server
```

This loads `configs/config.dev.yaml`, listens on `http://localhost:9090`, and stores SQLite and files in `data/`.

```bash
curl -fsS http://localhost:9090/healthz
curl -fsS http://localhost:9090/readyz
```

Production configuration defaults to port `8080`. See [deployment](docs/deployment.md) for environment overrides and PostgreSQL, and [development](docs/development.md) for toolchains, client builds, and releases.

## First use

1. Open the actual service URL and register the first user, who becomes administrator. Disable open registration after creating initial users if appropriate.
2. Install **OSS Sync and Share** from the Obsidian community plugin marketplace. For manual installation, copy `main.js`, `manifest.json`, and `styles.css` into `<vault>/.obsidian/plugins/oss-sync/`.
3. Save a device name in plugin settings, then enter a server URL including `http://` or `https://` and your credentials.
4. Approve the device in the web console and authorize its target Vault. Approval does not grant access to every Vault.
5. Select an existing Vault in plugin settings, or create one and perform the first full sync.
6. Subsequent edits enter the durable queue; remote changes arrive through polling. Review the sync scope before enabling `.obsidian` configuration sync.

The Vault-root `.oss-sync-state.json` (v3) stores baselines, pending work, and conflicts; it is never uploaded. The client checks for new versions and points to the community plugin marketplace; it does not download or replace plugin files itself.

## Plugins, blog templates, and console themes

Installable extensions are plugin ZIPs. Blog templates and console themes are declared package resources, not independently uploaded extensions. Built-in resources cannot be deleted; enabled plugins add resources to the corresponding selectors.

- Built-in blog templates: `default` for shared reading, plus homepage-capable `papertrail`, `sakura`, `glass`, and `starry-night`.
- Contracts: [server plugin protocol and SDK](docs/server-plugins.md), [complete plugin guide](internal/webui/assets/plugin-guide.md).
- Examples: [Echo](examples/server-plugin-echo), [Sections](examples/server-plugin-sections), [Shirone](examples/server-plugin-shirone). Build executable entrypoints for the server OS and architecture; Windows example binaries cannot run directly on Linux.

## Security and operations

- Use HTTPS for public deployments and restrict the backend port to trusted reverse proxies. Forward the protocol header and configure SSE timeouts; see [deployment](docs/deployment.md).
- Passwords use bcrypt. JWT secrets are randomly generated and stored in the database; back up both the database and file storage.
- Web sessions default to 24 hours and use HttpOnly, SameSite cookies; Secure is set for HTTPS requests. Cookie-authenticated web writes require CSRF; Bearer API requests use a different authentication mechanism.
- Device-bound client tokens default to 720 hours (30 days), after which login is required. File sync requires device approval and target Vault authorization.
- WASM has no WASI, filesystem, network, database, or environment access, but still receives host-provided hook data. This is not a guarantee that arbitrary untrusted plugins are safe. Executable plugins have the server account's permissions. Install only reviewed extensions.
- Monitor `/readyz`: database failure or unresolved missing/hash-mismatched storage objects produces non-200 responses. This is not a complete end-to-end sync health check.

See [project status](docs/README.md#项目现状) for current coverage gaps, toolchain discrepancies, and plugin permission risks.

## License

MIT, see [LICENSE](LICENSE).
