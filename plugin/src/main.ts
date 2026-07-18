// OSS Plugin main entry.
// 装配：settings → api → baseline → syncEngine → 三监听器 → 状态栏 → Ribbon。
//
// 数据持久化：所有 settings + token 都存在 Obsidian 的 loadData()/saveData()
// 单一对象里（结构见 PluginData）。
//
// Phase 3 范围：身份验证 + Vault 监听 + Lodash 防抖 + 并发上传下载 + 与 Phase 2 后端联调。
// Phase 5 追加右键分享菜单 + 冲突 Diff Modal。

import {
  App,
  Notice,
  Plugin,
  PluginManifest,
  TAbstractFile,
  TFile,
  TFolder,
} from "obsidian";
import { OSSApiClient } from "./api";
import { BaselineStore } from "./baseline";
import { ConflictModal, ConflictResolution } from "./conflict-modal";
import { OSSSettingTab } from "./settings-tab";
import { DEFAULT_SETTINGS, OSSSettings } from "./settings";
import { ShareModal } from "./share-modal";
import { SyncEngine, SyncState } from "./sync-engine";

interface PluginData extends OSSSettings {
  token?: string;
}

export default class OSSPlugin extends Plugin {
  settings: OSSSettings = DEFAULT_SETTINGS;
  api: OSSApiClient;
  baseline!: BaselineStore;
  syncEngine!: SyncEngine;

  private token?: string;
  private statusBarEl?: HTMLElement;

  constructor(app: App, manifest: PluginManifest) {
    super(app, manifest);
    this.api = new OSSApiClient(this.settings);
  }

  async onload(): Promise<void> {
    await this.loadSettings();

    this.api = new OSSApiClient(this.settings);
    if (this.token) this.api.setToken(this.token);

    this.baseline = new BaselineStore(this.app.vault);
    this.syncEngine = new SyncEngine(this.app, this.api, this.baseline, this);

    // 状态栏图标
    this.statusBarEl = this.addStatusBarItem();
    this.statusBarEl.addClass("oss-status-bar");
    this.setSyncState("idle");

    // 状态栏点击 = 全量同步（决策 6.5 全量校验按钮）
    this.statusBarEl.onClickEvent(() => {
      this.syncEngine.runOnce({ forceFull: true });
    });

    // Ribbon 立即强制全量同步
    this.addRibbonIcon("refresh-cw", "OSS force sync", async () => {
      new Notice("OSS: 触发全量同步");
      await this.syncEngine.runOnce({ forceFull: true });
    });

    // Vault 三监听器（决策 6.1、6.2 黑名单过滤在 syncEngine.enqueue 内）
    this.registerEvent(
      this.app.vault.on("create", (f: TAbstractFile) => {
        if (f instanceof TFile) this.syncEngine.enqueue(normalizeRel(f.path));
      })
    );
    this.registerEvent(
      this.app.vault.on("modify", (f: TAbstractFile) => {
        if (f instanceof TFile) this.syncEngine.enqueue(normalizeRel(f.path));
      })
    );
    this.registerEvent(
      this.app.vault.on("delete", (f: TAbstractFile) => {
        if (f instanceof TFile) this.syncEngine.enqueue(normalizeRel(f.path));
      })
    );

    // Phase 5：右键文件菜单「分享至轻博客」
    this.registerEvent(
      this.app.workspace.on("file-menu", (menu, file) => {
        if (file instanceof TFile || file instanceof TFolder) {
          menu.addItem((item) => {
            item
              .setTitle("分享至轻博客")
              .setIcon("share")
              .onClick(() => {
                new ShareModal(this.app, this, file).open();
              });
          });
        }
      })
    );

    // 设置面板
    this.addSettingTab(new OSSSettingTab(this.app, this));
  }

  // --- 持久化 ---

  async loadSettings(): Promise<void> {
    const data = (await this.loadData()) as PluginData | null;
    if (data) {
      this.settings = Object.assign({}, DEFAULT_SETTINGS, data);
      this.token = data.token;
    } else {
      this.settings = Object.assign({}, DEFAULT_SETTINGS);
    }
  }

  async saveSettings(): Promise<void> {
    const data: PluginData = { ...this.settings, token: this.token };
    await this.saveData(data);
  }

  // --- 鉴权（设置面板按钮调用） ---

  async login(): Promise<void> {
    const res = await this.api.login();
    this.token = res.token;
    await this.saveSettings();
  }

  async register(): Promise<void> {
    const res = await this.api.register();
    this.token = res.token;
    await this.saveSettings();
  }

  // --- 状态栏 ---

  setSyncState(state: SyncState, label?: string): void {
    if (!this.statusBarEl) return;
    this.statusBarEl.empty();
    this.statusBarEl.removeClass("is-syncing", "is-error");
    const text = label ? `: ${label}` : "";
    const span = this.statusBarEl.createSpan();
    if (state === "syncing") {
      this.statusBarEl.addClass("is-syncing");
      span.setText("🔄 OSS syncing" + text);
    } else if (state === "error") {
      this.statusBarEl.addClass("is-error");
      span.setText("🔴 OSS error" + (text ? " " + text : ""));
    } else {
      span.setText("🟢 OSS idle");
    }
  }

  // --- Phase 5：冲突解决 ---

  openConflictModal(path: string): void {
    const file = this.app.vault.getAbstractFileByPath(path);
    if (!(file instanceof TFile)) {
      new Notice("OSS: 冲突文件已不存在 " + path);
      this.syncEngine.dismissConflict(path);
      return;
    }
    void (async () => {
      let remote: string;
      try {
        const res = await this.api.download(path);
        remote = new TextDecoder().decode(new Uint8Array(res.content));
      } catch (e) {
        new Notice("OSS: 拉取云端版本失败 " + (e as Error).message);
        return;
      }
      new ConflictModal(this.app, this, this.api, file, remote, async (r) => {
        await this.applyConflictResolution(path, r);
      }).open();
    })();
  }

  async applyConflictResolution(path: string, r: ConflictResolution): Promise<void> {
    const file = this.app.vault.getAbstractFileByPath(path);
    if (!(file instanceof TFile)) throw new Error("file vanished");

    if (r === "accept_remote") {
      const res = await this.api.download(path);
      await this.app.vault.modifyBinary(file, res.content);
      this.baseline.set(path, { serverMTime: res.mtime, hash: res.hash });
      await this.baseline.save();
    } else if (r === "force_local") {
      const buf = await this.app.vault.readBinary(file);
      const adjusted = this.api.getAdjustedMtime(file.stat.mtime);
      const res = await this.api.upload(path, adjusted, buf);
      this.baseline.set(path, { serverMTime: res.mtime, hash: res.hash });
      await this.baseline.save();
    } else {
      const ts = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
      const parts = path.split("/");
      const base = parts.pop()!.replace(/\.md$/i, "");
      const ext = path.toLowerCase().endsWith(".md") ? ".md" : "";
      const copyPath = [...parts, `${base}_conflict_${ts}${ext}`].join("/");
      const localText = await this.app.vault.read(file);
      const copyFile = await this.app.vault.create(copyPath, localText);
      const res = await this.api.download(path);
      await this.app.vault.modifyBinary(file, res.content);
      this.baseline.set(path, { serverMTime: res.mtime, hash: res.hash });
      await this.baseline.save();
      this.syncEngine.enqueue(normalizeRel(copyFile.path));
    }
    this.syncEngine.dismissConflict(path);
    new Notice(`OSS: 冲突已解决 (${r})`, 4000);
  }
}

function normalizeRel(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}
