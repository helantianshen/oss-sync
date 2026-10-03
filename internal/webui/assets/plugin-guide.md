# OSS Sync Server Plugin Guide

## 0. Choose the right runtime

Start with an **executable plugin** unless you have a specific reason to use WASM.

| Runtime | Use it when | Access model |
| --- | --- | --- |
| `executable` | You need the public Go SDK, database access, network calls, files, background tasks, or a normal plugin process | Administrator-trusted server program; same OS permissions as the server |
| `wasm` | You need a narrow, memory-isolated request/response extension | WASM ABI v1; no WASI, filesystem, network, or database imports |

Executable plugins are not a sandbox. They can read and write server files, access the database, use the network, read environment variables, and run commands as the server account. Only install code you trust.

## 1. The shortest path to a working plugin

A useful first plugin has four parts:

1. `manifest.json`: package identity and install metadata.
2. A prebuilt executable for every target server platform you support.
3. A Go program using `pkg/ossplugin`.
4. Optional assets, migrations, settings, hooks, routes, or admin pages.

Recommended first milestone:

- one public `GET` route;
- one authenticated `POST` route;
- one `blog.data` hook or one admin page;
- no raw SQL until the request/response path works.

Do not begin by implementing the whole feature. First prove: install → enable → callback → response.

## 2. Minimal package

### 2.1 ZIP layout

For an executable plugin:

```text
my-plugin.zip
├── manifest.json
├── plugin-linux-amd64
├── plugin-windows-amd64.exe
└── assets/
    └── app.js
```

The entrypoint paths in the manifest must match the ZIP paths exactly. The server does not compile Go on the VM.

For a minimal WASM plugin:

```text
my-plugin.zip
├── manifest.json
└── plugin.wasm
```

The archive limits are enforced before installation: 32 MiB total, at most 512 files, at most 64 MiB extracted content, and at most 32 MiB per non-manifest file. WASM is limited to 8 MiB.

### 2.2 Minimal executable manifest

```json
{
  "id": "hello-tools",
  "name": "Hello tools",
  "version": "1.0.0",
  "description": "A small executable plugin",
  "api_version": 1,
  "runtime": "executable",
  "entrypoints": {
    "linux-amd64": "plugin-linux-amd64",
    "windows-amd64": "plugin-windows-amd64.exe"
  },
  "routes": [],
  "registration": {
    "routes": [
      {
        "method": "GET",
        "path": "/hello-tools",
        "callback": "hello.page",
        "auth": "public"
      }
    ]
  }
}
```

`id` is the stable identity. Do not change it during an upgrade. `version` is the plugin version shown by the host. `api_version` is currently `1`.

Optional top-level fields:

| Field | Purpose |
| --- | --- |
| `description` | Shown in the plugin list |
| `settings` | Host-rendered settings, stored per Vault |
| `settings_visibility` | Sidebar visibility for the settings entry: `always` shows it as soon as the plugin is enabled; omitted or `when_used` shows it only when one of the plugin's blog or console theme resources is selected by a Vault. Plugins without theme resources always show their settings |
| `update_url` | Author-run version endpoint used by **Check for updates**. See §11.1 |
| `auto_check_update` | Reserved automatic-check flag; the host does not check on startup yet |
| `hooks` | Simplified hook declaration |
| `blog_themes` | Blog theme resources |
| `console_themes` | Console theme resources |
| `args` | Arguments passed to an executable process |

### 2.3 Minimal Go program

```go
package main

import (
    "context"

    "github.com/helantianshen/oss-sync/pkg/ossplugin"
)

func main() {
    registration := ossplugin.Registration{
        Routes: []ossplugin.Route{
            {
                Method:   "GET",
                Path:     "/hello-tools",
                Callback: "hello.page",
                Auth:     "public",
            },
        },
    }

    ossplugin.RunMain(registration, func(client *ossplugin.Client) error {
        return client.On("hello.page", func(ctx context.Context, request ossplugin.Request) (ossplugin.Response, error) {
            return client.WriteTextResponse(200, "hello from OSS Sync"), nil
        })
    })
}
```

