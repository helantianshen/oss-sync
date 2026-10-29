import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";
import { build } from "esbuild";

const pluginRoot = fileURLToPath(new URL("../", import.meta.url));
const obsidianStub = `
  export const host = { request: null, modal: null };
  export const Platform = { isDesktop: false };
  export function requestUrl() { return host.request(); }
  export class App {}
  export class Modal {
    constructor() { this.contentEl = { buttons: [], createEl() {}, empty() {} }; }
    open() { host.modal = this; this.onOpen(); }
    close() { this.onClose(); }
  }
  export class Setting {
    constructor(el) { this.el = el; }
    addButton(apply) {
      const button = {
        setButtonText() { return this; },
        setWarning() { return this; },
        onClick(fn) { this.click = fn; return this; },
      };
      apply(button);
      this.el.buttons.push(button);
      return this;
    }
  }
`;

/** 打包实际源码，只模拟 Obsidian 宿主；每个用例独立且自动清理产物 */
async function load(t, name) {
  const dir = await mkdtemp(join(tmpdir(), "oss-functional-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const outfile = join(dir, "entry.mjs");
  await build({
    stdin: {
      contents: `export * from "./src/${name}.ts"; export { host } from "obsidian";`,
      resolveDir: pluginRoot,
    },
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
    plugins: [{
      name: "obsidian-host",
      setup(builder) {
        builder.onResolve({ filter: /^obsidian$/ }, () => ({ path: "obsidian", namespace: "host" }));
        builder.onLoad({ filter: /.*/, namespace: "host" }, () => ({ contents: obsidianStub }));
      },
    }],
  });
  return import(pathToFileURL(outfile).href);
}

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
