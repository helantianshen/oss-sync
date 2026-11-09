import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";
import { build } from "esbuild";

async function load(t) {
  const dir = await mkdtemp(join(tmpdir(), "oss-mobile-diagnostics-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const outfile = join(dir, "test.mjs");
  await build({
    stdin: { contents: ["diagnostic-report", "diagnostics", "blacklist", "http-transport", "api", "sync-engine", "settings-tab", "markdown-delta", "ordinary-sync-file-access"].map((name) => `export * from './src/${name}.ts';`).join("\n") + '\nexport {default as OSSPlugin} from "./src/main.ts";export {SidebarView} from "./src/sidebar-view.ts";\nexport {host,Platform} from "obsidian";', resolveDir: fileURLToPath(new URL("../", import.meta.url)) },
    outfile, bundle: true, platform: "node", format: "esm",
    plugins: [{ name: "mobile-host", setup(builder) {
      builder.onResolve({ filter: /^obsidian$/ }, () => ({ path: "obsidian", namespace: "host" }));
      builder.onLoad({ filter: /.*/, namespace: "host" }, () => ({ contents: `
        export const host={request:null}; export const Platform={isDesktop:false,isMobile:true};
        export function requestUrl(input){return host.request(input);}
        export class Notice{constructor(message){host.notices??=[];host.notices.push(message);}} export class App{} export class TAbstractFile{} export class TFile{} export class TFolder{} export class Vault{}
        export class Modal{} export class Setting{} export class ButtonComponent{} export class ItemView{} export class MarkdownView{} export class Plugin{} export class PluginSettingTab{}
        export const normalizePath=p=>p; export function setIcon(){} export function setTooltip(){} export const apiVersion="1.14.5";
        export function requireApiVersion(){return true;}
        export const editorInfoField = undefined;
        export function getLanguage(){return "zh";} export class MarkdownRenderer{}
      ` }));
    } }],
  });
  return import(pathToFileURL(outfile).href);
}

test("settings can be indexed before rendering and the indexed sync control still saves changes", async (t) => {
  const { OSSSettingTab } = await load(t);
  const settings = { deviceName: "", username: "tester", syncIntervalSec: 3 };
  const operations = [];
  let loggedIn = false;
  const tab = new OSSSettingTab({}, {
    t: (key) => key,
    settings,
    manifest: { version: "0.3.4" },
    isLoggedIn: () => loggedIn,
    isAdmin: () => false,
    saveSettings: async () => { operations.push("save"); },
    syncEngine: { resetDebounce: () => { operations.push("reset"); } },
    get api() { throw new Error("Indexing must not start an API request"); },
  });
  const indexedRows = () => tab.getSettingDefinitions()
    .flatMap((section) => section.items)
    .filter((row) => row.searchable !== false);
  assert.deepEqual(indexedRows().map((row) => row.name), ["settings.language.name", "settings.devices.setupName"]);

  settings.deviceName = "Test device";
  assert.ok(indexedRows().some((row) => row.name === "settings.server.login"));
  loggedIn = true;
  const rows = indexedRows();
  assert.ok(rows.some((row) => row.name === "settings.server.signedIn"));
  assert.ok(!rows.some((row) => row.name === "settings.server.login"));
  assert.ok(!rows.some((row) => row.name === "settings.serverUpdate.check"));

  const interval = rows.find((row) => row.name === "settings.sync.interval");
  assert.equal(interval.desc, "settings.sync.intervalDesc");
  let change;
  interval.render({
    addText(render) {
      render({
        setPlaceholder() { return this; },
        setValue(value) { assert.equal(value, "3"); return this; },
        onChange(callback) { change = callback; },
      });
    },
  });
  await change("0");
  assert.deepEqual(operations, []);
  await change("7");
  assert.equal(settings.syncIntervalSec, 7);
  assert.deepEqual(operations, ["save", "reset"]);
});

test("mobile file downloads skip the native JSON getter and preserve every byte", async (t) => {
  const m = await load(t);
  const diagnostics = new m.Diagnostics();
  m.setTransportDiagnosticSink((stage, details) => diagnostics.record({ kind: "detail", at: 1, area: "transport", stage, details }));
  const error = new SyntaxError('Unexpected token PRIVATE-NOTE-BODY');
  error.stack = 'SyntaxError: PRIVATE-NOTE-BODY\nPRIVATE-NOTE-BODY\n    at decode (app://obsidian.md/app.js:1:20)';
  const bytes = new TextEncoder().encode("# PRIVATE-NOTE-BODY\n路线测试\n");
  let jsonReads = 0;
  m.host.request = async () => ({ status: 200, arrayBuffer: bytes.buffer, text: new TextDecoder().decode(bytes), headers: { "Content-Type": "application/octet-stream", "Content-Length": String(bytes.byteLength), "X-OSS-Hash": "hash", "Set-Cookie": "SECRET-COOKIE" }, get json() { jsonReads++; throw error; } });
  const api = new m.OSSApiClient({ serverUrl: "https://example.invalid", clientId: "secret-device" }, diagnostics);
  api.setToken("SECRET-TOKEN");
  const downloaded = await api.downloadV2("secret-vault", "notes/手机.md", 17);
  assert.deepEqual(new Uint8Array(downloaded.content), bytes);
  assert.equal(jsonReads, 0);
  const response = diagnostics.snapshot().find((event) => event.stage === "response_received");
  assert.equal(response.details.status, 200);
  assert.equal(response.details.bytes, bytes.byteLength);
  assert.equal(response.details.contentType, "application/octet-stream");
  assert.equal(response.details.route, "/api/vaults/:vault/sync/download");
  assert.equal(response.details.path, "notes/手机.md");
  assert.equal(typeof response.details.requestId, "number");
  assert.equal(diagnostics.failureSnapshot().length, 0);
  assert.doesNotMatch(JSON.stringify(diagnostics.snapshot()), /PRIVATE-NOTE-BODY|SECRET-TOKEN|SECRET-COOKIE|secret-vault/);
  diagnostics.record({ kind: "api_failed", at: 2, method: "GET", path: "/api/test", message: "Unexpected token PRIVATE-NOTE-BODY is not valid JSON" });
  assert.doesNotMatch(JSON.stringify(diagnostics.snapshot()), /PRIVATE-NOTE-BODY/);
});

test("large Markdown uploads and downloads exchange small deltas and verify reconstructed bytes", async (t) => {
  const m = await load(t);
  const base = Buffer.alloc(10 * 1024 * 1024, 97);
  for (let offset = 0; offset < base.length; offset += 4096) {
    base.write(`## Paragraph ${offset}\r\n`, offset);
  }
  const target = Buffer.concat([
    base.subarray(0, 1024 * 1024 + 11), Buffer.from("新增一行 📝\r\n"),
    base.subarray(1024 * 1024 + 11, 8 * 1024 * 1024), base.subarray(8 * 1024 * 1024 + 3),
  ]);
  const hash = (body) => createHash("sha256").update(body).digest("hex");
  const expected = { size: target.length, hash: hash(target) };
  const path = "notes/large.md";
  const signature = await m.markdownSignature(base);
  const patch = await m.buildMarkdownDelta(signature, target);
  assert.ok(patch && patch.byteLength + signature.byteLength < target.length / 100);
  const diagnostics = new m.Diagnostics();
  const api = new m.OSSApiClient({ serverUrl: "https://example.invalid", clientId: "test-device", deviceName: "test" }, diagnostics);
  const binary = (body, headers) => ({ status: 200, arrayBuffer: body, headers, text: "" });
  const jsonResponse = (value) => ({
    ...binary(new ArrayBuffer(0), { "Content-Type": "application/json" }), text: JSON.stringify(value),
  });
  let newServer = true;
  let corruptDownload = false;
  let rejectUploadPatch = false;
  const fullBody = Uint8Array.from(target).buffer;
  const calls = [];
  m.host.request = async (request) => {
    const route = new URL(request.url).pathname.split("/").at(-1);
    const type = request.headers["Content-Type"];
    calls.push([route, type]);
    switch (route) {
      case "manifest":
        return jsonResponse({ server_time: Date.now(), ...(newServer ? { delta_v1: true } : {}) });
      case "delta-base":
        return binary(signature, { "Content-Type": m.DELTA_SIGNATURE_TYPE, "X-OSS-Hash": hash(base), "X-OSS-Revision": "1" });
      case "upload":
        if (type === m.DELTA_CONTENT_TYPE) {
          assert.deepEqual(Buffer.from(await m.applyMarkdownDelta(base, request.body, expected)), target);
          if (rejectUploadPatch) return { ...jsonResponse({ code: "delta_invalid" }), status: 422 };
        } else {
          assert.equal(type, "application/octet-stream");
          assert.deepEqual(Buffer.from(request.body), target);
        }
        return jsonResponse({ revision: 2, hash: expected.hash });
      case "download-delta": {
        assert.deepEqual(Buffer.from(request.body), Buffer.from(signature));
        const received = patch.slice(0);
        if (corruptDownload) new Uint8Array(received)[received.byteLength - 1] ^= 1;
        return binary(received, { "Content-Type": m.DELTA_CONTENT_TYPE, "X-OSS-Hash": expected.hash, "X-OSS-Revision": "2" });
      }
      case "download":
        return binary(fullBody, { "X-OSS-Hash": expected.hash, "X-OSS-Revision": "2" });
      default:
        throw new Error(`Unexpected route ${route}`);
    }
  };
  await api.manifest("vault");
  const upload = { path, baseRevision: 1, deltaBaseHash: hash(base), hash: expected.hash, content: fullBody, operationID: "test", mtime: 1 };
  const uploaded = await api.uploadV2("vault", upload);
  assert.equal(uploaded.revision, 2);
  assert.equal(uploaded.usedDelta, true);
  const download = async () => api.downloadV2("vault", path, 2, { ...expected, readBase: async () => base });
  const downloaded = await download();
  assert.deepEqual(Buffer.from(downloaded.content), target);
  assert.equal(downloaded.usedDelta, true);
  assert.deepEqual(calls.map(([route]) => route), ["manifest", "delta-base", "upload", "download-delta"]);
  corruptDownload = true;
  const fallback = await download();
  assert.deepEqual(Buffer.from(fallback.content), target);
  assert.equal(fallback.usedDelta, undefined);
  assert.deepEqual(calls.slice(-2).map(([route]) => route), ["download-delta", "download"]);
  rejectUploadPatch = true;
  assert.equal((await api.uploadV2("vault", upload)).usedDelta, undefined);
  assert.deepEqual(calls.slice(-2), [["upload", m.DELTA_CONTENT_TYPE], ["upload", "application/octet-stream"]]);
  newServer = false;
  await api.manifest("vault");
  assert.equal((await api.uploadV2("vault", upload)).usedDelta, undefined);
  assert.deepEqual(calls.slice(-2).map(([route]) => route), ["manifest", "upload"]);
  const transfers = diagnostics.snapshot().filter((event) => event.kind === "transfer");
  assert.deepEqual(transfers.map(({ scope, mode }) => [scope, mode]), [
    ["upload", "delta"], ["download", "delta"], ["download", "full"], ["upload", "full"], ["upload", "full"],
  ]);
  for (const event of transfers.slice(0, 4)) {
    assert.equal(event.path, path);
    assert.equal(event.fullBytes, target.length);
    assert.equal(event.patchBytes, patch.byteLength);
    assert.equal(event.signatureBytes, signature.byteLength);
    assert.equal(event.bytes, patch.byteLength + (event.mode === "full" ? target.length : 0));
  }
  assert.equal(transfers[4].bytes, target.length);
  assert.equal(transfers[4].patchBytes, undefined);
  assert.equal(transfers[4].signatureBytes, undefined);
});

test("sync notices distinguish actual deltas, full transfers, and skipped paths", async (t) => {
  const { SyncEngine } = await load(t);
  const notices = [];
  const report = (transfers) => SyncEngine.prototype.reportTransferNotifications.call(
    { showTransferNotice: (notice) => notices.push(notice) },
    transfers.map(([path, kind]) => ({ path, kind })),
    transfers.map(([, , transfer]) => ({ ok: true, result: { kind: "resolved", transfer } })),
  );
  report([
    ["edited-local.md", "upload", "delta"],
    ["edited-remote.md", "download", "delta"],
    ["new.md", "download", "full"],
    ["skipped.md", "download"],
  ]);
  assert.deepEqual(notices.splice(0), [
    { kind: "downloaded", path: "new.md" },
    { kind: "deltaSynced", path: "edited-local.md" },
    { kind: "deltaSynced", path: "edited-remote.md" },
  ]);
  report(Array.from({ length: 6 }, (_, index) => [`note-${index}.md`, "download", "delta"]));
  assert.deepEqual(notices, [{ kind: "deltaSynced", count: 6 }]);
});

test("path rules preserve hard exclusions and portable configuration uses the hidden directory adapter", async (t) => {
  const m = await load(t);
  const configDir = ".custom-config";
  const rules = { syncConfiguration: true, syncExclusions: "Private/\n**/*.pdf", syncWhitelist: "Private/Shared/\n**/allowed.pdf\n.custom-config/plugins/**" };
  const allowed = m.buildSyncPathFilter(configDir, rules);
  assert.equal(allowed("Private/hidden.md"), false);
  assert.equal(allowed("Private/Shared/note.md"), true);
  assert.equal(allowed("folder/document.pdf"), false);
  assert.equal(allowed("allowed.pdf"), true);
  assert.equal(allowed(`${configDir}/appearance.json`), true);
  assert.equal(allowed(`${configDir}/themes/custom/theme.css`), true);
  assert.equal(allowed(`${configDir}/plugins/other/data.json`), false);
  assert.equal(allowed(`${configDir}/plugins/other/main.js`), false);
  assert.equal(allowed(`${configDir}/workspace-mobile.json`), false);
  assert.equal(m.buildSyncPathFilter(configDir, { ...rules, syncConfiguration: false })(`${configDir}/appearance.json`), false);

  const files = new Map();
  const folders = new Set([configDir]);
  const adapter = {
    stat: async (path) => folders.has(path) ? { type: "folder" } : files.has(path) ? { type: "file", mtime: 1 } : null,
    exists: async (path) => folders.has(path) || files.has(path),
    mkdir: async (path) => { folders.add(path); },
    readBinary: async (path) => files.get(path),
    writeBinary: async (path, value) => { files.set(path, value); },
    remove: async (path) => { files.delete(path); },
  };
  const access = new m.OrdinarySyncFileAccess({ configDir, adapter });
  const path = `${configDir}/snippets/style.css`;
  const bytes = new TextEncoder().encode(".example { color: red; }");
  const created = await access.create(path, { kind: "absent" }, bytes);
  assert.equal(created.kind, "created");
  assert.deepEqual((await access.readExact(path)).bytes, bytes);
  assert.equal((await access.replace(path, { kind: "hash", hash: "stale" }, new Uint8Array())).kind, "stale");
  assert.equal((await access.deleteExact(path, { kind: "hash", hash: created.snapshot.hash })).kind, "deleted");
  assert.equal(await access.readExact(path), null);
  assert.ok(folders.has(configDir));
});

test("the plugin export uses Obsidian Platform flags and retains the saved file if opening fails", async (t) => {
  const m = await load(t);
  const files = new Map();
  const diagnostics = new m.Diagnostics();
  const plugin = {
    diagnostics, manifest: { version: "0.3.1" }, token: "PRIVATE-TOKEN",
    settings: { serverUrl: "https://example.invalid", password: "PRIVATE-PASSWORD", diagnosticsEnabled: false },
    api: { hasToken: () => true },
    t: (key, values) => key + JSON.stringify(values ?? {}),
    emitRuntimeInfo: () => diagnostics.record({ kind: "runtime_info", at: 1, pluginVersion: "0.3.1", diagnosticsEnabled: false }),
    app: { vault: { getAbstractFileByPath: (path) => files.get(path), createFolder: async (path) => files.set(path, {}), create: async (path, content) => { const file = { path, content }; files.set(path, file); return file; } }, workspace: { getLeaf: () => ({ openFile: async () => { throw new Error("viewer unavailable"); } }) } },
  };
  const platforms = [
    [{ isMobile: true, isAndroidApp: true }, "mobile", "android"],
    [{ isMobile: true, isIosApp: true, isMacOS: true }, "mobile", "ios"],
    [{ isWin: true }, "desktop", "windows"],
    [{ isMacOS: true }, "desktop", "macos"],
    [{ isLinux: true }, "desktop", "linux"],
    [{ isMobile: true, isWin: true }, "mobile", "windows"],
    [{}, "desktop", "unknown"],
  ];
  for (const [flags, platform, operatingSystem] of platforms) {
    Object.assign(m.Platform, { isMobile: false, isAndroidApp: false, isIosApp: false, isWin: false, isMacOS: false, isLinux: false }, flags);
    const path = await m.OSSPlugin.prototype.exportDiagnosticLogs.call(plugin);
    const content = files.get(path).content;
    const report = JSON.parse(content.split("```json\n")[1].split("\n```")[0]);
    assert.equal(report.pluginID, "oss-sync");
    assert.equal(report.runtime.platform, platform);
    assert.equal(report.runtime.operatingSystem, operatingSystem);
    assert.equal(Object.hasOwn(report.runtime, "userAgent"), false);
    assert.doesNotMatch(content, /PRIVATE-TOKEN|PRIVATE-PASSWORD/);
  }
  assert.ok(m.host.notices.some((message) => message.includes("settings.diagnostics.saved")));
  assert.ok(m.host.notices.some((message) => message.includes("settings.diagnostics.openHint")));
});

test("turning diagnostics off stops collection while retaining earlier logs for export", async (t) => {
  const m = await load(t);
  let enabled = false;
  const diagnostics = new m.Diagnostics(undefined, 20, () => enabled);
  const event = { kind: "api", at: 1, method: "GET", durationMs: 1 };
  diagnostics.record(event);
  assert.equal(diagnostics.snapshot().length, 0);
  enabled = true;
  diagnostics.record(event);
  enabled = false;
  diagnostics.record(event);
  assert.equal(diagnostics.snapshot().length, 1);
  diagnostics.clear();
  assert.equal(diagnostics.snapshot().length, 0);
});

test("mobile transport preserves original network errors and ignores a failing diagnostic observer", async (t) => {
  const m = await load(t);
  m.setTransportDiagnosticSink(() => { throw new Error("observer failure"); });
  m.host.request = async () => ({ status: 200, json: { ok: true }, text: "{}", arrayBuffer: new ArrayBuffer(0), headers: {} });
  assert.equal((await m.httpRequest({ url: "https://example.invalid", method: "GET", headers: {} })).status, 200);
  const error = new Error("ERR_CONNECTION_TIMED_OUT");
  m.host.request = async () => { throw error; };
  await assert.rejects(m.httpRequest({ url: "https://example.invalid", method: "GET", headers: {} }), (actual) => actual === error);
});

test("JSON APIs, empty responses and HTML errors do not access the native JSON getter", async (t) => {
  const m = await load(t);
  for (const [status, text, expected] of [[200, '{"ok":true}', { ok: true }], [204, "", null], [502, "<html>gateway error</html>", null]]) {
    m.host.request = async () => ({ status, text, arrayBuffer: new TextEncoder().encode(text).buffer, headers: {}, get json() { throw new Error("native JSON getter must not run"); } });
    const response = await m.httpRequest({ url: "https://example.invalid/api", method: "GET", headers: {} });
    assert.deepEqual(response.json, expected);
    assert.equal(response.text, text);
    assert.equal(response.status, status);
  }
  const api = new m.OSSApiClient({ serverUrl: "https://example.invalid" });
  await assert.rejects(api.downloadV2("vault", "a.md", 1), (error) => error instanceof m.OSSApiError && error.status === 502);
});

test("arbitrary attachment bytes and JSON-looking note files remain byte-identical", async (t) => {
  const m = await load(t);
  const api = new m.OSSApiClient({ serverUrl: "https://example.invalid" });
  for (const bytes of [Uint8Array.from({ length: 4096 }, (_, index) => index % 256), new TextEncoder().encode('{"a":"this is a note, not API metadata"}'), new Uint8Array(0)]) {
    m.host.request = async () => ({ status: 200, text: new TextDecoder().decode(bytes), arrayBuffer: bytes.buffer, headers: { "content-type": "application/octet-stream" }, get json() { throw new SyntaxError("not JSON"); } });
    const result = await api.downloadV2("vault", "data.bin", 1);
    assert.deepEqual(new Uint8Array(result.content), bytes);
  }
});

test("a mobile download reaches local write and baseline acknowledgement after the parsing fix", async (t) => {
  const m = await load(t);
  const diagnostics = new m.Diagnostics();
  const bytes = new TextEncoder().encode("# route guide\n中文测试\n");
  m.host.request = async () => ({ status: 200, text: new TextDecoder().decode(bytes), arrayBuffer: bytes.buffer, headers: { "x-oss-revision": "17", "x-oss-hash": "hash" }, get json() { throw new SyntaxError("not JSON"); } });
  const writes = [], acknowledgements = [], removed = [];
  const remote = { path: "notes/mobile.md", size: bytes.length, revision: 17 };
  const engine = {
    diagnostics, api: new m.OSSApiClient({ serverUrl: "https://example.invalid" }, diagnostics),
    ordinaryFileAccess: { create: async (path, expected, received) => { writes.push({ path, expected, bytes: received }); return { kind: "written", snapshot: { hash: "hash" } }; } },
    liveAcknowledgement: async (input) => { acknowledgements.push(input); return { acknowledged: true }; },
    baseline: { set() {}, removePendingForPath: (path) => removed.push(path) },
  };
  const result = await m.SyncEngine.prototype.applyDownload.call(engine, "vault", remote, { kind: "absent" });
  assert.equal(result.kind, "resolved");
  assert.deepEqual(writes[0].bytes, bytes);
  assert.equal(acknowledgements[0].server.revision, 17);
  assert.deepEqual(removed, [remote.path]);
  assert.ok(diagnostics.snapshot().some((event) => event.stage === "written"));
  assert.equal(diagnostics.failureSnapshot().length, 0);
});

test("only explicit SQLite busy failures get two bounded retries for the same download", async (t) => {
  const m = await load(t);
  const previousWindow = globalThis.window;
  const delays = [];
  globalThis.window = { setTimeout: (callback, delay) => { delays.push(delay); queueMicrotask(callback); return 0; } };
  t.after(() => { if (previousWindow === undefined) delete globalThis.window; else globalThis.window = previousWindow; });
  const diagnostics = new m.Diagnostics();
  const requests = [];
  m.host.request = async (request) => {
    requests.push(request);
    const status = requests.length < 3 ? 500 : 200;
    const text = status === 500 ? '{"error":"database is locked (5) (SQLITE_BUSY)"}' : "# downloaded";
    return { status, text, arrayBuffer: new TextEncoder().encode(text).buffer, headers: {}, get json() { throw new SyntaxError("native getter"); } };
  };
  const api = new m.OSSApiClient({ serverUrl: "https://example.invalid" }, diagnostics);
  const result = await api.downloadV2("vault", "notes/a.md", 17);
  assert.equal(new TextDecoder().decode(result.content), "# downloaded");
  assert.deepEqual(delays, [250, 750]);
  assert.equal(new Set(requests.map((request) => request.url)).size, 1);
  assert.ok(requests.every((request) => request.method === "GET"));
  assert.equal(diagnostics.snapshot().filter((event) => event.stage === "database_busy_retry").length, 2);
  let calls = 0;
  m.host.request = async () => { calls++; return { status: 500, text: '{"error":"SQLITE_BUSY"}', arrayBuffer: new ArrayBuffer(0), headers: {} }; };
  await assert.rejects(api.downloadV2("vault", "notes/a.md", 17), (error) => error instanceof m.OSSApiError && error.status === 500 && error.message === "SQLITE_BUSY");
  assert.equal(calls, 3);
  for (const status of [401, 403, 404, 409, 500]) {
    calls = 0;
    m.host.request = async () => { calls++; return { status, text: '{"error":"other error"}', arrayBuffer: new ArrayBuffer(0), headers: {} }; };
    await assert.rejects(api.downloadV2("vault", "notes/a.md", 17), (error) => error instanceof m.OSSApiError && error.status === status);
    assert.equal(calls, 1);
  }
});

test("a local write failure is distinguished from download failure and is still propagated", async (t) => {
  const m = await load(t);
  const diagnostics = new m.Diagnostics();
  const error = new Error("EACCES permission denied");
  const remote = { path: "notes/手机.md", size: 64, revision: 5 };
  const engine = { diagnostics, isSyncPath: () => true, api: { downloadV2: async () => ({ content: new ArrayBuffer(64), meta: remote }) }, ordinaryFileAccess: { create: async () => { throw error; } } };
  engine.applyDownload = (...args) => m.SyncEngine.prototype.applyDownload.apply(engine, args);
  engine.handleActionFailure = (...args) => m.SyncEngine.prototype.handleActionFailure.apply(engine, args);
  const action = { kind: "download", path: remote.path, remote, expectedLocal: { kind: "absent" } };
  await assert.rejects(m.SyncEngine.prototype.applyAction.call(engine, "vault", action), (actual) => actual === error);
  assert.ok(diagnostics.snapshot().some((event) => event.stage === "write_started" && event.details.bytes === 64));
  assert.ok(diagnostics.failureSnapshot().some((event) => event.area === "sync_action" && event.details.errorMessage === error.message));
});

test("export redacts secrets, filters settings and survives an overflowing event buffer", async (t) => {
  const m = await load(t);
  const d = new m.Diagnostics(undefined, 2);
  d.record({ kind: "detail", at: 1, area: "sync_action", stage: "failed", details: { path: "notes/a.md", errorMessage: 'TOKEN-LITERAL password="PASSWORD-LITERAL" Bearer abcdef https://user:URL-PASSWORD@example.com/api?token=QUERY-SECRET#private', body: "PRIVATE-BODY" } });
  for (let at = 2; at < 5; at++) d.record({ kind: "api", at, method: "GET", durationMs: 1 });
  const report = JSON.parse(m.createDiagnosticReport({ pluginVersion: "0.3.1", obsidianVersion: "1.14.5", platform: "mobile", operatingSystem: "android", userAgent: "PRIVATE-UA", serverURL: "https://user:URL-PASSWORD@example.com/api?password=QUERY-SECRET", loggedIn: true, vaultBound: true, pendingCount: 2, settings: { maxConcurrency: 6, password: "PASSWORD-LITERAL", token: "TOKEN-LITERAL", username: "PRIVATE-USER" }, events: d.snapshot(), recentFailures: d.failureSnapshot() }, ["TOKEN-LITERAL", "PASSWORD-LITERAL"]));
  assert.equal(report.events.length, 2);
  assert.equal(report.recentFailures.length, 1);
  assert.equal(report.settings.maxConcurrency, 6);
  assert.equal(report.runtime.platform, "mobile");
  assert.doesNotMatch(JSON.stringify(report), /TOKEN-LITERAL|PASSWORD-LITERAL|URL-PASSWORD|QUERY-SECRET|PRIVATE-BODY|PRIVATE-USER|PRIVATE-UA|abcdef/);
  assert.equal(report.recentFailures[0].details.path, "notes/a.md");
  d.clear();
  assert.equal(d.failureSnapshot().length, 0);
});

test("export saves a readable Markdown file locally without overwriting or entering sync", async (t) => {
  const m = await load(t);
  const files = new Map();
  const vault = { getAbstractFileByPath: (path) => files.get(path), createFolder: async (path) => files.set(path, { folder: true }), create: async (path, content) => { assert.equal(files.has(path), false); const file = { path, content }; files.set(path, file); return file; } };
  const now = new Date("2026-11-01T00:00:00Z");
  const first = await m.saveDiagnosticReport(vault, '{"ok":true}', now);
  const second = await m.saveDiagnosticReport(vault, '{"ok":true}', now);
  assert.notEqual(first.path, second.path);
  assert.match(first.content, /```json\n\{"ok":true\}\n```/);
  assert.match(first.path, /\.md$/);
  assert.equal(m.shouldSync(first.path, ".obsidian"), false);
  assert.equal(m.shouldSync(m.DIAGNOSTIC_FOLDER, ".obsidian"), false);
  assert.equal(m.shouldSync("notes/a.md", ".obsidian"), true);
  assert.equal(m.shouldSync(m.DIAGNOSTIC_FOLDER + "-notes/a.md", ".obsidian"), true);
  await assert.rejects(m.saveDiagnosticReport({ ...vault, create: async () => { throw new Error("disk full"); } }, "{}"), /disk full/);
});


test("侧边栏关闭或重开后只访问工作区内的视图", async (t) => {
  const m = await load(t);
  let leaves = [];
  const plugin = { app: { workspace: { getLeavesOfType: () => leaves } } };
  const getView = Object.getOwnPropertyDescriptor(m.OSSPlugin.prototype, "sidebarView").get;
  const first = Object.create(m.SidebarView.prototype);
  const reopened = Object.create(m.SidebarView.prototype);
  assert.equal(getView.call(plugin), undefined);
  leaves = [{ view: first }];
  assert.equal(getView.call(plugin), first);
  leaves = [];
  assert.equal(getView.call(plugin), undefined);
  leaves = [{ view: {} }, { view: reopened }];
  assert.equal(getView.call(plugin), reopened);
});

test("卸载立即停止所有任务，保存尚未完成或失败都不阻止清理", async (t) => {
  const m = await load(t);
  const calls = [];
  const errors = [];
  t.mock.method(console, "error", (...args) => errors.push(args));
  const previousWindow = globalThis.window;
  globalThis.window = { clearInterval: (id) => calls.push(["clearInterval", id]) };
  t.after(() => {
    if (previousWindow === undefined) delete globalThis.window;
    else globalThis.window = previousWindow;
  });
  let rejectSave;
  const saving = new Promise((_, reject) => { rejectSave = reject; });
  const plugin = {
    loaded: true, pluginVersionCheckTimer: 42,
    syncEngine: {
      stop: () => calls.push("sync stopped"),
      flushPendingQueue: () => { calls.push("flush"); return saving; },
    },
    collabManager: { stop: () => calls.push("collaboration stopped") },
    serverUpdatePoller: { dispose: () => calls.push("updates stopped") },
  };
  assert.equal(m.OSSPlugin.prototype.onunload.call(plugin), undefined);
  assert.deepEqual(calls, [["clearInterval", 42], "sync stopped", "collaboration stopped", "updates stopped", "flush"]);
  assert.equal(plugin.loaded, false);
  assert.equal(plugin.pluginVersionCheckTimer, null);
  assert.equal(plugin.serverUpdatePoller, null);
  const error = new Error("save failed");
  rejectSave(error);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(errors.length, 1);
  assert.equal(errors[0][1], error);
});