`RunMain` starts the JSON Lines protocol, sends the ready frame, registers the runtime capabilities, and dispatches callbacks. Write diagnostics to stderr. Never write logs to stdout: stdout is the plugin protocol stream.

Build from the repository root while developing against this checkout:

```text
go build ./examples/server-plugin-echo/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o plugin-linux-amd64 ./path/to/main.go
```

An external plugin should use its own Go module and import `github.com/helantianshen/oss-sync/pkg/ossplugin` at a released version or a deliberate local `replace` during development. Do not copy the SDK into the plugin.

## 3. Manifest versus runtime registration

There are two declarations because the host has two responsibilities:

| Declaration | Purpose |
| --- | --- |
| `manifest.json` | Package validation, entrypoint selection, installation, legacy namespaced routes, and recovery metadata |
| `ossplugin.Registration` sent by `RunMain` | The executable process's active hooks, dynamic routes, middleware, admin pages, assets, settings, tasks, migrations, dependencies, and lifecycle callbacks |

For an executable plugin, the ready-frame registration becomes the active runtime registration. Keep the manifest `registration` and the Go `Registration` consistent. A mismatch is a common reason for a route, asset, or admin page to be missing after installation.

The top-level manifest `routes` array is a separate legacy/package route declaration. Use the runtime `Registration.Routes` for new executable-plugin routes and use the top-level manifest routes only when you intentionally need the `/plugins/<id>/...` and `/api/plugins/<id>/...` namespaces.

## 4. Routes and authentication

### 4.1 Dynamic runtime routes

Runtime routes support:

```go
ossplugin.Route{
    Method:   "POST",
    Path:     "/hello-tools/items",
    Callback: "items.create",
    Auth:     "user", // public, user, or admin
    Priority: 10,
}
```

Use a plugin-specific path prefix such as `/hello-tools/...`. Dynamic routes are matched by the host's fallback router. Exact paths and terminal `/*` path patterns are supported.

Runtime route requests contain:

- `Method`, `Path`;
- `Query`, `Params`;
- `Headers`, `Cookies`;
- `User` for authenticated requests;
- `BodyBase64` for the complete request body;
- `Callback` when supplied by the route.

`public` does not authenticate the request. `user` accepts a Bearer identity or the web-console session cookie. Cookie-authenticated state-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) require the `X-CSRF-Token` header to match the `oss_csrf` cookie. Bearer requests do not need CSRF because the bearer token is not sent automatically by a browser.

### 4.2 Namespaced manifest routes

Top-level manifest routes are exposed as:

```text
/plugins/<plugin-id>/<declared-path>       public namespace
/api/plugins/<plugin-id>/<declared-path>   authenticated namespace
```

An authenticated route requires a normal OSS Sync Bearer JWT in the `Authorization` header. When its query contains `vault_id`, the host verifies Vault access and sends the saved plugin settings in the request. Public routes do not receive Vault settings.

Prefer dynamic runtime routes for a new executable plugin when you need `user`/`admin` auth, request cookies, request headers, or raw browser-console access. Do not assume that a route is automatically prefixed by the plugin ID.

### 4.3 Response helpers

```go
return client.WriteTextResponse(200, "ok"), nil

return client.WriteJSONResponse(200, map[string]any{
    "ok": true,
})
```

For a custom response:

```go
return ossplugin.Response{
    Status:  201,
    Headers: map[string]string{
        "Content-Type": "application/json; charset=utf-8",
    },
    BodyBase64: base64.StdEncoding.EncodeToString(body),
}, nil
```

The host limits request and response bodies to 1 MiB and one callback invocation to approximately two seconds. Return a real status code and a useful JSON error; do not hide errors as HTTP 200.

## 5. Admin pages and automatic console styling

Register an admin page:

```go
AdminPages: []ossplugin.AdminPage{
    {
        Slug:     "orders",
        Label:    "Orders",
        Callback: "orders.admin",
    },
},
```

