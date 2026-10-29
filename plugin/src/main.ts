// Obsidian 插件入口

import {
  App,
  getLanguage as getObsidianLanguage,
  Notice,
  Plugin,
  PluginManifest,
  TAbstractFile,
  TFile,
  TFolder,
  Vault,
} from "obsidian";
import type { Command, Editor } from "obsidian";
import { OSSApiClient, VaultOut } from "./api";
import type {
  ShareOut,
  ServerVersionInfo,
  ServerUpdateCheckResponse,
  ServerUpdateStatusResponse,
  ServerUpdateTriggerResponse,
  ServerPluginHook,
} from "./api";
import type { AuthResponse } from "./api";
import {
  checkPluginVersion,
  PLUGIN_VERSION_CHECK_INTERVAL_MS,
} from "./plugin-update";
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
import { isTokenExpired, shouldInitializeAuthorizedSession } from "./login-state";
import {
  resolveLanguage,
  translate,
  type PluginLanguage,
  type TranslationKey,
  type TranslationParams,
} from "./i18n";
import { localizeError } from "./localized-error";
import { NoticeGuard } from "./notice-guard";
import { httpRequest, setNonJsonSink } from "./http-transport";
import { ConflictOpeningGuard } from "./conflict-opening-guard";

/** 插件持久化数据结构，在设置之外额外保存登录令牌 */
interface PluginData extends OSSSettings {
  token?: string;
}

/** Obsidian 插件入口：装配同步、协作、诊断与更新等子系统，并桥接 vault 事件到同步引擎 */
export default class OSSPlugin extends Plugin {
  settings: OSSSettings = DEFAULT_SETTINGS;
  api: OSSApiClient;
  baseline!: BaselineStore;
  syncEngine!: SyncEngine;
  collabManager!: CollabManager;
  sidebarView?: SidebarView;

  /** 诊断事件输出，开启后写入控制台，协作类关键失败额外用 warn 级别确保可见 */
  private readonly diagnostics = new Diagnostics((event) => {
    if (this.settings.diagnosticsEnabled) {
      console.debug("[oss-sync]", event.kind, JSON.stringify(event));
      // 同时用 warn 级别输出，确保过滤 debug 的控制台仍可见关键协作失败
      if (event.kind === "api_error" || event.kind === "collab_upload_attempt") {
        console.warn("[oss-sync]", event.kind, JSON.stringify(event));
      }
    }
  });
  private token?: string;
  private statusBarEl?: HTMLElement;
  private ribbonEl?: HTMLElement;
  /** 已注册命令与其翻译键的对应表，语言切换时据此刷新命令名 */
  private readonly localizedCommands: Array<{ command: Command; key: TranslationKey }> = [];
  availableVaults: VaultOut[] = [];
  private serverUpdatePoller: ServerUpdatePoller | null = null;
  private loaded = false;
  private pluginVersionCheckTimer: number | null = null;
  private pluginVersionCheckInProgress = false;
  /** 同一路径冲突编辑告警的上次提示时间，用于 3 秒去重 */
  private readonly conflictWarningLast = new Map<string, number>();
  private readonly conflictOpenings = new ConflictOpeningGuard();
  /** 登录态与网络类失败的提示冷却，同步成功后清零 */
  readonly syncErrorNotices = new NoticeGuard();

  /**
   * 构造时先以默认设置建立 API 客户端，凭据在 onload 中从持久化数据恢复
   * @param app Obsidian 应用实例
   * @param manifest 插件清单，提供版本与目录信息
   */
  constructor(app: App, manifest: PluginManifest) {
    super(app, manifest);
    this.api = new OSSApiClient(this.settings, this.diagnostics, () => this.handleTokenExpired(), this.app.vault.configDir);
  }

