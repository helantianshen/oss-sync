import { App, Modal, Notice, Setting } from "obsidian";
import type OSSPlugin from "./main";
import { localizeError } from "./localized-error";
import type { CollabEntry, OSSApiClient } from "./api";
import { normalizePath } from "./baseline";
import type { Diagnostics } from "./diagnostics";
import {
  COLLAB_DIR,
  CollaborationFileVault,
  collabLocalPath,
  isCollabPath,
} from "./collaboration-file-vault.js";
import { CollaborationFileSync } from "./collaboration-file-sync.js";
import type { AcceptedCollaborationFile } from "./collaboration-file-sync.js";
import { CollaborationRemoteSync } from "./collaboration-remote-sync.js";
import { CollaborationSyncCoordinator } from "./collaboration-sync-coordinator.js";
import { CollaborationTransport } from "./collaboration-transport.js";
import { createOperationID } from "./operation-id.js";
import { NoticeGuard } from "./notice-guard.js";

export { COLLAB_DIR, collabLocalPath, isCollabPath };

/** 协作管理入口：编排文件同步、远端同步与事件传输 */
export class CollabManager {
  private collabs: CollabEntry[] = [];
  private readonly vaultAccess: CollaborationFileVault;
  private readonly fileSync: CollaborationFileSync;
  private readonly remoteSync: CollaborationRemoteSync;
  private readonly coordinator = new CollaborationSyncCoordinator();
  private readonly transport: CollaborationTransport;
  /** 协作列表刷新由长轮询驱动，失败提示按 key 冷却抑制 */
  private readonly loadNotices = new NoticeGuard();

  constructor(
    private readonly app: App,
    private readonly api: OSSApiClient,
    private readonly plugin: OSSPlugin,
    private readonly onChange: () => void,
    private readonly diagnostics?: Diagnostics
  ) {
    this.vaultAccess = new CollaborationFileVault({ app: this.app });
    this.fileSync = new CollaborationFileSync({
      baseline: this.plugin.baseline,
      vault: this.vaultAccess,
      api: this.api,
      plugin: this.plugin,
      getAccepted: () => this.getAcceptedFiles(),
      onChange: () => this.onChange(),
      now: () => Date.now(),
      createOperationID: () => createOperationID(),
      coordinator: this.coordinator,
    });
    this.remoteSync = new CollaborationRemoteSync({
      baseline: this.plugin.baseline,
      vault: this.vaultAccess,
      api: this.api,
      plugin: this.plugin,
      getAccepted: () => this.getAcceptedFiles(),
      getUsername: () => this.plugin.settings.username ?? "",
      now: () => Date.now(),
      createOperationID: () => createOperationID(),
      onChange: () => this.onChange(),
      coordinator: this.coordinator,
      noticeGuard: this.plugin.syncErrorNotices,
      onConflict: () => this.plugin.activateSidebar(),
    });
    this.transport = new CollaborationTransport({
      api: this.api,
      diagnostics: this.diagnostics,
      getVaultId: () => this.plugin.settings.vaultId,
      getForceSSE: () => this.plugin.settings.forceSSE,
      onRefresh: () => this.refresh(),
      onChanged: () => this.applyRemoteChange(),
      onStatusChange: () => this.onChange(),
    });
  }

  /** 启动事件传输 */
  start(): void {
    this.transport.start();
  }

  /** 停止事件传输与文件同步，并清除失败提示的冷却状态 */
  stop(): void {
    this.transport.stop();
    this.fileSync.stop();
    this.loadNotices.reset();
  }

  /** 事件传输是否在运行 */
  isRunning(): boolean {
    return this.transport.isRunning();
  }

  /** 传输状态的中文或英文标签 */
  getTransportStatus(): string {
    const status = this.transport.getStatus();
    if (status === "sse") return this.plugin.t("sidebar.collabSSE");
    if (status === "sse_failed") return this.plugin.t("sidebar.collabSSEFailed");
    if (status === "sse_unavailable") return this.plugin.t("sidebar.collabSSEUnavailable");
    if (status === "long_poll") return this.plugin.t("sidebar.collabConnected");
    return this.plugin.t("sidebar.collabDisconnected");
  }

  /** 等待当前用户回应的邀请 */
  getPendingCollabs(): CollabEntry[] {
    return this.collabs.filter(
      (entry) => entry.status === "pending" && entry.collaborator_username === this.plugin.settings.username
    );
  }

  /** 当前持有的全部协作关系，含 revoked */
  getCollaborations(): readonly CollabEntry[] {
    return this.collabs;
  }

  /** 定位本地路径对应的服务端仓库与文件路径；协作文件不可从普通历史接口恢复 */
  getHistoryTarget(path: string): { vaultID: string; path: string; canRestore: boolean } | null {
    const normalized = normalizePath(path);
    for (const entry of this.collabs) {
      if (entry.status !== "accepted") continue;
      if (collabLocalPath(entry.owner_username, entry.file_path) !== normalized) continue;
      return { vaultID: entry.vault_id, path: entry.file_path, canRestore: false };
    }
    return null;
  }

