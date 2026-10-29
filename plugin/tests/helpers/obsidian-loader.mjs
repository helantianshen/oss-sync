// esbuild 打包辅助：用最小 Obsidian 桩替换 "obsidian" 模块，把插件源码打包为可注入的 ESM
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

const OBSIDIAN_STUB = `
  export class ItemView {
    constructor(leaf) {
      this.leaf = leaf;
      this.containerEl = leaf.containerEl;
      this.contentEl = leaf.contentEl;
    }
  }
  export class Notice {
    constructor(message) {
      this.message = message;
      globalThis.__ossNotices?.push(message);
    }
  }
  export class App {}
  export class Modal {
    constructor(app) {
      this.app = app;
      this.modalEl = new globalThis.FakeElement("modal");
      this.titleEl = new globalThis.FakeElement("h1");
      this.contentEl = new globalThis.FakeElement("div");
    }
    open() {}
    close() {}
  }
  export class Setting {
    constructor(containerEl) {
      this.containerEl = containerEl;
      this.nameText = "";
      this.toggleValue = false;
      this.toggleDisabled = false;
      this.toggleHandlers = [];
    }
    setName(text) {
      this.nameText = text;
      this.containerEl?.setAttribute?.("data-setting-name", text);
      return this;
    }
    setDesc(text) {
      this.containerEl?.setAttribute?.("data-setting-desc", text);
      return this;
    }
    addButton(apply) {
      const element = this.containerEl.createEl("button");
      const button = {
        setButtonText: (text) => { element.setText(text); return button; },
        onClick: (handler) => { element.addEventListener("click", handler); return button; },
      };
      apply(button);
      return this;
    }
    addToggle(apply) {
      const toggle = {
        setValue: (value) => {
          this.toggleValue = value;
          return toggle;
        },
        setDisabled: (value) => {
          this.toggleDisabled = value;
          return toggle;
        },
        onChange: (handler) => {
          this.toggleHandlers.push(handler);
          return toggle;
        },
      };
      apply(toggle);
      this.containerEl?.setAttribute?.("data-toggle", "true");
      return this;
    }
  }
  export class TFile {}
  export function setIcon() {}
`;

function obsidianStubPlugin() {
  return {
    name: "obsidian-stub",
    setup(builder) {
      builder.onResolve({ filter: /^obsidian$/ }, () => ({
        path: "obsidian",
        namespace: "obsidian-stub",
      }));
      builder.onLoad({ filter: /.*/, namespace: "obsidian-stub" }, () => ({
        contents: OBSIDIAN_STUB,
        loader: "js",
      }));
    },
  };
}

/** 把 entryPoint 打包到临时目录并动态导入，返回 { module, cleanup }*/
export async function loadEntry(entryPoint) {
  const dir = await mkdtemp(join(tmpdir(), "oss-sidebar-"));
  const outfile = join(dir, "bundle.mjs");
  await build({
    entryPoints: [entryPoint],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
    plugins: [obsidianStubPlugin()],
  });
  const module = await import(pathToFileURL(outfile).href);
  return { module, cleanup: () => rm(dir, { recursive: true, force: true }) };
}
