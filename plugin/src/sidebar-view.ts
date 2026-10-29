// 侧边栏视图：展示同步状态、分享管理、协作邀请与未解决冲突
import { ItemView, Notice, setIcon, WorkspaceLeaf } from "obsidian";
import type OSSPlugin from "./main";
import { OSS_LOGO_DATA_URL } from "./brand";

/** 侧边栏视图在 Obsidian 中的注册类型标识 */
export const SIDEBAR_VIEW_TYPE = "oss-sync-sidebar";

/** 侧边栏视图；按固定顺序渲染头部、状态、操作、管理入口、回收站、冲突与控制台链接 */
export class SidebarView extends ItemView {
  constructor(leaf: WorkspaceLeaf, private plugin: OSSPlugin) {
    super(leaf);
  }

  getViewType(): string {
    return SIDEBAR_VIEW_TYPE;
  }

  getDisplayText(): string {
    return this.plugin.t("sidebar.title");
  }

  getIcon(): string {
    return "refresh-cw";
  }

  async onOpen(): Promise<void> {
    this.contentEl.addClass("oss-sidebar");
    this.refresh();
  }

  async onClose(): Promise<void> {
    this.contentEl.removeClass("oss-sidebar");
    this.contentEl.empty();
  }

  /** 重新渲染整个侧边栏；渲染顺序固定，冲突区无内容时整段跳过 */
  refresh(): void {
    const { contentEl } = this;
    contentEl.empty();
    this.renderHeader();
    this.renderStatus();
    this.renderActions();
    this.renderManagers();
    this.renderRecycle();
    this.renderConflicts();
    this.renderConsoleLink();
  }

  /** 分享数据变化后的外部刷新入口 */
  reloadShares(): void {
    this.refresh();
  }

  /** 渲染标题区：图标、标题、摘要与关闭按钮 */
  private renderHeader(): void {
    const { contentEl, plugin } = this;
    const header = contentEl.createDiv({ cls: "oss-sidebar-header" });
    const icon = header.createDiv({ cls: "oss-sidebar-mark", attr: { "aria-hidden": "true" } });
    icon.createEl("img", { attr: { src: OSS_LOGO_DATA_URL, alt: "", width: "52", height: "21" } });

    const copy = header.createDiv({ cls: "oss-sidebar-heading" });
    copy.createEl("h3", { text: plugin.t("sidebar.title") });
    copy.createDiv({ cls: "oss-sidebar-summary", text: plugin.t("sidebar.summary") });

    const closeLabel = plugin.t("sidebar.close");
    const closeButton = header.createEl("button", {
      cls: "clickable-icon oss-sidebar-close",
      attr: { type: "button", "aria-label": closeLabel, title: closeLabel },
    });
    setIcon(closeButton, "x");
    closeButton.addEventListener("click", () => this.leaf.detach());
  }

  /** 渲染状态区：用户、设备、仓库、协作连接与同步模式 */
  private renderStatus(): void {
    const { plugin, contentEl } = this;
    const status = this.createSection(plugin.t("sidebar.status"));
    status.createDiv({
      cls: "oss-sidebar-status-line",
      text: plugin.t("sidebar.user", {
        value: plugin.settings.username || plugin.t("common.notLoggedIn"),
      }),
    });
    status.createDiv({
      cls: "oss-sidebar-status-line",
      text: plugin.t("sidebar.device", {
        value: plugin.settings.deviceName || plugin.t("common.unset"),
      }),
    });
    status.createDiv({
      cls: "oss-sidebar-status-line",
      text: plugin.t("sidebar.vault", {
        value: plugin.settings.vaultName || plugin.t("sidebar.unbound"),
      }),
    });
    status.createDiv({ cls: "oss-sidebar-status-line", text: plugin.collabManager.getTransportStatus() });
    status.createDiv({
      cls: "oss-sidebar-status-line",
      text: plugin.t("sidebar.syncMode", { value: plugin.syncEngine.getEffectiveModeLabel() }),
    });
  }

  /** 渲染操作区：全量同步与打开设置；未绑定仓库时同步按钮改为提示绑定 */
  private renderActions(): void {
    const { plugin, contentEl } = this;
    const actions = this.createSection(plugin.t("sidebar.actions"));
    const cluster = actions.createDiv({ cls: "oss-sidebar-actions oss-sidebar-actions--two-column" });
    cluster.createEl("button", { text: plugin.t("sidebar.sync"), cls: "mod-cta" })
      .addEventListener("click", () => {
        if (!plugin.settings.vaultId) {
          new Notice(plugin.t("notice.bindVaultFirst"));
          return;
        }
        void plugin.syncEngine.runOnce({ forceFull: true });
      });
    cluster.createEl("button", { text: plugin.t("sidebar.settings") })
      .addEventListener("click", () => {
        plugin.openSettings();
      });
  }

