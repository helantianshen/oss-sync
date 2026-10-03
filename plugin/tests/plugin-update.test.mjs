import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { loadUpdateModule } from "./helpers/plugin-update-loader.mjs";

function encode(text) {
  return new TextEncoder().encode(text).buffer;
}

function decode(buffer) {
  return new TextDecoder().decode(buffer);
}

function manifestFile(version) {
  return encode(JSON.stringify({ id: "oss-sync", version }));
}

function digest(text) {
  return `sha256:${createHash("sha256").update(text).digest("hex")}`;
}

function makeFakeAdapter(initial = {}) {
  const files = new Map();
  for (const [path, content] of Object.entries(initial)) files.set(path, content);
  const ops = [];
  const adapter = {
    files,
    ops,
    writeFailFor: new Set(),
    renameFailFor: new Set(),
    async read(path) {
      ops.push(["read", path]);
      return files.has(path) ? files.get(path) : null;
    },
    async write(path, data) {
      ops.push(["write", path]);
      if (adapter.writeFailFor.has(path)) throw new Error(`write failed: ${path}`);
      // Obsidian 的 adapter.write 只接受字符串，传入其他类型必须抛错，与真实实现一致
      if (typeof data !== "string") {
        throw new TypeError(
          'The "data" argument must be of type string or an instance of Buffer, TypedArray, or DataView. Received an instance of ArrayBuffer'
        );
      }
      files.set(path, data);
    },
    async rename(oldPath, newPath) {
      ops.push(["rename", oldPath, newPath]);
      if (adapter.renameFailFor.has(oldPath)) throw new Error(`rename failed: ${oldPath}`);
      files.set(newPath, files.get(oldPath));
      files.delete(oldPath);
    },
    async remove(path) {
      ops.push(["remove", path]);
      files.delete(path);
    },
  };
  return adapter;
}

function makeReloadController(getManifestVersion) {
  const state = { loaded: false, version: null, disabledCount: 0, enabledCount: 0, failNextEnable: false };
  return {
    state,
    async disablePlugin() {
      state.disabledCount++;
      state.loaded = false;
    },
    async enablePlugin() {
      state.enabledCount++;
      if (state.failNextEnable) {
        state.failNextEnable = false;
        throw new Error("enable failed");
      }
      state.loaded = true;
      state.version = getManifestVersion();
    },
    isLoaded(id, expectedVersion) {
      return state.loaded && (expectedVersion == null || state.version === expectedVersion);
    },
  };
}

function updateFiles() {
  return [
    { name: "main.js", content: encode("new-main") },
    { name: "manifest.json", content: manifestFile("0.2.0") },
    { name: "styles.css", content: encode("new-css") },
  ];
}

const ORIGINAL_FILES = {
  "dir/main.js": "old-main",
  "dir/manifest.json": '{"id":"oss-sync","version":"0.1.0"}',
  "dir/styles.css": "old-css",
};

// 版本比较

test("compareVersions orders semver core versions", async () => {
  const { compareVersions, cleanup } = await loadUpdateModule("src/plugin-update.ts");
  try {
    assert.equal(compareVersions("0.1.0", "0.1.0"), 0);
    assert.equal(compareVersions("0.1", "0.1.0"), 0);
    assert.equal(compareVersions("0.1.0", "0.1.1"), -1);
    assert.equal(compareVersions("0.1.9", "0.2.0"), -1);
    assert.equal(compareVersions("1.0.0", "0.9.9"), 1);
    assert.equal(compareVersions("0.10.0", "0.9.9"), 1);
  } finally {
    await cleanup();
  }
});

test("compareVersions orders pre-release identifiers below releases", async () => {
  const { compareVersions, cleanup } = await loadUpdateModule("src/plugin-update.ts");
  try {
    assert.equal(compareVersions("0.2.0-beta.1", "0.2.0"), -1);
    assert.equal(compareVersions("0.2.0", "0.2.0-beta.1"), 1);
    assert.equal(compareVersions("0.2.0-alpha", "0.2.0-beta"), -1);
    assert.equal(compareVersions("0.2.0-beta.10", "0.2.0-beta.2"), 1);
    assert.equal(compareVersions("0.2.0-beta.1", "0.2.0-beta.1"), 0);
  } finally {
    await cleanup();
  }
});

