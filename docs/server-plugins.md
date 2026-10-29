# OSS Sync Server Plugins

## 1. Runtime choice

OSS Sync has two runtimes:

- `executable`: a persistent, administrator-trusted process launched with the server account's OS permissions. Use this for the Go SDK, network integrations, database-backed features, file services, scheduled tasks, and admin pages.
- `wasm`: WASM ABI v1 with a narrow memory boundary. WASI, filesystem, network, and database imports are unavailable.

An executable plugin is not a sandbox. It can read/write server files, access the database, use the network, read environment variables, and run commands as the server account. Install only reviewed code.

## 2. Package contract

Every ZIP contains `manifest.json` and the files referenced by that manifest. The archive limits are:

- 32 MiB archive;
- 512 files;
- 64 MiB extracted content;
- 32 MiB per non-manifest file;
- 8 MiB for `plugin.wasm`.

Absolute paths, `..`, backslashes, duplicate entries, directories, and symlinks are rejected.

### Executable package

```text
manifest.json
plugin-linux-amd64
plugin-windows-amd64.exe
assets/...
```

```json
{
  "id": "hello-tools",
  "name": "Hello tools",
  "version": "1.0.0",
  "api_version": 1,
  "runtime": "executable",
  "entrypoints": {
    "linux-amd64": "plugin-linux-amd64",
    "windows-amd64": "plugin-windows-amd64.exe"
  },
  "routes": [],
  "registration": {
    "routes": [
      {"method":"GET","path":"/hello-tools","callback":"hello.page","auth":"public"}
    ]
  }
}
```

The server runs the selected prebuilt entrypoint. It does not compile Go on the VM.

### WASM package

```text
manifest.json
plugin.wasm
```

A WASM module must export `memory`, `oss_abi_version`, `oss_alloc`, and `oss_handle`, and must not import functions or memory. The request and response JSON shapes are the same as the executable protocol.

## 3. Manifest and ready registration

The manifest controls package installation, platform entrypoints, resource extraction, and recovery metadata. An executable process sends a runtime registration in its first `ready` frame. The runtime registration controls active hooks, routes, middleware, admin pages, assets, settings, tasks, migrations, dependencies, and lifecycle callbacks.

Keep both declarations consistent. If an asset or route is present only in `manifest.json` but absent from the Go `Registration`, the installed file may exist while the active process does not expose it.

The top-level manifest `routes` array is the legacy/package route declaration. New executable plugins should generally declare their callback-driven capabilities in `registration` and in the Go SDK registration.

## 4. Executable protocol

The host starts one process for every enabled executable plugin. The working directory is the installed plugin directory. Requests and responses are UTF-8 JSON Lines on stdin/stdout. Diagnostics belong on stderr; stdout must contain protocol frames only.

The first plugin frame must be:

```json
{"type":"ready","api_version":1}
```

A ready frame may include `registration`:

```json
{
  "type": "ready",
  "api_version": 1,
  "registration": {
    "routes": [
      {"method":"GET","path":"/hello-tools","auth":"public","callback":"hello.page"}
    ],
    "admin_pages": [
      {"slug":"orders","label":"Orders","callback":"orders.admin"}
    ]
  }
}
```

The host sends requests like:

```json
{
  "type":"request",
  "id":"1",
  "request": {
    "method":"GET",
    "path":"/hello-tools",
    "query":{"name":["world"]}
  }
}
```

The plugin replies with the same ID:

```json
{
  "type":"response",
  "id":"1",
  "response": {
    "status":200,
    "headers":{"Content-Type":"text/plain; charset=utf-8"},
    "body_base64":"aGVsbG8="
  }
}
```

The host can send `shutdown`. The plugin should flush stdout after every frame and exit cleanly. Requests and responses are limited to 1 MiB; one invocation is limited to approximately two seconds. A missing handshake, malformed frame, process crash, or protocol violation fails pending calls and marks the instance unavailable until it is enabled again.