The page appears in the administrator menu at:

```text
/dashboard/admin/plugins/<plugin-id>/page/orders
```

### 5.1 Default: return an HTML fragment

Return a fragment, not a full document:

```go
func ordersAdmin(client *ossplugin.Client) ossplugin.Handler {
    return func(context.Context, ossplugin.Request) (ossplugin.Response, error) {
        html := `<section class="ledger-panel">
  <header><h2>Orders</h2></header>
  <form method="post">
    <label><span>Search</span><input name="q"></label>
    <label><span>Status</span><select name="status"><option>open</option></select></label>
    <button class="button button--primary" type="submit">Search</button>
  </form>
</section>`
        return client.WriteTextResponse(200, html), nil
    }
}
```

The host automatically wraps a fragment in the console layout and loads:

- `console.css`;
- `theme.js` and the current light/dark state;
- the active console theme CSS;
- the console sidebar and top bar;
- the current user and CSRF context.

Use existing classes when you want the normal appearance:

- `.button`, `.button--primary`, `.button--danger`;
- `.text-button`;
- `.gate-panel`, `.gate-form`, `.stack-form`;
- `.ledger-panel`, `.control-grid`, `.action-cell`.

Bare `input` and `select` elements receive base console styling. Bare `button` and `textarea` do not receive the complete button/editor appearance: use the classes above or supply plugin CSS.

### 5.2 Explicit opt-out: return a complete document

If the callback returns content beginning with `<!doctype html>` or `<html>`, the host serves it unchanged. Use this only when the plugin intentionally owns:

- the complete `<html>`, `<head>`, and `<body>`;
- stylesheet and script loading;
- responsive layout;
- light/dark theme behavior;
- CSRF and navigation UI.

A standalone document does not automatically inherit the console theme.

## 6. Static assets

Declare runtime assets:

```go
Assets: []ossplugin.Asset{
    {Path: "assets/app.js"},
    {Path: "assets/app.css"},
},
```

Ship the files at those exact paths in the ZIP. They are served at:

```text
/plugins/<plugin-id>/assets/assets/app.js
/plugins/<plugin-id>/assets/assets/app.css
```

Avoid confusing duplicate `assets/assets` paths. A simpler package is:

```text
manifest.json
plugin
app.js
app.css
```

```go
Assets: []ossplugin.Asset{
    {Path: "app.js"},
    {Path: "app.css"},
},
```

Then reference `/plugins/<plugin-id>/assets/app.js`.

If a standalone plugin page wants to reuse the console base stylesheet, it may link:

```html
<link rel="stylesheet" href="/ui/assets/console.css">
<script src="/ui/assets/theme.js"></script>
```

That does not automatically load the user's selected custom console theme or the console shell. The safer default is to use an AdminPage fragment and let the host wrap it.

## 7. Settings

Declare settings in runtime registration (and keep the manifest registration consistent):

```go
Settings: []ossplugin.SettingField{
    {Key: "endpoint", Label: "Endpoint", Type: "url", MaxLength: 500},
    {Key: "label", Label: "Label", Type: "text", MaxLength: 100},
},
```

The host settings schema supports `text`, `textarea`, `url`, `choice`, and non-nested `group`. The current Go SDK `SettingField` only describes simple fields: `choice` requires `choices`, and `group` requires nested fields, so neither can be declared with the SDK type shown above. Settings are stored per Vault. A blog theme resource may declare public keys in `theme.json` under `public_settings`; the host exposes only those keys through the generic blog fields and `.ThemeConfigJS`. Other public routes do not receive Vault settings.

The plugin cannot inject arbitrary settings HTML or JavaScript. Use declared fields for configuration and use an AdminPage for operational UI.

SDK access:

```go
var settings struct {
    Endpoint string `json:"endpoint"`
    Token    string `json:"token"`
}
if err := client.Services().GetSetting(ctx, vaultID, &settings); err != nil {
    return ossplugin.Response{}, err
}
```

## 8. Hooks and blog integration

### 8.1 Content filters

