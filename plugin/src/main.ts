// Obsidian 插件入口。

import {
  App,
  getLanguage as getObsidianLanguage,
  Notice,
  Plugin,
  PluginManifest,
  requestUrl,
  TAbstractFile,
  TFile,
  TFolder,
  Vault,
} from "obsidian";
import type { Command } from "obsidian";
import { OSSApiClient, VaultOut } from "./api";
import type {
  ShareOut,
  ServerVersionInfo,
  ServerUpdateCheckResponse,
  ServerUpdateStatusResponse,
  ServerUpdateTriggerResponse,
} from "./api";
import type { AuthResponse } from "./api";
import {
  checkForUpdates,
  downloadUpdateAssets,
  GitHubReleaseSource,
  isUpdateAvailable,
  type UpdateFile,
  type UpdateCheckResult,
} from "./plugin-update";
import {
  applyPluginUpdate,
  type PluginFileAdapter,
  type ReloadController,
} from "./plugin-update-apply";
import { ServerUpdatePoller, type ServerUpdatePollerOptions } from "./server-update";
import { BaselineStore } from "./baseline";
import { ConflictModal, ConflictResolution } from "./conflict-modal";
import { OSSSettingTab } from "./settings-tab";
import { DEFAULT_SETTINGS, OSSSettings } from "./settings";
import { Diagnostics } from "./diagnostics";
import type { DiagnosticEvent } from "./diagnostics";
import { ShareModal } from "./share-modal";
import { SyncEngine, SyncState } from "./sync-engine";
import { CollabInviteModal, CollabManager, isCollabPath } from "./collab-manager";
import { HistoryModal } from "./history-modal";
import { SidebarView, SIDEBAR_VIEW_TYPE } from "./sidebar-view";
import { ShareManagerModal } from "./share-manager-modal";
import { CollabManagerModal } from "./collab-manager-modal";
import { RecycleManagerModal } from "./recycle-manager-modal";
import { CollaborationFileVault } from "./collaboration-file-vault.js";
import { resolvePersistedCollaborationConflict } from "./collaboration-cas-resolver.js";
import { createOperationID } from "./operation-id.js";
import {
  createClientID,
  loginWithRevokedDeviceRecovery,
  type DeviceLoginRecoveryResult,
} from "./device-login-recovery";
import { shouldInitializeAuthorizedSession } from "./login-state";
import {
  resolveLanguage,
  translate,
  type PluginLanguage,
  type TranslationKey,
  type TranslationParams,
} from "./i18n";
import { localizeError } from "./localized-error";

interface PluginData extends OSSSettings {
  token?: string;
}

export default class OSSPlugin extends Plugin {
  settings: OSSSettings = DEFAULT_SETTINGS;
  api: OSSApiClient;
  baseline!: BaselineStore;
  syncEngine!: SyncEngine;
  collabManager!: CollabManager;
  sidebarView?: SidebarView;

  private readonly diagnostics = new Diagnostics((event) => {
    if (this.settings.diagnosticsEnabled) {
      console.log("[oss-sync]", event);
      // 同时用 warn 级别确保在过滤 debug 的控制台也能看到关键协作失败
      if (event.kind === "api_error" || event.kind === "collab_upload_attempt") {
        console.warn("[oss-sync]", event);
      }
    }
  });
  private token?: string;
  private statusBarEl?: HTMLElement;
  private ribbonEl?: HTMLElement;
  private readonly localizedCommands: Array<{ command: Command; key: TranslationKey }> = [];
  availableVaults: VaultOut[] = [];
  private serverUpdatePoller: ServerUpdatePoller | null = null;
  private loaded = false;
  private readonly conflictWarningLast = new Map<string, number>();

  constructor(app: App, manifest: PluginManifest) {
    super(app, manifest);
    this.api = new OSSApiClient(this.settings, this.diagnostics, () => this.handleTokenExpired());
  }