  /** 渲染管理入口：分享管理与协作管理；未绑定仓库或未登录时只给出不可用提示 */
  private renderManagers(): void {
    const { plugin } = this;
    const section = this.createSection(plugin.t("sidebar.management"));
    if (!plugin.settings.vaultId || !plugin.api.hasToken()) {
      section.createDiv({ cls: "oss-sidebar-empty", text: plugin.t("sidebar.sharesUnavailable") });
      return;
    }
    const actions = section.createDiv({ cls: "oss-sidebar-actions oss-sidebar-actions--stacked" });
    actions.createEl("button", {
      cls: "oss-sidebar-manager-button oss-sidebar-share-manager",
      text: plugin.t("sidebar.manageShares"),
    }).addEventListener("click", () => plugin.openShareManager());
    actions.createEl("button", {
      cls: "oss-sidebar-manager-button oss-sidebar-collab-manager",
      text: plugin.t("sidebar.manageCollaboration"),
    }).addEventListener("click", () => plugin.openCollabManager());
  }

  /** 渲染回收站入口；未绑定仓库或未登录时给出引导文案 */
  private renderRecycle(): void {
    const { plugin } = this;
    const section = this.createSection(plugin.t("sidebar.manageRecycle"));
    if (!plugin.settings.vaultId || !plugin.api.hasToken()) {
      section.createDiv({ cls: "oss-sidebar-empty", text: plugin.t("recycle.bindFirst") });
      return;
    }
    section.createEl("button", {
      cls: "oss-sidebar-manager-button oss-sidebar-recycle-manager",
      text: plugin.t("sidebar.manageRecycle"),
    }).addEventListener("click", () => plugin.openRecycleManager());
  }

  /** 渲染冲突区；普通冲突与协作冲突都没有时整段不渲染 */
  private renderConflicts(): void {
    const { plugin, contentEl } = this;
    const conflicts = plugin.baseline.conflicts();
    const collabConflicts = plugin.baseline.collaborationEntries().filter((e) => e.conflict !== null);
    if (conflicts.length === 0 && collabConflicts.length === 0) return;
    const section = this.createSection(plugin.t("sidebar.conflicts"));
    for (const entry of conflicts) {
      const row = section.createDiv({ cls: "oss-sidebar-conflict" });
      row.createDiv({ cls: "oss-sidebar-conflict-path", text: entry.path });
      row.createDiv({
        cls: "oss-sidebar-conflict-meta",
        text: plugin.t("sidebar.conflictSourceDevice", { revision: entry.remoteRevision }),
      });
      const actions = row.createDiv({ cls: "oss-sidebar-actions" });
      actions.createEl("button", { text: plugin.t("sidebar.resolveConflict"), cls: "mod-cta" })
        .addEventListener("click", () => plugin.openConflictModal(entry.path));
    }
    for (const entry of collabConflicts) {
      const row = section.createDiv({ cls: "oss-sidebar-conflict" });
      row.createDiv({ cls: "oss-sidebar-conflict-path", text: entry.localPath });
      // 本地协作路径形如 协作目录/所有者/文件路径，所有者位于第二段
      const owner = entry.localPath.split("/")[1] ?? plugin.t("common.unknown");
      const rev = entry.conflict?.remoteRevision ?? 0;
      row.createDiv({
        cls: "oss-sidebar-conflict-meta",
        text: plugin.t("sidebar.conflictSourceCollab", { user: owner, revision: rev }),
      });
      const actions = row.createDiv({ cls: "oss-sidebar-actions" });
      actions.createEl("button", { text: plugin.t("sidebar.resolveConflict"), cls: "mod-cta" })
        .addEventListener("click", () => plugin.openCollaborationConflictModal(entry.vaultId, entry.fileId));
    }
  }

  /** 渲染控制台跳转链接；目标地址由 serverUrl 去掉末尾斜杠后拼上 /dashboard */
  private renderConsoleLink(): void {
    const { plugin, contentEl } = this;
    const url = plugin.settings.serverUrl.replace(/\/$/, "") + "/dashboard";
    contentEl.createEl("a", {
      cls: "oss-sidebar-console-link",
      text: plugin.t("sidebar.openConsole"),
      href: url,
    });
  }

  /** 创建带标题的卡片分区，返回可继续填充内容的容器 */
  private createSection(title: string): HTMLElement {
    const section = this.contentEl.createEl("section", { cls: "oss-sidebar-card" });
    section.createEl("h4", { text: title });
    return section.createDiv({ cls: "oss-sidebar-section" });
  }
}