A filter receives content and returns replacement content:

```go
Hooks: []ossplugin.Hook{
    {Name: "blog.content", Callback: "blog.content", Kind: "filter"},
},
```

```go
client.On("blog.content", func(_ context.Context, request ossplugin.Request) (ossplugin.Response, error) {
    content, _ := request.Payload["content"].(string)
    return client.WriteTextResponse(200, content+"\n<!-- plugin -->"), nil
})
```

Use `action` when you only need a side effect and must not replace the value. Filters run in priority order and the output of one filter becomes the input of the next.

Common host-called names include `blog.content`, `theme.render`, and `blog.data`. Hook names are validated, but plugin-defined names can be triggered through the host hook mechanism where the caller supports them.

### 8.2 Blog data

`blog.data` is the preferred way to add widgets, comments, VIP state, statistics, or recommendations without rewriting the article body. The payload includes:

- `vault_id`, `share_id`, `path`;
- `is_home`, `is_folder`;
- `method`, `request_url`, `query`;
- `headers`, `cookies`, `client_ip`.

Return JSON. The blog exposes it under `.PluginData[plugin-id]`. A theme can read it with `pluginField`:

```gotemplate
{{with pluginField .PluginData "comments-plugin" "items"}}
  {{range .}}<article>{{.body}}</article>{{end}}
{{end}}
```

Use `safeHTML` only for HTML produced by trusted plugin code. Never pass user comments or database text directly to `safeHTML` without sanitizing it yourself.

## 9. Database, migrations, and host services

### 9.1 Plugin-owned tables

Use a migration for plugin-owned tables:

```go
Migrations: []ossplugin.Migration{
    {
        ID: "comments_v1",
        Statements: []string{
            `CREATE TABLE IF NOT EXISTS plugin_comments (
                id INTEGER PRIMARY KEY,
                vault_id TEXT NOT NULL,
                file_path TEXT NOT NULL,
                body TEXT NOT NULL,
                created_at DATETIME NOT NULL
            )`,
        },
    },
},
```

Migration IDs are applied once per plugin. A migration batch runs in one database transaction. Keep migrations additive and compatible with upgrades.

### 9.2 Query and execute

```go
rows, err := client.Services().Query(ctx,
    "SELECT id, body FROM plugin_comments WHERE file_path = ? ORDER BY id DESC",
    path,
)

_, err = client.Services().Exec(ctx,
    "INSERT INTO plugin_comments (vault_id, file_path, body, created_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)",
    vaultID, path, body,
)
```

`db.exec` is intentionally powerful for trusted executable plugins. Treat it as a dangerous capability:

- prefer plugin-owned tables;
- use parameter arguments, never string concatenation;
- do not rewrite core file rows directly;
- do not store secrets in logs;
- do not assume core schema internals are stable.

### 9.3 Core model services

`host.models` exposes these model names:

```text
users, vaults, files, shares, collaborations, devices
```

The SDK exposes `Models`, `ModelList`, `Create`, `Update`, and `Delete`. These operations are useful for read-only dashboards and carefully scoped business operations, but plugin-owned data should normally stay in plugin-owned tables.

### 9.4 Files

Use the SDK wrappers:

```go
file, err := client.Services().GetFile(ctx, vaultID, path)
result, err := client.Services().PutFile(ctx, vaultID, path, content)
```

`PutFile` runs the host's real write pipeline: path validation, quota checks, atomic storage, hash deduplication, sync revision, history, long-poll notification, and collaboration notification. Do not update `files` or storage blobs with raw SQL.

A plugin is trusted, so the host RPC does not replace your business authorization. Check `request.User`, verify the target Vault, and scope every operation to the current user or an explicitly authorized administrator.

Other typed host operations include Vault CRUD, Share CRUD, blog get/filter, plugin listing, per-Vault settings, and `host.hook`. Use `client.HostCall` when a typed SDK wrapper is not yet available.

## 10. Scheduled tasks

```go
Tasks: []ossplugin.Task{
    {Name: "send_digest", Schedule: "@hourly", Callback: "digest.run"},
},
```

