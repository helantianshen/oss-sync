import assert from "node:assert/strict";
import test from "node:test";
import { loadSource as load } from "./helpers/source-loader.mjs";

test("默认和自定义配置目录排除设备状态，保留正常同步文件", async (t) => {
  const { shouldSync, classifySyncPath } = await load(t, "blacklist");
  for (const dir of [".obsidian", ".custom", "settings/obsidian"]) {
    for (const file of ["workspace.json", "workspace-mobile.json", "cache/index", "plugins/oss-sync/data.json"]) {
      assert.equal(shouldSync(`${dir}/${file}`, dir), false);
    }
    assert.equal(shouldSync(`${dir}/app.json`, dir), true);
    assert.equal(shouldSync(`${dir}-other/workspace.json`, dir), true);
    assert.equal(classifySyncPath(`${dir}/app.json`, dir), "config");
    assert.equal(classifySyncPath(`${dir}-other/app.json`, dir), "attachment");
  }
  assert.equal(shouldSync("notes/example.md", ".custom"), true);
  assert.equal(classifySyncPath("notes/example.md", ".custom"), "markdown");
});

test("移动端正常请求可用，失败保留原错误且不降级到 Node", async (t) => {
  const { httpRequest, host } = await load(t, "http-transport");
  const request = { url: "https://example.invalid/api", method: "GET", headers: {} };
  host.request = async () => ({ status: 200, json: { ok: true }, text: '{"ok":true}', arrayBuffer: new ArrayBuffer(0), headers: {} });
  assert.equal((await httpRequest(request)).json.ok, true);
  for (const error of [new Error("ERR_TOO_MANY_REDIRECTS"), new SyntaxError("invalid JSON")]) {
    host.request = async () => { throw error; };
    await assert.rejects(httpRequest(request), (actual) => actual === error);
  }
});

test("确认弹窗等待选择，取消或关闭不允许继续执行", async (t) => {
  const { ConfirmModal, host } = await load(t, "confirm-modal");
  for (const action of ["confirm", "cancel", "close"]) {
    let completed = false;
    const result = ConfirmModal.ask({}, "删除？", "删除", "取消").then((value) => {
      completed = true;
      return value;
    });
    await Promise.resolve();
    assert.equal(completed, false);
    if (action === "close") host.modal.close();
    else host.modal.contentEl.buttons[action === "confirm" ? 1 : 0].click();
    assert.equal(await result, action === "confirm");
  }
});
test("third-party installation files require both opt-ins and cannot include this plugin or private data", async (t) => {
  const { buildSyncPathFilter } = await load(t, "blacklist");
  const rules = {
    syncConfiguration: true, syncCommunityPlugins: true, ownPluginDir: ".custom/plugins/local-oss",
    syncExclusions: ".custom/plugins/excluded", syncWhitelist: ".custom/plugins/oss-sync/**",
  };
  const allows = buildSyncPathFilter(".custom", rules);
  for (const file of ["manifest.json", "main.js", "styles.css"]) {
    assert.equal(allows(`.custom/plugins/fixture/${file}`), true);
  }
  for (const path of [
    ".custom/plugins/fixture/data.json", ".custom/plugins/fixture/extra.js",
    ".custom/plugins/oss-sync/main.js", ".CUSTOM/plugins/OSS-SYNC/main.js",
    ".custom/plugins/local-oss/main.js", ".custom/plugins/oss-sync./main.js",
    ".custom/plugins/excluded/main.js",
  ]) {
    assert.equal(allows(path), false, path);
  }
  for (const optOut of [{ syncConfiguration: false }, { syncCommunityPlugins: false }]) {
    assert.equal(buildSyncPathFilter(".custom", { ...rules, ...optOut })(".custom/plugins/fixture/main.js"), false);
  }
});