The process receives:

```text
OSS_PLUGIN_ID
OSS_PLUGIN_DIR
OSS_PLUGIN_PROTOCOL
```

## 5. Runtime registration surface

The public Go SDK `ossplugin.Registration` supports:

- `Hooks`: action/filter callbacks;
- `Routes`: callback routes with `public`, `user`, or `admin` auth;
- `Middleware`: before/after request processing;
- `AdminPages`: administrator pages;
- `Assets`: files served from the installed package;
- `Settings`: per-Vault declared fields;
- `Tasks`: scheduled callbacks;
- `Migrations`: plugin-owned SQL migrations;
- `Dependencies`: enabled-plugin/version dependencies;
- `Lifecycle`: activate, deactivate, upgrade, uninstall callbacks.

The host validates registration names, paths, counts, settings, migrations, and callback names before activating the process.

## 6. Routes and namespaces

Top-level manifest routes are exposed at:

```text
/plugins/<plugin-id>/<declared-path>       public namespace
/api/plugins/<plugin-id>/<declared-path>   Bearer-authenticated namespace
```

Authenticated namespaced routes require an OSS Sync Bearer JWT. If a request has `vault_id`, the host verifies Vault access and includes the plugin's saved settings. Public routes do not receive Vault settings.

Runtime dynamic routes are matched by their declared absolute path. Prefer a plugin-specific prefix such as `/hello-tools/`. Runtime routes can use `public`, `user`, or `admin` authentication. User routes accept Bearer identity or the console web session cookie. Cookie-authenticated state-changing requests require the `X-CSRF-Token` header to match the `oss_csrf` cookie.

The request object includes method, path, query, params, headers, cookies, user, settings, hook, payload, and base64 body as applicable.

## 7. Host services

The executable SDK exposes:

```text
db.query
db.exec
host.models
host.model.list
host.model.create
host.model.update
host.model.delete
host.vault.*
host.file.get
host.file.put
host.share.*
host.blog.*
host.plugin.list
host.settings.get
host.settings.set
host.hook
```

Typed SDK methods are available from `client.Services()` for queries, model operations, plugin listing, settings, file reads, and file writes.

### Database

`db.query` and `db.exec` are intentionally powerful for trusted executable plugins. Use parameter arguments and plugin-owned tables. Do not write core file rows or storage blobs directly.

### Files

Use:

```go
file, err := client.Services().GetFile(ctx, vaultID, path)
result, err := client.Services().PutFile(ctx, vaultID, path, content)
```

`PutFile` runs path validation, quota checks, atomic storage, SHA-256 deduplication, sync revision, history, long-poll notifications, and collaboration notifications. Direct SQL/file writes bypass these invariants.

The host capability does not replace plugin business authorization. Check the authenticated user and target Vault before using a host service.

## 8. Admin pages and console theme

Admin pages appear in the administrator menu at:

```text
/dashboard/admin/plugins/<plugin-id>/page/<slug>
```

By default, an admin callback should return an HTML **fragment**. The host wraps the fragment in the console layout and loads `console.css`, `theme.js`, the active console theme, sidebar, user context, and CSRF context. Reuse `.button`, `.button--primary`, `.button--danger`, `.text-button`, `.gate-form`, `.stack-form`, `.ledger-panel`, and related classes.

Bare `input` and `select` elements receive base console styling. Buttons need `.button` classes for the full appearance. Textareas and special controls need a plugin class.

If callback output begins with `<!doctype html>` or `<html>`, the host treats it as a complete document and serves it unchanged. That is the explicit opt-out for a plugin that owns its entire page shell and theme.

## 9. Blog hooks and data

Declare hooks such as:

```json
{
  "hooks": [
    {"name":"blog.content"},
    {"name":"blog.data"}
  ]
}
```

`blog.content` is a content filter. A filter returns replacement content; an action performs side effects without replacing the value.