Tasks run through the existing scheduler. Keep jobs idempotent: a process restart or retry must not duplicate notifications or corrupt data. Store a checkpoint or unique event ID in a plugin-owned table.

## 11. Lifecycle and upgrades

```go
Lifecycle: ossplugin.Lifecycle{
    Activate:   "plugin.activate",
    Deactivate: "plugin.deactivate",
    Upgrade:    "plugin.upgrade",
    Uninstall:  "plugin.uninstall",
},
```

The normal flow is:

1. Administrator uploads a ZIP.
2. The host validates paths, sizes, manifest, and entrypoints.
3. Executable plugins complete the ready handshake.
4. The web console enables the plugin after installation.
5. The host starts one persistent process for each enabled executable plugin.
6. On upgrade, migrations and lifecycle callbacks run; the old package is retained until activation succeeds.
7. On disable, the process receives shutdown and resources disappear from selectors.
8. Delete a plugin only after disabling it.

If the process crashes or violates the protocol, pending calls fail and the plugin can be enabled again to start a fresh process.

### 11.1 Self-service update endpoint

The host does not bundle an update channel for plugins; authors own distribution. Declare a lookup endpoint in the manifest:

```json
{
  "id": "your-plugin",
  "version": "1.0.0",
  "update_url": "https://example.com/oss-plugin/latest.json",
  "auto_check_update": false
}
```

When an administrator clicks **Check for updates**, the host issues a GET to `update_url` and expects:

```json
{
  "version": "1.1.0",
  "url": "https://example.com/oss-plugin/your-plugin-1.1.0.zip",
  "notes": "Fixes attachment sync"
}
```

Behaviour by outcome:

| Outcome | Host behaviour |
| --- | --- |
| `update_url` omitted | No check action is offered; the plugin must implement updates itself |
| Endpoint unreachable or non-200 | Reports a failed check; other plugins are unaffected |
| `version` not newer than current | Reports that the plugin is up to date |
| Newer version with `url` | The action becomes **Update to x.y.z** and the version cell shows **New version: x.y.z**; the host downloads and applies the upgrade |
| Newer version without `url` | Tells the administrator to update manually from the author's instructions |

Versions are compared as strict SemVer (`major.minor.patch`); an invalid version is treated as no update. `auto_check_update` is reserved for future automatic checks and currently means nothing beyond documentation, so plugin startup never depends on an external endpoint.

Downloading reuses the upload pipeline: size limits, path traversal and symlink checks, manifest and entrypoint validation, the ready handshake, and old-package retention on failure. The `url` must therefore point at a complete plugin ZIP, not at a patch file.

## 12. Testing checklist

Before uploading:

```text
go test ./...
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o plugin-linux-amd64 ./main.go
```

Verify at least:

- a clean install from an empty data directory;
- startup on the target OS/architecture;
- public route status and response body;
- authenticated route with missing, valid, and invalid credentials;
- CSRF rejection for cookie-authenticated writes;
- settings for two different Vaults do not mix;
- migration runs once and survives restart;
- plugin disable stops routes and tasks;
- upgrade preserves data and rolls back on failed startup;
- a plugin process writes diagnostics to stderr, not stdout;
- user-controlled HTML is escaped or sanitized.

## 13. Debugging checklist

| Symptom | Check |
| --- | --- |
| Plugin is not installed | ZIP contains `manifest.json`, valid paths, and a target entrypoint |
| Plugin installs but will not enable | Run the binary manually; check stdout protocol and stderr; verify `api_version` |
| Route returns 404 | Check whether it is a manifest namespace route or runtime route; check exact path and registration callback |
| Route returns 401 | Check `Auth`, Bearer header, web session cookie, and whether a cookie POST includes `X-CSRF-Token` |
| Route runs but settings are empty | Use an authenticated namespaced route with `vault_id`; public and dynamic route contexts do not automatically receive settings |
| Admin page is unstyled | Return an HTML fragment, not a full document; use `.button` classes |
| Admin page has no sidebar | The callback probably returned `<html>` and intentionally opted out of host wrapping |
| Asset returns 404 | Declare the exact asset path in runtime `Assets` and include the same path in the ZIP |
| File edit creates no sync revision | Use `Services().PutFile`; never write the core `files` table directly |
| Process becomes unavailable | Check stdout for non-protocol logs, response size, callback duration, and frame IDs |