test("parseVersionTag strips the v/V prefix", async () => {
  const { parseVersionTag, cleanup } = await loadUpdateModule("src/plugin-update.ts");
  try {
    assert.equal(parseVersionTag("v1.2.3"), "1.2.3");
    assert.equal(parseVersionTag("V0.1.0"), "0.1.0");
    assert.equal(parseVersionTag("1.2.3"), "1.2.3");
  } finally {
    await cleanup();
  }
});

test("manifestVersionFromText parses the version field and rejects garbage", async () => {
  const { manifestVersionFromText, cleanup } = await loadUpdateModule("src/plugin-update.ts");
  try {
    assert.equal(manifestVersionFromText('{"id":"oss-sync","version":"0.2.0"}'), "0.2.0");
    assert.equal(manifestVersionFromText('{"version": 5}'), null);
    assert.equal(manifestVersionFromText("not json"), null);
    assert.equal(manifestVersionFromText(null), null);
  } finally {
    await cleanup();
  }
});

// GitHub Release 查询与下载

function releaseResponse({ tag = "v0.2.0", missingAssets = [] } = {}) {
  const assets = ["main.js", "manifest.json", "styles.css"]
    .filter((name) => !missingAssets.includes(name))
    .map((name) => ({
      name,
      browser_download_url: `https://github.com/owner/repo/releases/download/${tag}/${name}`,
      size: Buffer.byteLength(`content-of-${name}`),
      digest: digest(`content-of-${name}`),
    }));
  return {
    status: 200,
    json: {
      tag_name: tag,
      html_url: `https://github.com/owner/repo/releases/tag/${tag}`,
      assets,
    },
    text: "",
    arrayBuffer: new ArrayBuffer(0),
    headers: {},
  };
}

function makeFetch(responder) {
  const calls = [];
  const fetchImpl = async (options) => {
    calls.push(options.url);
    return responder(options.url, options);
  };
  return { calls, fetchImpl };
}

test("checkForUpdates reads the remote version from the release manifest", async () => {
  const { checkForUpdates, isUpdateAvailable, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    const { fetchImpl } = makeFetch((url) => {
      if (url.endsWith("/releases/latest")) return releaseResponse();
      if (url.endsWith("/manifest.json")) {
        return { status: 200, json: {}, text: '{"version":"0.2.0"}', arrayBuffer: new ArrayBuffer(0), headers: {} };
      }
      throw new Error(`unexpected request ${url}`);
    });
    const source = new GitHubReleaseSource(fetchImpl);

    // 本地 0.1.0，远端 manifest.json 声明 0.2.0
    const result = await checkForUpdates("owner/repo", "0.1.0", source);

    assert.equal(result.remoteVersion, "0.2.0");
    assert.equal(result.currentVersion, "0.1.0");
    assert.equal(isUpdateAvailable(result), true);
  } finally {
    await cleanup();
  }
});

test("checkForUpdates falls back to the release tag when the manifest asset is missing", async () => {
  const { checkForUpdates, isUpdateAvailable, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    const { fetchImpl } = makeFetch((url) => {
      if (url.endsWith("/releases/latest")) return releaseResponse({ missingAssets: ["manifest.json"] });
      throw new Error(`unexpected request ${url}`);
    });
    const source = new GitHubReleaseSource(fetchImpl);

    const result = await checkForUpdates("owner/repo", "0.1.0", source);

    assert.equal(result.remoteVersion, "0.2.0");
    assert.equal(isUpdateAvailable(result), true);
  } finally {
    await cleanup();
  }
});

test("checkForUpdates reports no update when remote is not newer", async () => {
  const { checkForUpdates, isUpdateAvailable, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    const { fetchImpl } = makeFetch((url) => {
      if (url.endsWith("/releases/latest")) return releaseResponse({ tag: "v0.1.0" });
      if (url.endsWith("/manifest.json")) {
        return { status: 200, json: {}, text: '{"version":"0.1.0"}', arrayBuffer: new ArrayBuffer(0), headers: {} };
      }
      throw new Error(`unexpected request ${url}`);
    });
    const source = new GitHubReleaseSource(fetchImpl);

    const result = await checkForUpdates("owner/repo", "0.1.0", source);

    assert.equal(result.remoteVersion, "0.1.0");
    assert.equal(isUpdateAvailable(result), false);
  } finally {
    await cleanup();
  }
});