`blog.data` receives:

```text
vault_id, share_id, path,
is_home, is_folder,
method, request_url, query,
headers, cookies, client_ip
```

The returned JSON is available in the selected blog template under `.PluginData[plugin-id]`. This is the preferred integration point for comments, VIP state, article statistics, recommendations, and widgets.

`safeHTML` is for trusted plugin-generated HTML only. Sanitize user-controlled comments, names, URLs, and article fields before rendering them as HTML.

## 10. Settings, migrations, tasks, and dependencies

Settings use the constrained field schema: `text`, `textarea`, `url`, `choice`, and non-nested `group`. Values are stored per Vault and sent only in contexts where the host has established Vault access.

Migrations are identified by plugin ID plus migration ID and run transactionally. Use additive, upgrade-safe migrations. Tasks use the existing scheduler; make them idempotent and persist checkpoints in plugin-owned tables. Dependencies must be enabled before the dependent plugin starts.

## 11. Lifecycle

The normal lifecycle is:

1. Administrator uploads the ZIP.
2. The host validates the manifest and payload.
3. Executable plugins complete the ready handshake.
4. The web-console upload flow enables the plugin.
5. One persistent process starts per enabled executable plugin.
6. Server restart restores enabled plugins.
7. Disable sends a graceful shutdown and removes materialized plugin resources from selectors.
8. Delete requires the plugin to be disabled first.

Upgrade migrations and lifecycle callbacks must remain compatible with the previous version. Failed activation restores the previous package where possible; external side effects created by the plugin cannot be rolled back by the host.

## 12. Security requirements

Executable plugins have server-account permissions. Treat these as mandatory review items:

- validate every user-provided path, URL, ID, and body;
- use parameterized SQL;
- scope every query by user/Vault ownership;
- use `PutFile` for file writes;
- sanitize or escape user-controlled HTML;
- protect cookie-authenticated writes with CSRF;
- keep credentials in settings or environment, never source or logs;
- never write non-protocol data to stdout;
- cap pagination, request sizes, and remote API retries;
- make scheduled jobs and webhooks idempotent.

## 13. Testing and debugging

Before upload:

```text
go test ./...
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o plugin-linux-amd64 ./main.go
```

Test a clean install, target-platform startup, public and authenticated routes, CSRF rejection, per-Vault settings isolation, migration idempotence, disable/re-enable, upgrade failure recovery, process restart, no-op file writes, and user-controlled HTML.

Common diagnosis:

| Symptom | Likely cause |
| --- | --- |
| Cannot install | Invalid ZIP path, missing manifest, wrong entrypoint, or package limit |
| Cannot enable | Invalid ready frame, wrong API version, stdout log noise, or callback registration error |
| Route 404 | Wrong namespace, wrong exact path, missing runtime registration, or disabled plugin |
| Route 401/403 | Wrong `Auth`, missing Bearer/session identity, or missing CSRF header |
| Settings empty | Public route, missing `vault_id`, or Vault access failure |
| Admin page unstyled | Returned a complete document or did not use console classes in a fragment |
| Asset 404 | Manifest asset and runtime `Assets` list do not match the ZIP path |
| File revision missing | Core `files` table/blob was changed directly instead of using `PutFile` |
| Process unavailable | Protocol output on stdout, malformed JSON Lines, timeout, or oversized response |

## 14. AI-assisted plugin development

Give an AI this context before asking it to write code:

```text
You are implementing an OSS Sync executable server plugin.
Use github.com/helantianshen/oss-sync/pkg/ossplugin.
The plugin is trusted server code, not a sandbox.
Do not invent SDK methods or manifest fields. Verify them in pkg/ossplugin/sdk.go,
internal/serverplugin/package.go, internal/serverplugin/registration.go, and docs.
Use plugin-owned migrations/tables for plugin data.
Use Services().PutFile for file writes; never update core file rows or blobs directly.
For cookie-authenticated browser writes, send X-CSRF-Token matching oss_csrf.
AdminPage callbacks should return an HTML fragment unless they intentionally return
<!doctype html> or <html> as a complete document.
Keep stdout reserved for the plugin protocol; log only to stderr.
Before coding, state the manifest, runtime registration, callbacks, routes, data model,
authorization, and tests. Implement the smallest end-to-end slice first.
```

Require the AI to produce:

1. package tree;
2. manifest and runtime registration;
3. callback-to-route table;
4. request and response JSON schemas;
5. migration SQL and ownership rules;
6. input validation and threat model;
7. platform build/package commands;
8. deterministic tests for auth, no-op writes, retries, and upgrades.

Reject answers that invent APIs, confuse route namespaces, treat executable plugins as sandboxed, or recommend direct writes to core file rows.

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
| `.MetaDescription` | Current page description: article summary (or article title when empty); blog introduction on home/folder pages, falling back to page title. Use this in `<meta name="description">` |
| `.LogoURL`, `.LogoSize`, `.LogoShape` | Logo URL, pixel size, square/circle |
| `.BannerURL`, `.MobileBannerURL` | Desktop and mobile banner URLs |
| `.Buttons` | Links with `.Label`, `.URL`, `.IconURL`, `.Position` |
| `.CustomHeader`, `.CustomFooter`, `.FooterNotice` | Custom fragments and notice; custom fragments are policy-controlled and sanitized |
| `.ThemeConfigJS` | Safe JSON for `<script>window.config = {{.ThemeConfigJS}};</script>`; there is no `.ThemeConfig` field |
| `.PluginData` | Plugin display data; read via `pluginField`, `hasPlugin`, or guarded `with index`; use `safeHTML` only for trusted, sanitized HTML |

Frontmatter starts at the beginning of the document and ends with a standalone `---` line. Fields are `title`, `description` (fallback `summary`), `published` (fallback `date`), `category`, `tags`, and `image` (fallback `cover`). Primary keys take precedence. YAML literal/folded multiline descriptions, quoted strings, tag lists and comma-separated tag text are supported. Valid frontmatter is hidden; malformed/unclosed blocks remain visible. Blank or missing titles use the filename. Blank or missing summaries are extracted from body text up to 120 characters (plus an ellipsis when truncated), skipping headings, standalone section-navigation links, images, code-only blocks, tables and HTML blocks. Normal link labels and inline code within prose are retained without Markdown formatting. Dates default to file update time. This affects rendered metadata only; source notes are unchanged.

`./` and `../` attachment references resolve only relative to the article and cannot escape the Vault. Other paths prefer an exact Vault-root match, then an article-relative match; bare names finally match same-Vault attachment basenames, preferring the latest mtime. Use `./` to avoid root-name collisions. Markdown image and frontmatter cover URLs support encoded paths, query parameters and fragments: only the decoded path selects the attachment, while the fragment remains on the generated URL. Wiki embeds use literal filenames, so percent signs and hash characters do not become URL escapes or fragments. Remote HTTP(S) images/covers retain their URL; covers starting with `/` are site URLs. Folder asset URLs carry a `source` article path, checked against share scope, live article existence and its actual references.

### Diagnostics and acceptance

Templates use Go `html/template` escaping and `missingkey=zero`. Missing map keys yield zero values; unknown struct fields, unguarded nil chains and syntax errors still fail. Guard optional data with `with`. Failures fall back to `default` and expose the reason in `X-Theme-Fallback`. Default has no homepage post list, so HTTP 200 alone does not prove successful custom rendering.

Verify homepage, individual article, folder index and folder article separately. Check template markers, fallback headers, CSS/JS/images, copy-disabled states, empty and over-100-post lists, mobile layout and keyboard behavior. Existing templates receive the complete HomePosts list; large collections create longer pages.