## 14. AI-assisted development template

When asking an AI to implement a plugin, give it this context first:

```text
You are implementing an OSS Sync executable server plugin.
Use github.com/helantianshen/oss-sync/pkg/ossplugin.
The plugin is trusted server code, not a sandbox.
Do not invent host methods or manifest fields: verify them in pkg/ossplugin/sdk.go,
internal/serverplugin/package.go, internal/serverplugin/registration.go, and docs.
Use plugin-owned migrations/tables for plugin data.
Use Services().PutFile for file writes; never update core file rows or blobs directly.
For browser cookie-authenticated writes, send X-CSRF-Token matching oss_csrf.
Admin page callbacks should return an HTML fragment unless they intentionally return
<!doctype html> or <html> as a complete document.
Keep stdout reserved for the plugin protocol; log only to stderr.
Before coding, state the manifest, callbacks, routes, data model, authorization,
and test cases. Then implement the smallest end-to-end slice and run focused tests.
```

Ask the AI to return these artifacts:

1. package tree;
2. manifest and runtime registration;
3. callback-to-route table;
4. request fields and response schema;
5. migration SQL and ownership rules;
6. threat model and input validation;
7. build/package commands for every target platform;
8. deterministic tests for auth, no-op writes, retries, and upgrades.

Never accept an answer that invents an SDK method, assumes a route namespace, writes core file rows directly, or treats an executable plugin as sandboxed.

## Blog template contract

Blogs render dynamically per HTTP request; they do not export static sites. Built-in `default` handles shared reading, while `papertrail` supports a blog homepage. Select a homepage-capable theme in Vault settings, enable the public blog, and create individual article shares. Enabling a blog does not publish unshared notes.

### Installation

Templates are plugin resources, not independently installable ZIPs. Include a valid manifest, a working runtime entrypoint, and the resource files. Add this field to the plugin manifest:

```json
"blog_themes": [{"id":"clean","name":"Clean","path":"blog/clean"}]
```

```text
manifest.json
<runtime entrypoint declared by the manifest>
blog/clean/template.html
blog/clean/style.css
blog/clean/theme.js
blog/clean/theme.json
```

`template.html` is required; other files depend on the template. Include the resource files in the ZIP, upload and enable the plugin in plugin management, then select its theme in Vault settings. Use `.ThemeBaseURL` for `/themes/<plugin-id>--<resource-id>` assets. Disabling the plugin makes its resources unavailable. Edit source files through plugin management; `data/themes` contains derived copies regenerated on enable or upgrade. There is no standalone template upload or scaffold management page.

Recommended `theme.json`:

```json
{"supports_public_blog":true,"public_settings":["blog_name","description"]}
```

Only advertise homepage support when the template implements `.IsHome`. Legacy templates without `theme.json` are considered homepage-capable; when the file exists, an omitted/false flag or invalid JSON disables support. Public settings require homepage support, an enabled owning plugin, a whitelist entry, a matching top-level `settings` or `registration.settings` declaration, and a saved Vault value. Never whitelist secrets.

### Rendering contract

```gotemplate
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{{.Title}}</title>
<link rel="stylesheet" href="{{.ThemeBaseURL}}/style.css"></head><body>
{{if .IsHome}}
  <h1>{{.Title}}</h1>
  {{range .HomePosts}}<article><h2><a href="{{.URL}}">{{.Title}}</a></h2><p>{{.Summary}}</p></article>{{else}}<p>No shared posts</p>{{end}}
{{else if .IsFolder}}
  <h1>{{.FolderTitle}}</h1><nav>{{.ContentHTML}}</nav>
{{else}}
  <h1>{{.ArticleTitle}}</h1><article>{{.ContentHTML}}</article>
{{end}}
</body></html>
```

