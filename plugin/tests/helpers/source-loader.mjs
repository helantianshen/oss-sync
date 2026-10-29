import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { build } from "esbuild";

const pluginRoot = fileURLToPath(new URL("../../", import.meta.url));
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
export async function loadSource(t, name) {
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