test("latestRelease rejects invalid repositories and missing releases", async () => {
  const { GitHubReleaseSource, cleanup } = await loadUpdateModule("src/plugin-update.ts");
  try {
    const { fetchImpl } = makeFetch(() => ({ status: 404, json: {}, text: "", arrayBuffer: new ArrayBuffer(0), headers: {} }));
    const source = new GitHubReleaseSource(fetchImpl);

    await assert.rejects(source.latestRelease("not-a-repo"), /invalid GitHub repository/);
    await assert.rejects(source.latestRelease("owner/repo"), /no GitHub releases/);
  } finally {
    await cleanup();
  }
});

test("downloadUpdateAssets downloads the release trio and rejects missing assets", async () => {
  const { downloadUpdateAssets, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    const { calls, fetchImpl } = makeFetch((url) => {
      if (url.endsWith("/releases/latest")) return releaseResponse();
      return {
        status: 200,
        json: {},
        text: "",
        arrayBuffer: encode(`content-of-${url.split("/").pop()}`),
        headers: {},
      };
    });
    const source = new GitHubReleaseSource(fetchImpl);
    const release = await source.latestRelease("owner/repo");

    const files = await downloadUpdateAssets(source, release);

    assert.deepEqual(files.map((file) => file.name), ["main.js", "manifest.json", "styles.css"]);
    assert.equal(decode(files[0].content), "content-of-main.js");
    assert.equal(decode(files[2].content), "content-of-styles.css");
    assert.equal(calls.length, 4);

    const missing = releaseResponse({ missingAssets: ["main.js"] });
    await assert.rejects(
      downloadUpdateAssets(new GitHubReleaseSource(makeFetch(() => missing).fetchImpl), missing.json),
      /main\.js/
    );
  } finally {
    await cleanup();
  }
});

test("downloadUpdateAssets rejects size and SHA-256 mismatches", async () => {
  const { downloadUpdateAssets, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    for (const mutation of [
      (asset) => { asset.size += 1; },
      (asset) => { asset.digest = `sha256:${"0".repeat(64)}`; },
    ]) {
      const response = releaseResponse();
      mutation(response.json.assets[0]);
      const { fetchImpl } = makeFetch((url) => {
        if (url.endsWith("/releases/latest")) return response;
        return {
          status: 200,
          json: {},
          text: "",
          arrayBuffer: encode(`content-of-${url.split("/").pop()}`),
          headers: {},
        };
      });
      const source = new GitHubReleaseSource(fetchImpl);
      const release = await source.latestRelease("owner/repo");
      await assert.rejects(downloadUpdateAssets(source, release), /mismatch/);
    }
  } finally {
    await cleanup();
  }
});

test("downloadUpdateAssets rejects non-GitHub asset URLs", async () => {
  const { downloadUpdateAssets, GitHubReleaseSource, cleanup } =
    await loadUpdateModule("src/plugin-update.ts");
  try {
    const response = releaseResponse();
    response.json.assets[0].browser_download_url = "https://example.com/main.js";
    const source = new GitHubReleaseSource(makeFetch(() => response).fetchImpl);
    await assert.rejects(
      downloadUpdateAssets(source, response.json),
      /invalid download URL/,
    );
  } finally {
    await cleanup();
  }
});

// 临时文件写入必须适配 Obsidian 的 adapter.write 字符串契约
// 直接传入 ArrayBuffer 会在 Electron 侧抛 "data argument must be of type string"
test("applyPluginUpdate writes decoded strings instead of ArrayBuffer", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    const reload = makeReloadController(() => "0.2.0");
    const written = [];
    const originalWrite = adapter.write.bind(adapter);
    adapter.write = async (path, data) => {
      written.push({ path, type: typeof data });
      return originalWrite(path, data);
    };

    await applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() });

    assert.ok(written.length > 0, "applyPluginUpdate must call adapter.write");
    for (const entry of written) {
      assert.equal(entry.type, "string", `${entry.path} must be written as a string`);
    }
  } finally {
    await cleanup();
  }
});

// 原子替换与重载