| Field | Contract |
| --- | --- |
| `.Title`, `.ThemeName`, `.ThemeBaseURL` | Page title, theme resource name, asset URL prefix |
| `.VaultID`, `.ShareID` | Vault and share IDs; ShareID is empty on the homepage |
| `.IsHome`, `.HomePosts` | `/b/<vault-id>` homepage and all valid individually shared Markdown posts, ordered by share creation descending then share ID; no pagination; folder-only shares are excluded |
| HomePosts item | `.Title`, `.Summary`, `.URL`, `.Date`, `.Time`, `.Category`, `.Tags`, `.CoverURL`, `.WordCount`; Time is file update time, Date prefers frontmatter |
| `.IsFolder`, `.FolderTitle` | Folder index and title; an article inside a folder has IsFolder=false |
| `.ArticleTitle`, `.ArticlePost` | Article title and metadata; ArticlePost has `.Summary`, `.Date`, `.Category`, `.Tags`, `.CoverURL`, `.WordCount`, `.ReadingMinutes` |
| `.ContentHTML` | Rendered article or directory HTML; empty on homepages, which must use HomePosts |
| `.AllowCopy`, `.BlogHomeURL` | Whether to show copy controls; public homepage URL, empty when disabled |
| `.BlogName`, `.Description` | Blog name and introduction |
| `.LogoURL`, `.LogoSize`, `.LogoShape` | Logo URL, pixel size, square/circle |
| `.BannerURL`, `.MobileBannerURL` | Desktop and mobile banner URLs |
| `.Buttons` | Links with `.Label`, `.URL`, `.IconURL`, `.Position` |
| `.CustomHeader`, `.CustomFooter`, `.FooterNotice` | Custom fragments and notice; custom fragments are policy-controlled and sanitized |
| `.ThemeConfigJS` | Safe JSON for `<script>window.config = {{.ThemeConfigJS}};</script>`; there is no `.ThemeConfig` field |
| `.PluginData` | Plugin display data; read via `pluginField`, `hasPlugin`, or guarded `with index`; use `safeHTML` only for trusted, sanitized HTML |

Frontmatter starts at the beginning of the document and ends with a standalone `---` line. Fields are `title`, `description` (fallback `summary`), `published` (fallback `date`), `category`, `tags`, and `image` (fallback `cover`). Primary keys take precedence. YAML literal/folded multiline descriptions, quoted strings, tag lists and comma-separated tag text are supported. Valid frontmatter is hidden; malformed/unclosed blocks remain visible. Defaults are the filename for title, first non-heading body line for summary, and file update time for date.

`./` and `../` attachment references resolve only relative to the article and cannot escape the Vault. Other paths prefer an exact Vault-root match, then an article-relative match; bare names finally match same-Vault attachment basenames, preferring the latest mtime. Use `./` to avoid root-name collisions. Markdown image and frontmatter cover URLs support encoded paths, query parameters and fragments: only the decoded path selects the attachment, while the fragment remains on the generated URL. Wiki embeds use literal filenames, so percent signs and hash characters do not become URL escapes or fragments. Remote HTTP(S) images/covers retain their URL; covers starting with `/` are site URLs. Folder asset URLs carry a `source` article path, checked against share scope, live article existence and its actual references.

### Diagnostics and acceptance

Templates use Go `html/template` escaping and `missingkey=zero`. Missing map keys yield zero values; unknown struct fields, unguarded nil chains and syntax errors still fail. Guard optional data with `with`. Failures fall back to `default` and expose the reason in `X-Theme-Fallback`. Default has no homepage post list, so HTTP 200 alone does not prove successful custom rendering.

Verify homepage, individual article, folder index and folder article separately. Check template markers, fallback headers, CSS/JS/images, copy-disabled states, empty and over-100-post lists, mobile layout and keyboard behavior. Existing templates receive the complete HomePosts list; large collections create longer pages.