  async onload(): Promise<void> {
    console.log("[oss-sync] plugin loading version", this.manifest.version);
    this.loaded = true;
    await this.loadSettings();
    console.log("[oss-sync] diagnosticsEnabled=", this.settings.diagnosticsEnabled);
    this.emitRuntimeInfo();

    this.api = new OSSApiClient(this.settings, this.diagnostics, () => this.handleTokenExpired());
    if (this.token) this.api.setToken(this.token);

    this.baseline = new BaselineStore(this.app.vault);
    this.syncEngine = new SyncEngine(this.app, this.api, this.baseline, this, this.diagnostics);
    this.syncEngine.start();

    this.collabManager = new CollabManager(
      this.app,
      this.api,
      this,
      () => this.sidebarView?.refresh(),
      this.diagnostics
    );

    this.registerView(SIDEBAR_VIEW_TYPE, (leaf) => {
      this.sidebarView = new SidebarView(leaf, this);
      return this.sidebarView;
    });
    this.ribbonEl = this.addRibbonIcon("refresh-cw", this.t("ribbon.openSidebar"), () => {
      void this.activateSidebar();
    });
    this.ribbonEl.addClass("oss-ribbon-button");
    this.ribbonEl.setAttribute("data-oss-sync-ribbon", "true");

    this.statusBarEl = this.addStatusBarItem();
    this.statusBarEl.addClass("oss-status-bar");
    this.setSyncState("idle");

    this.statusBarEl.onClickEvent(() => {
      void this.activateSidebar();
    });

    this.registerCommands();

    this.registerEvent(
      this.app.vault.on("create", (f: TAbstractFile) => {
        if (f instanceof TFile && isCollabPath(f.path)) {
          this.collabManager.handleLocalEdit(f.path);
          return;
        }
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueUpsert(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("modify", (f: TAbstractFile) => {
        if (f instanceof TFile && isCollabPath(f.path)) {
          this.maybeWarnConflictEdit(f.path);
          this.collabManager.handleLocalEdit(f.path);
          return;
        }
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.maybeWarnConflictEdit(f.path);
          this.syncEngine.enqueueUpsert(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("delete", (f: TAbstractFile) => {
        if (isCollabPath(f.path)) return;
        if (f instanceof TFile && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueDelete(normalizeRel(f.path));
        } else if (f instanceof TFolder && !this.syncEngine.isSuppressed(f.path)) {
          this.syncEngine.enqueueDeleteTree(normalizeRel(f.path));
        }
      })
    );
    this.registerEvent(
      this.app.vault.on("rename", (f: TAbstractFile, oldPath: string) => {
        if (isCollabPath(f.path) || isCollabPath(oldPath)) return;
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
        if (file instanceof TFile) {
          menu.addItem((item) => {
            item
              .setTitle(this.t("menu.fileHistory"))
              .setIcon("history")
              .onClick(() => {
                new HistoryModal(this.app, this, file.path).open();
              });
          });
        }
        if (file instanceof TFile && file.extension === "md") {
          menu.addItem((item) => {
            item
              .setTitle(this.t("menu.inviteCollaboration"))
              .setIcon("user-plus")
              .onClick(() => {
                new CollabInviteModal(this.app, this, file.path).open();
              });
          });
        }
        if (file instanceof TFile || file instanceof TFolder) {
          menu.addItem((item) => {
            item
              .setTitle(this.t("menu.share"))
              .setIcon("share")
              .onClick(() => {
                void this.toggleShare(file);
              });
            void this.updateShareMenuItem(item, file);
          });
        }
      })
    );

    this.addSettingTab(new OSSSettingTab(this.app, this));

    this.app.workspace.onLayoutReady(() => {
      if (!this.token) return;
      void (async () => {
        await Promise.resolve();
        if (!this.loaded) return;
        if (this.settings.vaultId) {
          this.collabManager.start();
        }
        try {
          await this.ensureVaultBinding();
        } catch (error: unknown) {
          if (this.isDeviceIdentityError(error)) {
            await this.handleDeviceIdentityRequired();
            return;
          }
          throw error;
        }
        if (!this.loaded) return;
        if (this.settings.vaultId) {
          void this.syncEngine.runOnce({ forceFull: true }).catch((error: unknown) => {
            if (this.isDeviceIdentityError(error)) {
              void this.handleDeviceIdentityRequired();
              return;
            }
            new Notice(this.t("sync.error", { error: this.localizedError(error) }));
          });
        }
        // 管理员自动检查更新，有新版本时用 Notice 提示
        if (this.isAdmin()) {
          void this.autoCheckUpdates();
        }
      })().catch((error: unknown) => {
        if (this.isDeviceIdentityError(error)) {
          void this.handleDeviceIdentityRequired();
          return;
        }
        new Notice(this.t("notice.loadVaultsFailed", { error: this.localizedError(error) }));
      });
    });
  }

  onunload(): void {
    this.loaded = false;
    this.syncEngine?.stop();
    this.collabManager?.stop();
    this.serverUpdatePoller?.dispose();
    this.serverUpdatePoller = null;
  }


  private registerCommands(): void {
    this.addLocalizedCommand("command.openSidebar", {
      id: "oss-sync-open-sidebar",
      callback: () => {
        void this.activateSidebar();
      },
    });
    this.addLocalizedCommand("command.forceFullSync", {
      id: "oss-sync-force-sync",
      callback: () => {
        if (!this.settings.vaultId) {
          new Notice(this.t("notice.bindVaultFirst"));
          return;
        }
        new Notice(this.t("notice.fullSyncTriggered"));
        void this.syncEngine.runOnce({ forceFull: true });
      },
    });
    this.addLocalizedCommand("command.syncCurrentVault", {
      id: "oss-sync-now",
      callback: () => {
        if (!this.settings.vaultId) {
          new Notice(this.t("notice.bindVaultFirst"));
          return;
        }
        void this.syncEngine.runOnce({ forceFull: true });
      },
    });
    this.addLocalizedCommand("command.openConsole", {
      id: "oss-sync-open-console",
      callback: () => {
        window.open(this.webURL("/dashboard"), "_blank", "noopener,noreferrer");
      },
    });
    this.addLocalizedCommand("command.fileHistory", {
      id: "oss-file-history",
      callback: () => {
        const file = this.app.workspace.getActiveFile();
        if (!file) {
          new Notice(this.t("notice.noActiveFile"));
          return;
        }
        new HistoryModal(this.app, this, file.path).open();
      },
    });
  }

  private addLocalizedCommand(key: TranslationKey, command: Omit<Command, "name">): void {
    const registered = this.addCommand({ ...command, name: this.t(key) });
    this.localizedCommands.push({ command: registered, key });
  }

  private async activateSidebar(): Promise<void> {
    const leaves = this.app.workspace.getLeavesOfType(SIDEBAR_VIEW_TYPE);
    if (leaves.length > 0) {
      await this.app.workspace.revealLeaf(leaves[0]);
      return;
    }
    const leaf = this.app.workspace.getRightLeaf(false);
    if (!leaf) return;
    await leaf.setViewState({ type: SIDEBAR_VIEW_TYPE, active: true });
    await this.app.workspace.revealLeaf(leaf);
  }

  async loadSettings(): Promise<void> {
    const data = (await this.loadData()) as PluginData | null;
    if (data) {
      const { token, ...settings } = data;
      this.settings = Object.assign({}, DEFAULT_SETTINGS, settings);
      this.token = token;
    } else {
      this.settings = Object.assign({}, DEFAULT_SETTINGS);
    }
    // Passwords from older plugin versions are never retained after loading.
    this.settings.password = "";
    if (!this.settings.clientId) {
      this.settings.clientId = createClientID();
      await this.saveSettings();
    }
    if (!this.settings.deviceName) {
      this.settings.deviceName = `${this.app.vault.getName()} - Obsidian`;
      await this.saveSettings();
    }
  }

  async saveSettings(): Promise<void> {
    const data: PluginData = {
      ...this.settings,
      password: "",
      ...(this.token ? { token: this.token } : {}),
    };
    await this.saveData(data);
  }

  getLanguage(): PluginLanguage {
    return resolveLanguage(this.settings.language, getObsidianLanguage());
  }

  webURL(path: "/dashboard" | "/register"): string {
    return new URL(path, this.settings.serverUrl.replace(/\/$/, "") + "/").toString();
  }

  t(key: TranslationKey, params: TranslationParams = {}): string {
    return translate(this.getLanguage(), key, params);
  }

  localizedError(error: unknown): string {
    return localizeError(error, this.t.bind(this), this.t("common.unknownError"));
  }

  getDiagnostics(): readonly DiagnosticEvent[] {
    return this.diagnostics.snapshot();
  }

  emitRuntimeInfo(): void {
    this.diagnostics.record({
      kind: "runtime_info",
      at: Date.now(),
      pluginVersion: this.manifest.version,
      diagnosticsEnabled: this.settings.diagnosticsEnabled,
    });
  }

  emitDiagnosticsEnabled(enabled: boolean): void {
    this.diagnostics.record({
      kind: "diagnostics_enabled",
      at: Date.now(),
      enabled,
    });
    // 立即输出 runtime_info 以便控制台可关联版本
    this.emitRuntimeInfo();
    // 输出一条测试事件，确保控制台可见
    this.diagnostics.record({
      kind: "collab_upload_attempt",
      at: Date.now(),
      hasBaseRevision: true,
      baseRevisionValid: true,
      hasOperationID: true,
      operationIDValid: true,
      operationIDLength: 36,
    });
  }

  private maybeWarnConflictEdit(path: string): void {
    if (!this.settings.conflictEditWarning) return;
    if (!this.baseline) return;
    const key = normalizeRel(path);
    const now = Date.now();
    const last = this.conflictWarningLast.get(key) ?? 0;
    if (now - last < 3000) return;
    const hasOrdinary = !!this.baseline.getConflict(key);
    const hasCollab = this.baseline.collaborationEntries().some((e) => normalizeRel(e.localPath) === key && !!e.conflict);
    if (!hasOrdinary && !hasCollab) return;
    this.conflictWarningLast.set(key, now);
    new Notice(this.t("notice.conflictEditWarning", { path: key }), 6000);
    // 自动揭示侧边栏，帮助用户第一时间发现
    void this.activateSidebar().catch(() => {});
  }

  refreshLocalizedUI(): void {
    const ribbonLabel = this.t("ribbon.openSidebar");
    this.ribbonEl?.setAttribute("aria-label", ribbonLabel);
    this.ribbonEl?.setAttribute("data-tooltip-position", "right");
    this.ribbonEl?.setAttribute("title", ribbonLabel);
    for (const item of this.localizedCommands) {
      item.command.name = this.t(item.key);
    }
    this.sidebarView?.refresh();
  }

  async login(): Promise<DeviceLoginRecoveryResult<AuthResponse>> {
    const result = await loginWithRevokedDeviceRecovery(
      () => this.api.login(),
      async () => {
        this.settings.clientId = createClientID();
        this.token = undefined;
        this.api.setToken(null);
        await this.saveSettings();
      }
    );
    const res = result.response;
    this.token = res.token;
    this.settings.password = "";
    this.settings.role = res.role ?? "";
    await this.saveSettings();
    if (!shouldInitializeAuthorizedSession(res.device_status)) {
      this.settings.vaultId = "";
      this.settings.vaultName = "";
      await this.saveSettings();
      this.collabManager.stop();
      return result;
    }
    await this.ensureVaultBinding();
    this.syncEngine.start();
    if (this.settings.vaultId) {
      this.collabManager.start();
    }
    return result;
  }

  isLoggedIn(): boolean {
    return this.api.hasToken();
  }

  /** 当前登录用户是否服务端管理员；在线更新仅对管理员开放。 */
  isAdmin(): boolean {
    return this.settings.role === "admin";
  }

  /** 查询 GitHub Release 并返回当前/远端版本对比结果。 */
  async checkPluginUpdate(): Promise<UpdateCheckResult> {
    return checkForUpdates(this.settings.updateRepo, this.manifest.version, this.githubReleaseSource());
  }

  /** 下载最新 Release 三件套、原子替换并重载插件；失败时回滚。 */
  async updatePluginFromRelease(): Promise<void> {
    const source = this.githubReleaseSource();
    const check = await checkForUpdates(this.settings.updateRepo, this.manifest.version, source);
    if (!isUpdateAvailable(check)) {
      new Notice(this.t("notice.updateNoUpdate"));
      return;
    }
    const files = await downloadUpdateAssets(source, check.release);
    await this.applyPluginUpdateFiles(files);
  }

  // 服务端更新

  async getServerVersion(): Promise<ServerVersionInfo> {
    return this.api.getServerVersion();
  }

  async checkServerUpdate(): Promise<ServerUpdateCheckResponse> {
    return this.api.checkServerUpdate();
  }

  async getServerUpdateStatus(): Promise<ServerUpdateStatusResponse> {
    return this.api.getServerUpdateStatus();
  }

  async triggerServerUpdate(opts: { readonly checkId: string; readonly expectedVersion: string }): Promise<ServerUpdateTriggerResponse> {
    return this.api.triggerServerUpdate(opts);
  }

  createServerUpdatePoller(opts: ServerUpdatePollerOptions): ServerUpdatePoller {
    // Ensure previous poller is cleaned up — bounded, no leaked timers.
    this.serverUpdatePoller?.dispose();
    const poller = new ServerUpdatePoller(
      {
        getStatus: () => this.getServerUpdateStatus(),
        getVersion: () => this.getServerVersion(),
      },
      opts,
    );
    this.serverUpdatePoller = poller;
    return poller;
  }

  stopServerUpdatePolling(): void {
    this.serverUpdatePoller?.dispose();
    this.serverUpdatePoller = null;
  }

  private async applyPluginUpdateFiles(files: UpdateFile[]): Promise<void> {
    // 重载前停止同步与协作引擎，避免旧实例在替换文件后继续轮询。
    const reload = getPluginReloadController(this.app);
    const restartCollaboration = this.collabManager.isRunning();
    this.syncEngine.stop();
    this.collabManager.stop();
    const dir = this.manifest.dir ?? `.obsidian/plugins/${this.manifest.id}`;
    try {
      await applyPluginUpdate({
        adapter: this.app.vault.adapter as PluginFileAdapter,
        reload,
        dir,
        pluginID: this.manifest.id,
        files,
      });
    } catch (error) {
      // 替换前失败时当前实例仍存活，恢复被暂停的后台任务。
      if (this.loaded) {
        this.syncEngine.start();
        if (restartCollaboration) this.collabManager.start();
      }
      throw error;
    }
  }

  private githubReleaseSource(): GitHubReleaseSource {
    return new GitHubReleaseSource(async (options) => {
      const response = await requestUrl({
        url: options.url,
        method: options.method,
        headers: options.headers,
        throw: false,
      });
      return {
        status: response.status,
        json: response.json,
        text: response.text,
        arrayBuffer: response.arrayBuffer,
        headers: response.headers,
      };
    });
  }

  private isDeviceIdentityError(error: unknown): boolean {
    const code =
      typeof error === "object" && error !== null && "code" in error && typeof (error as { code?: unknown }).code === "string"
        ? (error as { code: string }).code
        : "";
    return code === "device_identity_required" || code === "device_identity_mismatch";
  }

  private async handleDeviceIdentityRequired(): Promise<void> {
    this.token = undefined;
    this.api.setToken(null);
    this.availableVaults = [];
    this.settings.vaultId = "";
    this.settings.vaultName = "";
    this.syncEngine.stop();
    this.collabManager.stop();
    await this.saveSettings();
    this.setSyncState("idle");
    new Notice(this.t("notice.reloginRequired"));
  }

  private async handleTokenExpired(): Promise<void> {
    this.token = undefined;
    this.api.setToken(null);
    this.availableVaults = [];
    this.syncEngine.stop();
    this.collabManager.stop();
    await this.saveSettings();
    this.setSyncState("idle");
    new Notice(this.t("auth.tokenExpired"));
  }

  private async autoCheckUpdates(): Promise<void> {
    await new Promise((r) => setTimeout(r, 5000));
    if (!this.loaded || !this.isLoggedIn()) return;
    try {
      const result = await this.checkPluginUpdate();
      if (isUpdateAvailable(result)) {
        new Notice(`插件有新版本 ${result.remoteVersion}（当前 ${result.currentVersion}），请到设置 → 插件更新中一键更新`, 10000);
      }
    } catch {}
    if (!this.isAdmin()) return;
    try {
      const check = await this.checkServerUpdate();
      if (check.update_available) {
        const latest = check.latest_version ?? check.candidate?.version ?? "";
        new Notice(`服务器有新版本 ${latest}（当前 ${check.current_version}），请到设置 → 服务端更新中一键更新`, 10000);
      }
    } catch {}
  }

  async logout(): Promise<void> {
    this.token = undefined;
    this.api.setToken(null);
    this.availableVaults = [];
    this.settings.password = "";
    this.settings.vaultId = "";
    this.settings.vaultName = "";
    this.syncEngine.stop();
    this.collabManager.stop();
    await this.saveSettings();
    this.setSyncState("idle");
  }

  openSettings(): void {
    const settings = Reflect.get(this.app, "setting");
    if (!isSettingsController(settings)) return;
    settings.open();
    settings.openTabById(this.manifest.id);
  }

  openShareManager(): void {
    new ShareManagerModal(this.app, this).open();
  }

  openCollabManager(): void {
    new CollabManagerModal(this.app, this).open();
  }

  openRecycleManager(): void {
    new RecycleManagerModal(this.app, this).open();
  }

  private async toggleShare(file: TFile | TFolder): Promise<void> {
    try {
      const existing = findShare(await this.api.listShares(), file);
      if (!existing) {
        new ShareModal(this.app, this, file).open();
        return;
      }
      await this.api.deleteShare(existing.share_id);
      this.sidebarView?.reloadShares();
      new Notice(this.t("sidebar.deleteShare"));
    } catch (error) {
      new Notice(this.t("sidebar.shareActionFailed", { error: this.localizedError(error) }));
    }
  }

  private async updateShareMenuItem(item: { setTitle(title: string): unknown; setIcon(icon: string): unknown }, file: TFile | TFolder): Promise<void> {
    try {
      const existing = findShare(await this.api.listShares(), file);
      item.setTitle(this.t(existing ? "menu.unshare" : "menu.share"));
      item.setIcon(existing ? "x" : "share");
    } catch {
      // Keep the default share action when the current share state cannot load.
    }
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
      if (this.settings.vaultId || this.settings.vaultName) {
        this.settings.vaultId = "";
        this.settings.vaultName = "";
        await this.saveSettings();
        this.collabManager.stop();
      }
      return;
    }
    const current = vaults.find((vault) => vault.id === this.settings.vaultId);
    if (current) {
      this.settings.vaultName = current.name;
      await this.saveSettings();
      return;
    }
    if (this.settings.vaultId || this.settings.vaultName) {
      this.settings.vaultId = "";
      this.settings.vaultName = "";
      await this.saveSettings();
      this.collabManager.stop();
    }
  }

  async bindVault(vault: VaultOut): Promise<boolean> {
    const changed = this.settings.vaultId !== vault.id;
    this.settings.vaultId = vault.id;
    this.settings.vaultName = vault.name;
    await this.saveSettings();
    await this.baseline.load();
    if (this.baseline.bindVault(vault.id)) {
      await this.baseline.save();
    }
    this.collabManager.start();
    if (changed) {
      return this.syncEngine.runOnce({ forceFull: true });
    }
    return true;
  }

  setSyncState(state: SyncState, label?: string): void {
    if (!this.statusBarEl) return;
    this.statusBarEl.empty();
    this.statusBarEl.removeClass("is-syncing", "is-error");
    const text = label ? `: ${label}` : "";
    const span = this.statusBarEl.createSpan();
    if (state === "syncing") {
      this.statusBarEl.addClass("is-syncing");
      span.setText(this.t("status.syncing", { detail: text }));
    } else if (state === "error") {
      this.statusBarEl.addClass("is-error");
      span.setText(this.t("status.error", { detail: text ? ` ${text}` : "" }));
    } else {
      span.setText(this.t("status.idle"));
    }
    this.sidebarView?.refresh();
  }

  openConflictModal(path: string): void {
    const file = this.app.vault.getAbstractFileByPath(path);
    if (!(file instanceof TFile)) {
      new Notice(this.t("notice.conflictFileMissing", { path }));
      this.syncEngine.dismissConflict(path);
      this.sidebarView?.refresh();
      return;
    }
    void (async () => {
      let remote: string;
      let baseText: string | null = null;
      try {
        const conflict = this.syncEngine.getConflict(path);
        if (!conflict || conflict.remoteDeleted) {
          new Notice(this.t("notice.conflictTextUnavailable"));
          return;
        }
        const res = await this.api.downloadV2(
          this.settings.vaultId,
          path,
          conflict.remoteRevision
        );
        remote = new TextDecoder().decode(new Uint8Array(res.content));
        baseText = this.syncEngine.getBaseline(path)?.baseText ?? null;
      } catch (e) {
        new Notice(this.t("notice.fetchRemoteFailed", { error: this.localizedError(e) }));
        return;
      }
      new ConflictModal(this.app, this, this.api, file, remote, async (r) => {
        await this.applyConflictResolution(path, r);
      }, { baseText }).open();
    })();
  }

  openCollaborationConflictModal(vaultId: string, fileId: number): void {
    const entry = this.baseline.getCollaboration(vaultId, fileId);
    if (!entry?.conflict) {
      new Notice(this.t("notice.conflictTextUnavailable"));
      return;
    }
    const file = this.app.vault.getAbstractFileByPath(entry.localPath);
    if (!(file instanceof TFile)) {
      new Notice(this.t("notice.conflictFileMissing", { path: entry.localPath }));
      return;
    }
    const remote = entry.conflict.remoteText;
    const baseText = entry.baseText ?? "";
    new ConflictModal(this.app, this, this.api, file, remote, async (r) => {
      await this.applyCollaborationConflictResolution(vaultId, fileId, r);
    }, { baseText }).open();
  }

  async applyCollaborationConflictResolution(vaultId: string, fileId: number, r: ConflictResolution): Promise<void> {
    const entry = this.baseline.getCollaboration(vaultId, fileId);
    if (!entry?.conflict) throw new Error(this.t("sync.conflictNotFound"));
    const vaultAccess = new CollaborationFileVault({ app: this.app });
    await resolvePersistedCollaborationConflict(
      {
        baseline: this.baseline,
        vault: vaultAccess,
        api: this.api,
        plugin: this,
        now: () => Date.now(),
        createOperationID: () => createOperationID(),
        onChange: () => this.sidebarView?.refresh(),
      },
      vaultId,
      fileId,
      r,
    );
    this.sidebarView?.refresh();
    const labelKey = (typeof r === "object" ? "conflict.orderedMergeButton" : { accept_remote: "conflict.acceptRemoteButton", force_local: "conflict.forceLocalButton", keep_both: "conflict.keepBothButton" }[r as string]) as TranslationKey;
    new Notice(this.t("notice.conflictResolved", { resolution: this.t(labelKey) }), 4000);
  }

  private async hashBytes(bytes: Uint8Array): Promise<string> {
    const digest = await crypto.subtle.digest("SHA-256", bytes as unknown as ArrayBuffer);
    return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, "0")).join("");
  }

  async applyConflictResolution(path: string, r: ConflictResolution): Promise<void> {
    await this.syncEngine.resolveConflict(path, r);
    const isOrdered = typeof r === "object" && (r as { kind: string }).kind === "ordered_merge";
    const key: TranslationKey = isOrdered ? "conflict.orderedMergeButton" : (r as "accept_remote" | "force_local" | "keep_both") as unknown as TranslationKey;
    const resolutionKeys: Record<string, TranslationKey> = {
      accept_remote: "conflict.acceptRemoteButton",
      force_local: "conflict.forceLocalButton",
      keep_both: "conflict.keepBothButton",
      ordered_merge: "conflict.orderedMergeButton",
    };
    const labelKey = isOrdered ? resolutionKeys.ordered_merge : resolutionKeys[r as string];
    new Notice(this.t("notice.conflictResolved", { resolution: this.t(labelKey) }), 4000);
  }
}

function normalizeRel(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}

function findShare(shares: { readonly shares: readonly ShareOut[] }, file: TFile | TFolder): ShareOut | undefined {
  return shares.shares.find(
    (share) => share.target_path === file.path && share.is_folder === (file instanceof TFolder)
  );
}

interface SettingsController {
  open(): void;
  openTabById(id: string): void;
}

function isSettingsController(value: unknown): value is SettingsController {
  if (!value || typeof value !== "object") return false;
  const open = Reflect.get(value, "open");
  const openTabById = Reflect.get(value, "openTabById");
  return typeof open === "function" && typeof openTabById === "function";
}

interface PluginManagerController {
  plugins: Record<string, unknown>;
  disablePlugin(id: string): Promise<void>;
  enablePlugin(id: string): Promise<void>;
}

/** 包装未公开的 app.plugins，实现 disablePlugin/enablePlugin 无感重载。 */
function getPluginReloadController(app: App): ReloadController {
  const manager = Reflect.get(app, "plugins") as Partial<PluginManagerController> | null | undefined;
  if (
    !manager ||
    typeof manager.disablePlugin !== "function" ||
    typeof manager.enablePlugin !== "function"
  ) {
    throw new Error("app.plugins is unavailable");
  }
  const plugins = manager.plugins ?? {};
  return {
    disablePlugin: (id) => manager.disablePlugin!(id),
    enablePlugin: (id) => manager.enablePlugin!(id),
    isLoaded: (id, expectedVersion) => {
      const instance = plugins[id] as { manifest?: { version?: string } } | undefined;
      if (!instance) return false;
      return expectedVersion == null || instance.manifest?.version === expectedVersion;
    },
  };
}
