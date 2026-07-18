// Obsidian 插件入口。

import {
  App,
  Notice,
  Plugin,
  PluginManifest,
  TAbstractFile,
  TFile,
  TFolder,
  Vault,
} from "obsidian";
import { OSSApiClient, VaultOut } from "./api";
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
  availableVaults: VaultOut[] = [];

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
    this.syncEngine.start();

    this.statusBarEl = this.addStatusBarItem();
    this.statusBarEl.addClass("oss-status-bar");
    this.setSyncState("idle");

    this.statusBarEl.onClickEvent(() => {
      this.syncEngine.runOnce({ forceFull: true });
    });

    this.addRibbonIcon("refresh-cw", "OSS force sync", async () => {
      new Notice("OSS: 触发全量同步");
      await this.syncEngine.runOnce({ forceFull: true });
    });

    this.registerEvent(
      this.app.vault.on("create", (f: TAbstractFile) => {
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueUpsert(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("modify", (f: TAbstractFile) => {
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueUpsert(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("delete", (f: TAbstractFile) => {
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueDelete(normalizeRel(f.path));
        } else if (f instanceof TFolder && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueDeleteTree(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("rename", (f: TAbstractFile, oldPath: string) => {
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueRename(normalizeRel(oldPath), normalizeRel(f.path));
        } else if (f instanceof TFolder) {
          const newRoot = normalizeRel(f.path);
          const oldRoot = normalizeRel(oldPath);
          Vault.recurseChildren(f, (child) => {
            if (!(child instanceof TFile) || this.syncEngine.isSuppressed(child.path)) return;
            const suffix = normalizeRel(child.path).slice(newRoot.length).replace(/^\/+/, "");
            const previousPath = suffix ? `${oldRoot}/${suffix}` : oldRoot;
            this.syncEngine.enqueueRename(previousPath, normalizeRel(child.path));
          });
        }
      })
    );

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

    this.addSettingTab(new OSSSettingTab(this.app, this));

    this.app.workspace.onLayoutReady(() => {
      if (this.token) {
        void this.ensureVaultBinding().then(() => {
          if (this.settings.vaultId) {
            void this.syncEngine.runOnce({ forceFull: true });
          }
        }).catch((error: unknown) => {
          new Notice("OSS: 无法加载仓库列表 " + errorMessage(error));
        });
      }
    });
  }

  onunload(): void {
    this.syncEngine?.stop();
  }

  async loadSettings(): Promise<void> {
    const data = (await this.loadData()) as PluginData | null;
    if (data) {
      this.settings = Object.assign({}, DEFAULT_SETTINGS, data);
      this.token = data.token;
    } else {
      this.settings = Object.assign({}, DEFAULT_SETTINGS);
    }
    if (!this.settings.clientId) {
      this.settings.clientId =
        typeof crypto.randomUUID === "function"
          ? crypto.randomUUID()
          : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
      await this.saveSettings();
    }
    if (!this.settings.deviceName) {
      this.settings.deviceName = `${this.app.vault.getName()} - Obsidian`;
      await this.saveSettings();
    }
  }

  async saveSettings(): Promise<void> {
    const data: PluginData = { ...this.settings, token: this.token };
    await this.saveData(data);
  }

  async login(): Promise<void> {
    const res = await this.api.login();
    this.token = res.token;
    await this.saveSettings();
    await this.ensureVaultBinding();
  }

  async register(): Promise<void> {
    const res = await this.api.register();
    this.token = res.token;
    await this.saveSettings();
    await this.ensureVaultBinding();
  }

  async refreshVaults(): Promise<VaultOut[]> {
    if (!this.api.hasToken()) {
      this.availableVaults = [];
      return [];
    }
    const result = await this.api.listVaults();
    this.availableVaults = result.vaults;
    return this.availableVaults;
  }

  async ensureVaultBinding(): Promise<void> {
    const vaults = await this.refreshVaults();
    if (vaults.length === 0) {
      const created = await this.api.createVault("Default");
      await this.bindVault(created);
      return;
    }
    const current = vaults.find((vault) => vault.id === this.settings.vaultId);
    if (current) {
      this.settings.vaultName = current.name;
      await this.saveSettings();
      return;
    }
    await this.bindVault(vaults.find((vault) => vault.is_default) ?? vaults[0]);
  }

  async bindVault(vault: VaultOut): Promise<void> {
    const changed = this.settings.vaultId !== vault.id;
    this.settings.vaultId = vault.id;
    this.settings.vaultName = vault.name;
    await this.saveSettings();
    await this.baseline.load();
    if (this.baseline.bindVault(vault.id)) {
      await this.baseline.save();
    }
    if (changed) {
      await this.syncEngine.runOnce({ forceFull: true });
    }
  }

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
        const conflict = this.syncEngine.getConflict(path);
        if (!conflict || conflict.remoteDeleted) {
          new Notice("OSS: 该冲突无法使用文本 Diff 处理");
          return;
        }
        const res = await this.api.downloadV2(
          this.settings.vaultId,
          path,
          conflict.remoteRevision
        );
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
    await this.syncEngine.resolveConflict(path, r);
    new Notice(`OSS: 冲突已解决 (${r})`, 4000);
  }
}

function normalizeRel(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "未知错误";
}