  /** 重新拉取仓库内与跨仓库的协作关系并去重 */
  async refresh(): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    if (!this.api.hasToken() || !vaultID) {
      this.collabs = [];
      this.onChange();
      return;
    }
    try {
      await this.coordinator.run(async () => {
        const [vaultResult, inboxResult] = await Promise.all([
          this.api.collabList(vaultID),
          this.api.collabInbox(),
        ]);
        // 去重：同 vault:file:collaborator 仅保留最新一条，优先 accepted > pending > revoked
        const deduped = new Map<string, CollabEntry>();
        const rank = (s: string) => (s === "accepted" ? 2 : s === "pending" ? 1 : 0);
        const consider = (e: CollabEntry) => {
          const collabId = String(e.collaborator_id ?? e.collaborator_username ?? "");
          const key = `${e.vault_id}:${e.file_id}:${collabId}`;
          const prev = deduped.get(key);
          if (!prev) {
            deduped.set(key, e);
            return;
          }
          const rPrev = rank(prev.status);
          const rCur = rank(e.status);
          if (rCur > rPrev) {
            deduped.set(key, e);
            return;
          }
          if (rCur === rPrev && e.created_at > prev.created_at) {
            deduped.set(key, e);
          }
        };
        for (const entry of vaultResult.collaborations) consider(entry);
        for (const entry of inboxResult.collaborations) consider(entry);
        this.collabs = [...deduped.values()];
        // 清理本地已 revoked/非协作文件的残留 baseline
        await this.pruneStaleCollaborationBaselines();
        this.diagnostics?.record({
          kind: "collab_activity",
          at: Date.now(),
          entries: this.collabs.length,
          newestCreatedAt: newestCreatedAt(this.collabs),
        });
        this.onChange();
      });
      await this.remoteSync.refresh();
    } catch (error) {
      // 协作列表刷新由长轮询周期性驱动，失败提示走冷却抑制，状态栏与侧边栏持续展示
      if (this.loadNotices.shouldShow("collab.list")) {
        new Notice(this.plugin.t("collab.loadFailed", { error: errorMessage(error, this.plugin) }));
      }
    }
  }

  /** 清理与上传下载串行执行，失败时保留基线供后续刷新重试 */
  private async pruneStaleCollaborationBaselines(): Promise<void> {
    const baseline = this.plugin.baseline;
    if (typeof baseline.collaborationEntries !== "function" || typeof baseline.removeCollaboration !== "function") return;
    for (const entry of baseline.collaborationEntries()) {
      if (this.hasActiveCollaboration(entry.vaultId, entry.fileId)) continue;
      try {
        await this.cleanupCollaboration(entry.vaultId, entry.fileId);
      } catch (error) {
        this.showCleanupFailure(entry.vaultId, entry.fileId, error);
      }
    }
  }

  private hasActiveCollaboration(vaultId: string, fileId: number): boolean {
    return this.collabs.some((entry) =>
      entry.vault_id === vaultId && entry.file_id === fileId &&
      entry.collaborator_username === this.plugin.settings.username &&
      (entry.status === "accepted" || entry.status === "pending"));
  }

  /** 无基线的路径不归清理流程所有，同路径的有效协作仍需保留正文 */
  private async cleanupCollaboration(vaultId: string, fileId: number): Promise<void> {
    const baseline = this.plugin.baseline;
    const entry = baseline.getCollaboration(vaultId, fileId);
    if (!entry || this.hasActiveCollaboration(vaultId, fileId)) return;
    const sharedPath = this.collabs.some((active) =>
      active.collaborator_username === this.plugin.settings.username &&
      (active.status === "accepted" || active.status === "pending") &&
      normalizePath(collabLocalPath(active.owner_username, active.file_path)) === normalizePath(entry.localPath));
    if (!sharedPath) await this.vaultAccess.deleteLocal(entry.localPath, entry.serverHash);
    baseline.removeCollaboration(vaultId, fileId);
    try {
      await baseline.save();
    } catch (error) {
      baseline.setCollaboration(vaultId, fileId, entry);
      throw error;
    }
    this.loadNotices.reset(`cleanup:${vaultId}:${fileId}`);
  }

  private showCleanupFailure(vaultId: string, fileId: number, error: unknown, manual = false): void {
    const shouldShow = this.loadNotices.shouldShow(`cleanup:${vaultId}:${fileId}`);
    if (manual || shouldShow) {
      new Notice(this.plugin.t("collab.cleanupFailed", { error: errorMessage(error, this.plugin) }));
    }
  }

  /** 本地协作文件被编辑，转交文件同步防抖上传 */
  handleLocalEdit(path: string): void {
    this.fileSync.handleLocalEdit(path);
  }

  /** 接受或拒绝邀请 */
  async respond(entry: CollabEntry, accept: boolean): Promise<void> {
    try {
      await this.api.collabRespond(entry.vault_id, entry.id, accept);
      new Notice(this.plugin.t(accept ? "collab.accepted" : "collab.rejected"));
      await this.refresh();
    } catch (error) {
      new Notice(this.plugin.t("collab.respondFailed", { error: errorMessage(error, this.plugin) }));
    }
  }

  /** 撤回邀请或解除协作，同时清理本地文件与基线 */
  async revoke(entry: CollabEntry): Promise<void> {
    await this.endCollaboration(entry, "revoke");
  }

  /** 主动退出协作，同时清理本地文件与基线 */
  async leave(entry: CollabEntry): Promise<void> {
    await this.endCollaboration(entry, "leave");
  }

  private async endCollaboration(entry: CollabEntry, action: "leave" | "revoke"): Promise<void> {
    try {
      await this.coordinator.run(async () => {
        if (action === "leave") await this.api.collabLeave(entry.vault_id, entry.id);
        else await this.api.collabRevoke(entry.vault_id, entry.id);
        this.collabs = this.collabs.map((current) =>
          current.vault_id === entry.vault_id && current.id === entry.id ? { ...current, status: "revoked" } : current);
        try {
          await this.cleanupCollaboration(entry.vault_id, entry.file_id);
          new Notice(this.plugin.t(action === "leave" ? "collab.left" : "collab.revoked"));
        } catch (error) {
          this.showCleanupFailure(entry.vault_id, entry.file_id, error, true);
        }
      });
    } catch (error) {
      new Notice(this.plugin.t(action === "leave" ? "collab.leaveFailed" : "collab.revokeFailed", {
        error: errorMessage(error, this.plugin),
      }));
      return;
    }
    await this.refresh();
  }

  /** 当前用户已接受的协作文件列表 */
  private getAcceptedFiles(): readonly AcceptedCollaborationFile[] {
    return this.collabs
      .filter(
        (entry) => entry.status === "accepted" && entry.collaborator_username === this.plugin.settings.username
      )
      .map((entry) => ({
        vaultId: entry.vault_id,
        fileId: entry.file_id,
        localPath: collabLocalPath(entry.owner_username, entry.file_path),
      }));
  }

  /** 远端事件到达时同时刷新协作列表与普通同步，确保本地与远端一致 */
  private async applyRemoteChange(): Promise<void> {
    await Promise.all([this.refresh(), this.plugin.syncEngine.runOnce({ forceFull: false })]);
  }

  /** 切换协作者对该文件修改记录的读取权限；owner 或 manager 才可调用 */
  async setHistoryView(entry: CollabEntry, allow: boolean): Promise<void> {
    await this.plugin.api.collabSetHistoryView(entry.vault_id, entry.id, allow);
    await this.refresh();
  }
}