  /** 插件加载：恢复设置与凭据，装配各子系统，注册事件、命令与设置页，并在布局就绪后触发首轮同步 */
  async onload(): Promise<void> {
    this.loaded = true;
    // 非 JSON 响应体通常是服务端异常页，记录原文便于定位
    setNonJsonSink((info) => {
      console.warn("[oss-sync] non-JSON response", JSON.stringify(info));
    });
    await this.loadSettings();
    this.emitRuntimeInfo();

    this.api = new OSSApiClient(this.settings, this.diagnostics, () => this.handleTokenExpired(), this.app.vault.configDir);
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
        if (isCollabPath(f.path) || isCollabPath(oldPath)) {
          this.syncEngine.enqueueRenames([{ oldPath, path: f.path }]);
          return;
        }
        if (f instanceof TFile) {
          this.syncEngine.enqueueRename(normalizeRel(oldPath), normalizeRel(f.path));
        } else if (f instanceof TFolder) {
          const newRoot = normalizeRel(f.path);
          const oldRoot = normalizeRel(oldPath);
          const moves: { oldPath: string; path: string }[] = [];
          Vault.recurseChildren(f, (child) => {
            if (!(child instanceof TFile)) return;
            const suffix = normalizeRel(child.path).slice(newRoot.length).replace(/^\/+/, "");
            const previousPath = suffix ? `${oldRoot}/${suffix}` : oldRoot;
            moves.push({ oldPath: previousPath, path: normalizeRel(child.path) });
          });
          this.syncEngine.enqueueRenames(moves);
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
      if (!this.loaded) return;
      this.syncEngine.enableLocalDiscovery();
      void this.autoCheckPluginVersion();
      this.pluginVersionCheckTimer = window.setInterval(
        () => void this.autoCheckPluginVersion(), PLUGIN_VERSION_CHECK_INTERVAL_MS,
      );
      if (!this.token) return;
      void (async () => {
        await Promise.resolve();
        if (!this.loaded) return;
        await this.registerServerPluginCommands();
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
          void this.autoCheckServerUpdates();
        }
      })().catch((error: unknown) => {
        if (this.isDeviceIdentityError(error)) {
          void this.handleDeviceIdentityRequired();
          return;
        }
        // 启动期失败与后续轮询失败同源，冷期内只提示一次
        if (this.syncErrorNotices.shouldShow("plugin.vaults")) {
          new Notice(this.t("notice.loadVaultsFailed", { error: this.localizedError(error) }));
        }
      });
    });
  }

  /** 插件卸载：落盘待办队列并停止同步、协作与服务端更新轮询，卸载期间的 saveData 可能被丢弃 */
  async onunload(): Promise<void> {
    this.loaded = false;
    if (this.pluginVersionCheckTimer !== null) window.clearInterval(this.pluginVersionCheckTimer);
    this.pluginVersionCheckTimer = null;
    setNonJsonSink(null);
    await this.syncEngine?.flushPendingQueue();
    this.syncEngine?.stop();
    this.collabManager?.stop();
    this.serverUpdatePoller?.dispose();
    this.serverUpdatePoller = null;
  }


  /** 注册命令面板条目，命令名走翻译键以便语言切换时刷新 */
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

  /** 注册命令并登记其翻译键，供 refreshLocalizedUI 统一改名 */
  private addLocalizedCommand(key: TranslationKey, command: Omit<Command, "name">): void {
    const registered = this.addCommand({ ...command, name: this.t(key) });
    this.localizedCommands.push({ command: registered, key });
  }

  /** 打开侧边栏：已存在则揭示，否则在右栏创建视图 */
  async activateSidebar(): Promise<void> {
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

  /** 从磁盘加载设置，与默认值合并，过期令牌不恢复，密码始终置空 */
  async loadSettings(): Promise<void> {
    const data = (await this.loadData()) as PluginData | null;
    let hadLegacyLocalState = false;
    if (data) {
      const { token, syncPoisonObsidianFiles, ...settings } = data as PluginData & { syncPoisonObsidianFiles?: boolean };
      hadLegacyLocalState = syncPoisonObsidianFiles !== undefined;
      this.settings = Object.assign({}, DEFAULT_SETTINGS, settings);
      // 过期令牌不恢复：否则 isLoggedIn 为真但请求全部 401，设置页又不渲染登录表单
      this.token = token && !isTokenExpired(token) ? token : undefined;
    } else {
      this.settings = Object.assign({}, DEFAULT_SETTINGS);
    }
    // 密码不持久化，加载时始终置空
    this.settings.password = "";
    if (!this.settings.clientId) {
      this.settings.clientId = createClientID();
      await this.saveSettings();
    }
    if (hadLegacyLocalState) await this.saveSettings();
    if (this.settings.remotePollIntervalSec === 30) {
      this.settings.remotePollIntervalSec = 3;
      await this.saveSettings();
    }
  }

  /** 持久化设置与令牌，密码字段恒以空值写出，不落盘 */
  async saveSettings(): Promise<void> {
    const data: PluginData = {
      ...this.settings,
      password: "",
      ...(this.token ? { token: this.token } : {}),
    };
    await this.saveData(data);
  }

  /** 修改设备名；更名即换 clientId 并解绑仓库，服务端按设备身份隔离同步状态 */
  async setDeviceName(deviceName: string): Promise<void> {
    if (this.isLoggedIn()) throw new Error(this.t("notice.deviceNameChangeAfterLogout"));
    const nextName = deviceName.trim();
    if (!nextName) throw new Error(this.t("notice.deviceNameRequired"));
    if (this.settings.deviceName && this.settings.deviceName !== nextName) {
      this.settings.clientId = createClientID();
      this.settings.vaultId = "";
      this.settings.vaultName = "";
      this.availableVaults = [];
      this.api.setToken(null);
      this.syncEngine.stop();
      this.collabManager.stop();
    }
    this.settings.deviceName = nextName;
    await this.saveSettings();
  }

  /** 解析当前生效语言，未显式指定时跟随 Obsidian 界面语言 */
  getLanguage(): PluginLanguage {
    return resolveLanguage(this.settings.language, getObsidianLanguage());
  }

  /** 拼接服务端页面地址，serverUrl 末尾斜杠统一去除避免双斜杠 */
  webURL(path: "/dashboard" | "/register"): string {
    return new URL(path, this.settings.serverUrl.replace(/\/$/, "") + "/").toString();
  }

  /** 按当前语言取翻译文案，全插件提示统一入口 */
  t(key: TranslationKey, params: TranslationParams = {}): string {
    return translate(this.getLanguage(), key, params);
  }

  /** 把异常转为本语言可读文案，失败时退回 unknownError */
  localizedError(error: unknown): string {
    return localizeError(error, this.t.bind(this), this.t("common.unknownError"));
  }

  /** 取诊断事件快照，供侧边栏或排查使用 */
  getDiagnostics(): readonly DiagnosticEvent[] {
    return this.diagnostics.snapshot();
  }

  /** 记录运行时信息事件，用于在诊断流中关联插件版本 */
  emitRuntimeInfo(): void {
    this.diagnostics.record({
      kind: "runtime_info",
      at: Date.now(),
      pluginVersion: this.manifest.version,
      diagnosticsEnabled: this.settings.diagnosticsEnabled,
    });
  }

  /** 诊断开关切换时记录开关状态并补发运行时信息，便于控制台立即看到变化 */
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

  /** 用户编辑了存在冲突的文件时提醒，同一路径 3 秒内只提示一次并揭示侧边栏 */
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

  /** 语言或界面文案变化时刷新丝带、命令名与侧边栏 */
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

  /** 登录并在设备被吊销时自动以新身份重注册，成功后绑定仓库并启动同步 */
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
    // vault 绑定失败不推翻已成功的登录：凭据此时有效，稍后轮询与设置页会重试
    try {
      await this.ensureVaultBinding();
    } catch (error) {
      this.diagnostics?.record({
        kind: "api_failed",
        at: Date.now(),
        method: "GET",
        path: "/api/vaults",
        message: error instanceof Error ? `ensureVaultBinding: ${error.message}` : String(error),
      });
    }
    this.syncEngine.start();
    if (this.settings.vaultId) {
      this.collabManager.start();
      void this.syncEngine.runOnce({ forceFull: false });
    }
    return result;
  }

  /** 当前是否持有有效令牌 */
  isLoggedIn(): boolean {
    return this.api.hasToken();
  }

  /** 当前登录用户是否服务端管理员 */
  isAdmin(): boolean {
    return this.settings.role === "admin";
  }

  /** 服务端更新相关接口的薄封装，供设置页与轮询器调用 */

  /** 读取服务端当前版本信息 */
  async getServerVersion(): Promise<ServerVersionInfo> {
    return this.api.getServerVersion();
  }

  /** 查询服务端是否有可用更新 */
  async checkServerUpdate(): Promise<ServerUpdateCheckResponse> {
    return this.api.checkServerUpdate();
  }

  /** 读取服务端更新任务状态 */
  async getServerUpdateStatus(): Promise<ServerUpdateStatusResponse> {
    return this.api.getServerUpdateStatus();
  }

  /** 触发一次服务端更新，checkId 与期望版本用于防止重复触发 */
  async triggerServerUpdate(opts: { readonly checkId: string; readonly expectedVersion: string }): Promise<ServerUpdateTriggerResponse> {
    return this.api.triggerServerUpdate(opts);
  }

  /** 创建服务端更新轮询器并登记为当前实例，同一时刻只保留一个 */
  createServerUpdatePoller(opts: ServerUpdatePollerOptions): ServerUpdatePoller {
    // 先清理旧轮询器，避免计时器泄漏
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

  /** 停止并释放服务端更新轮询器 */
  stopServerUpdatePolling(): void {
    this.serverUpdatePoller?.dispose();
    this.serverUpdatePoller = null;
  }

  /** 判断是否为设备身份类错误，此类错误要求重新登录而非重试 */
  private isDeviceIdentityError(error: unknown): boolean {
    const code =
      typeof error === "object" && error !== null && "code" in error && typeof (error as { code?: unknown }).code === "string"
        ? (error as { code: string }).code
        : "";
    return code === "device_identity_required" || code === "device_identity_mismatch";
  }

  /** 设备身份失效：清空令牌与绑定并停止后台任务，提示重新登录 */
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

  /** 令牌过期：清空令牌与绑定并停止后台任务，由 API 客户端的回调触发 */
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

  /** 启动与定时检查共用入口，请求进行中或插件卸载后跳过 */
  private async autoCheckPluginVersion(): Promise<void> {
    if (!this.loaded || this.pluginVersionCheckInProgress) return;
    this.pluginVersionCheckInProgress = true;
    try {
      const version = await checkPluginVersion(this.manifest.version, (url) => httpRequest({
        url, method: "GET", headers: {},
      }));
      if (!this.loaded) return;
      this.diagnostics.record({
        kind: "plugin_version_check", at: Date.now(),
        result: version ? "update_available" : "up_to_date",
        currentVersion: this.manifest.version,
        ...(version ? { remoteVersion: version } : {}),
      });
      if (version) {
        new Notice(this.t("notice.pluginVersionAvailable", { version, current: this.manifest.version }), 10000);
      }
    } catch {
      if (this.loaded) {
        this.diagnostics.record({ kind: "plugin_version_check", at: Date.now(),
          result: "failed", currentVersion: this.manifest.version });
      }
    } finally {
      this.pluginVersionCheckInProgress = false;
    }
  }

  /** 管理员启动后延迟检查服务端版本，失败时保持静默 */
  private async autoCheckServerUpdates(): Promise<void> {
    await new Promise((r) => window.setTimeout(r, 5000));
    if (!this.loaded || !this.isLoggedIn() || !this.isAdmin()) return;
    try {
      const check = await this.checkServerUpdate();
      if (check.update_available) {
        const latest = check.latest_version ?? check.candidate?.version ?? "";
        new Notice(`服务器有新版本 ${latest}（当前 ${check.current_version}），请到设置 → 服务端更新中一键更新`, 10000);
      }
    } catch {
      // 自动检查失败不打断启动，用户仍可在设置页手动重试
    }
  }

  /** 登出：清空令牌与绑定、停止后台任务，并复查磁盘数据确保令牌未残留 */
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
    // saveData 在插件卸载期间可能被丢弃，再次确认持久化状态不含令牌
    const persisted = (await this.loadData()) as PluginData | null;
    if (persisted?.token) {
      const { token: _stale, ...rest } = persisted;
      await this.saveData(rest);
    }
    this.setSyncState("idle");
  }

  /** 打开 Obsidian 设置并定位到本插件页，控制器不可用时静默返回 */
  openSettings(): void {
    const settings = Reflect.get(this.app, "setting");
    if (!isSettingsController(settings)) return;
    settings.open();
    settings.openTabById(this.manifest.id);
  }

  /** 打开分享管理窗口 */
  openShareManager(): void {
    new ShareManagerModal(this.app, this).open();
  }

  /** 打开协作管理窗口 */
  openCollabManager(): void {
    new CollabManagerModal(this.app, this).open();
  }

  /** 打开回收站管理窗口 */
  openRecycleManager(): void {
    new RecycleManagerModal(this.app, this).open();
  }

  /** 切换文件或文件夹的分享状态，已分享则取消 */
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

  /** 按当前分享状态刷新右键菜单项文案与图标，加载失败保留默认项 */
  private async updateShareMenuItem(item: { setTitle(title: string): unknown; setIcon(icon: string): unknown }, file: TFile | TFolder): Promise<void> {
    try {
      const existing = findShare(await this.api.listShares(), file);
      item.setTitle(this.t(existing ? "menu.unshare" : "menu.share"));
      item.setIcon(existing ? "x" : "share");
    } catch {
      // 分享状态加载失败时保留默认分享操作
    }
  }

  /** 拉取当前账号可用仓库列表并缓存，未登录时清空缓存 */
  async refreshVaults(): Promise<VaultOut[]> {
    if (!this.api.hasToken()) {
      this.availableVaults = [];
      return [];
    }
    const result = await this.api.listVaults();
    this.availableVaults = result.vaults;
    return this.availableVaults;
  }

  /** 校验并修正仓库绑定：绑定失效或无可用仓库时解绑，未绑定则落到默认仓库 */
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
    // 未绑定或绑定已失效时自动落到默认仓库，避免登录后侧边栏与同步不可用
    const fallback = vaults.find((vault) => vault.is_default) ?? vaults[0];
    if (fallback) {
      this.settings.vaultId = fallback.id;
      this.settings.vaultName = fallback.name;
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

  /** 切换绑定仓库；仅在实际变更时触发一次全量同步，重复绑定同一仓库不重跑 */
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

  /** 刷新状态栏，label 为可选的错误或同步明细，同时联动侧边栏刷新 */
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

  /** 打开普通同步的冲突窗口，guard 保证同一路径不会并发弹出多个窗口 */
  openConflictModal(path: string): void {
    if (!this.conflictOpenings.begin(path)) return;
    const file = this.app.vault.getAbstractFileByPath(path);
    if (!(file instanceof TFile)) {
      this.conflictOpenings.end(path);
      new Notice(this.t("notice.conflictFileMissing", { path }));
      this.syncEngine.dismissConflict(path);
      this.sidebarView?.refresh();
      return;
    }
    void (async () => {
      let remote: string;
      let baseText: string | null = null;
      let binary = false;
      try {
        const conflict = this.syncEngine.getConflict(path);
        if (!conflict) {
          new Notice(this.t("notice.conflictTextUnavailable"));
          this.conflictOpenings.end(path);
          return;
        }
        baseText = this.syncEngine.getBaseline(path)?.baseText ?? null;
        binary = conflict.remoteType === "attachment";
        if (conflict.remoteDeleted || binary) {
          remote = "";
        } else {
          const res = await this.api.downloadV2(
            this.settings.vaultId,
            path,
            conflict.remoteRevision
          );
          remote = new TextDecoder().decode(new Uint8Array(res.content));
        }
      } catch (e) {
        new Notice(this.t("notice.fetchRemoteFailed", { error: this.localizedError(e) }));
        this.conflictOpenings.end(path);
        return;
      }
      new ConflictModal(this.app, this, this.api, file, remote, async (r) => {
        await this.applyConflictResolution(path, r);
      }, { baseText, binary, onClose: () => this.conflictOpenings.end(path) }).open();
    })();
  }

  /** 注册服务端下发的编辑器命令，能力列表失败多为登录态或网络问题 */
  private async registerServerPluginCommands(): Promise<void> {
    try {
      const capabilities = await this.api.getServerPluginCapabilities();
      for (const plugin of capabilities.plugins) {
        for (const hook of plugin.hooks) {
          if (hook.name !== "editor.command" || !hook.id || !hook.label) continue;
          const command: ServerPluginHook = hook;
          this.addCommand({
            id: `oss-plugin-${plugin.plugin_id}-${command.id}`,
            name: `${plugin.name}: ${command.label}`,
            editorCallback: (editor, view) => {
              void this.runEditorPluginCommand(plugin.plugin_id, command, editor, view);
            },
          });
        }
      }
    } catch (error: unknown) {
      // 能力列表失败多为登录态或网络问题，冷期内只提示一次，手动同步会在成功后清除抑制
      if (this.syncErrorNotices.shouldShow("plugin.capabilities")) {
        new Notice(this.t("notice.pluginCapabilitiesFailed", {
          error: error instanceof Error ? error.message : String(error),
        }));
      }
      return;
    }
  }

  /** 执行服务端编辑器命令，结果按调用时的文档快照计算，文档已变则丢弃不覆盖 */
  private async runEditorPluginCommand(
    pluginID: string, hook: ServerPluginHook, editor: Editor,
    view?: { file: TFile | null },
  ): Promise<void> {
    const content = editor.getValue();
    const file = view?.file;
    const path = file?.path;
    try {
      const result = await this.api.runServerPluginHook("editor.command", content, {
        plugin_id: pluginID,
        command_id: hook.id ?? "",
      });
      // 响应对应调用时的文档快照，不覆盖其后产生的新编辑
      if (editor.getValue() !== content || view?.file !== file || file?.path !== path) {
        new Notice(this.t("notice.pluginCommandDocumentChanged"));
        return;
      }
      editor.setValue(result.content);
    } catch (error: unknown) {
      new Notice(this.t("notice.pluginCommandFailed", {
        error: error instanceof Error ? error.message : String(error),
      }));
    }
  }

  /** 打开协作文件的冲突窗口，guard key 以 vault 与 fileId 隔离于普通同步冲突 */
  openCollaborationConflictModal(vaultId: string, fileId: number): void {
    const guardKey = `collaboration:${vaultId}:${fileId}`;
    if (!this.conflictOpenings.begin(guardKey)) return;
    const entry = this.baseline.getCollaboration(vaultId, fileId);
    if (!entry?.conflict) {
      this.conflictOpenings.end(guardKey);
      new Notice(this.t("notice.conflictTextUnavailable"));
      return;
    }
    const file = this.app.vault.getAbstractFileByPath(entry.localPath);
    if (!(file instanceof TFile)) {
      this.conflictOpenings.end(guardKey);
      new Notice(this.t("notice.conflictFileMissing", { path: entry.localPath }));
      return;
    }
    const remote = entry.conflict.remoteText;
    const baseText = entry.baseText ?? "";
    new ConflictModal(this.app, this, this.api, file, remote, async (r) => {
      await this.applyCollaborationConflictResolution(vaultId, fileId, r);
    }, { baseText, onClose: () => this.conflictOpenings.end(guardKey) }).open();
  }

  /** 应用协作冲突的解决方案，成功后按所选方案给出提示 */
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

  /** 计算字节数组的 SHA-256 十六进制摘要 */
  private async hashBytes(bytes: Uint8Array): Promise<string> {
    const digest = await crypto.subtle.digest("SHA-256", bytes as unknown as ArrayBuffer);
    return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, "0")).join("");
  }

  /** 应用普通同步冲突的解决方案，成功后按所选方案给出提示 */
  async applyConflictResolution(path: string, r: ConflictResolution): Promise<void> {
    await this.syncEngine.resolveConflict(path, r);
    const isOrdered = typeof r === "object" && (r as { kind: string }).kind === "ordered_merge";
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

/** 统一为 vault 相对路径：反斜杠转正斜杠并去掉前导 "./" */
function normalizeRel(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}

/** 在分享列表中按路径与是否文件夹精确匹配当前条目 */
function findShare(shares: { readonly shares: readonly ShareOut[] }, file: TFile | TFolder): ShareOut | undefined {
  return shares.shares.find(
    (share) => share.target_path === file.path && share.is_folder === (file instanceof TFolder)
  );
}

/** Obsidian 设置控制器的可见面，用于打开并定位插件设置页 */
interface SettingsController {
  open(): void;
  openTabById(id: string): void;
}

/** 运行时校验 app.setting 是否为可用的设置控制器 */
function isSettingsController(value: unknown): value is SettingsController {
  if (!value || typeof value !== "object") return false;
  const open = Reflect.get(value, "open");
  const openTabById = Reflect.get(value, "openTabById");
  return typeof open === "function" && typeof openTabById === "function";
}