test("applyPluginUpdate atomically replaces files and reloads the plugin", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    const reload = makeReloadController(() => {
      const manifest = adapter.files.get("dir/manifest.json");
      return manifest ? JSON.parse(manifest).version : null;
    });

    await applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() });

    assert.equal(adapter.files.get("dir/main.js"), "new-main");
    assert.equal(adapter.files.get("dir/manifest.json"), JSON.stringify({ id: "oss-sync", version: "0.2.0" }));
    assert.equal(adapter.files.get("dir/styles.css"), "new-css");
    assert.ok(![...adapter.files.keys()].some((path) => path.includes(".oss-update-tmp")));
    assert.equal(reload.state.disabledCount, 1);
    assert.equal(reload.state.enabledCount, 1);
    const renames = adapter.ops.filter(([op]) => op === "rename");
    assert.ok(renames.every(([, from, to]) => from.includes(".oss-update-tmp") && !to.includes(".oss-update-tmp")));
    assert.equal(renames.length, 3);
  } finally {
    await cleanup();
  }
});

test("applyPluginUpdate rolls back files when enablePlugin rejects", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    const reload = makeReloadController(() => {
      const manifest = adapter.files.get("dir/manifest.json");
      return manifest ? JSON.parse(manifest).version : null;
    });

    // 下一次启用失败，触发回滚
    reload.state.failNextEnable = true;
    await assert.rejects(applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() }), /enable failed/);

    assert.equal(adapter.files.get("dir/main.js"), "old-main");
    assert.equal(adapter.files.get("dir/manifest.json"), '{"id":"oss-sync","version":"0.1.0"}');
    assert.equal(adapter.files.get("dir/styles.css"), "old-css");
    assert.ok(reload.state.enabledCount >= 2);
    assert.equal(reload.state.version, "0.1.0");
  } finally {
    await cleanup();
  }
});

test("applyPluginUpdate rolls back files when the new instance is not loaded", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    // isLoaded 永远不满足预期版本，模拟新代码加载后仍是旧实例
    const reload = makeReloadController(() => "0.1.0");

    await assert.rejects(
      applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() }),
      /reload failed/
    );

    assert.equal(adapter.files.get("dir/main.js"), "old-main");
    assert.equal(adapter.files.get("dir/manifest.json"), '{"id":"oss-sync","version":"0.1.0"}');
    assert.ok(reload.state.enabledCount >= 2);
  } finally {
    await cleanup();
  }
});

test("applyPluginUpdate leaves existing files untouched when a temp write fails", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    const reload = makeReloadController(() => "0.2.0");
    adapter.writeFailFor.add("dir/styles.css.oss-update-tmp");

    await assert.rejects(
      applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() }),
      /write failed/
    );

    assert.equal(adapter.files.get("dir/main.js"), "old-main");
    assert.equal(adapter.files.get("dir/manifest.json"), '{"id":"oss-sync","version":"0.1.0"}');
    assert.ok(![...adapter.files.keys()].some((path) => path.includes(".oss-update-tmp")));
    assert.equal(reload.state.enabledCount, 0);
  } finally {
    await cleanup();
  }
});

test("applyPluginUpdate restores earlier targets when a rename fails midway", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    const adapter = makeFakeAdapter(ORIGINAL_FILES);
    const reload = makeReloadController(() => "0.2.0");
    adapter.renameFailFor.add("dir/manifest.json.oss-update-tmp");

    await assert.rejects(
      applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() }),
      /rename failed/
    );

    // main.js 已被 rename，需要恢复；styles.css 尚未 rename，保持原样
    assert.equal(adapter.files.get("dir/main.js"), "old-main");
    assert.equal(adapter.files.get("dir/manifest.json"), '{"id":"oss-sync","version":"0.1.0"}');
    assert.equal(adapter.files.get("dir/styles.css"), "old-css");
    assert.ok(![...adapter.files.keys()].some((path) => path.includes(".oss-update-tmp")));
    assert.equal(reload.state.enabledCount, 0);
  } finally {
    await cleanup();
  }
});

test("applyPluginUpdate handles missing originals when creating the plugin directory", async () => {
  const { applyPluginUpdate, cleanup } = await loadUpdateModule("src/plugin-update-apply.ts");
  try {
    // 插件目录为空，三件套尚不存在
    const adapter = makeFakeAdapter({});
    const reload = makeReloadController(() => "0.2.0");

    await applyPluginUpdate({ adapter, reload, dir: "dir", pluginID: "oss-sync", files: updateFiles() });

    assert.equal(adapter.files.get("dir/main.js"), "new-main");
    assert.equal(adapter.files.get("dir/manifest.json"), JSON.stringify({ id: "oss-sync", version: "0.2.0" }));
    assert.equal(adapter.files.get("dir/styles.css"), "new-css");
    assert.equal(reload.state.enabledCount, 1);
  } finally {
    await cleanup();
  }
});