/** 邀请协作者编辑指定 Markdown 文件的弹窗 */
export class CollabInviteModal extends Modal {
  private username = "";
  private allowHistoryView = false;

  constructor(app: App, private plugin: OSSPlugin, private filePath: string) {
    super(app);
  }

  onOpen(): void {
    const { contentEl, titleEl } = this;
    titleEl.setText(this.plugin.t("collab.inviteTitle", { path: this.filePath }));
    new Setting(contentEl)
      .setName(this.plugin.t("collab.username"))
      .setDesc(this.plugin.t("collab.usernameDesc"))
      .addText((text) =>
        text
          .setPlaceholder(this.plugin.t("collab.usernamePlaceholder"))
          .onChange((value) => {
            this.username = value.trim();
          })
      );
    new Setting(contentEl)
      .setName(this.plugin.t("collab.allowHistoryView"))
      .setDesc(this.plugin.t("collab.allowHistoryViewDesc"))
      .addToggle((toggle) =>
        toggle.setValue(this.allowHistoryView).onChange((value) => {
          this.allowHistoryView = value;
        })
      );
    new Setting(contentEl)
      .addButton((button) => button.setButtonText(this.plugin.t("common.cancel")).setWarning().onClick(() => this.close()))
      .addButton((button) => button.setButtonText(this.plugin.t("collab.send")).onClick(() => void this.invite()));
  }

  onClose(): void {
    this.contentEl.empty();
  }

  /** 提交邀请；未填用户名或未绑定仓库时给出对应提示 */
  private async invite(): Promise<void> {
    if (!this.username) {
      new Notice(this.plugin.t("collab.usernameRequired"));
      return;
    }
    const vaultID = this.plugin.settings.vaultId;
    if (!vaultID) {
      new Notice(this.plugin.t("collab.bindFirst"));
      return;
    }
    try {
      await this.plugin.api.collabInvite(vaultID, this.filePath, this.username, this.allowHistoryView);
      new Notice(this.plugin.t("collab.sent"));
      this.close();
    } catch (error) {
      new Notice(this.plugin.t("collab.sendFailed", { error: errorMessage(error, this.plugin) }));
    }
  }
}

/** 把错误本地化为用户可读文本 */
function errorMessage(error: unknown, plugin: OSSPlugin): string {
  return localizeError(error, plugin.t.bind(plugin), plugin.t("common.unknownError"));
}

/** 取列表中最新创建时间，ISO 字符串可直接按字典序比较 */
function newestCreatedAt(entries: readonly CollabEntry[]): string | undefined {
  let newest: string | undefined;
  for (const entry of entries) {
    if (entry.created_at && (!newest || entry.created_at > newest)) newest = entry.created_at;
  }
  return newest;
}
