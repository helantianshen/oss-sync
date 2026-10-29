import { App, Modal, Notice, Setting } from "obsidian";
import type { CollabEntry } from "./api";
import { localizeError } from "./localized-error";
import type OSSPlugin from "./main";

/** 发出的邀请详情窗口；按文章列出，每行可进入协作者列表或整篇取消 */
export class CollabOutgoingModal extends Modal {
  private readonly selectedArticleKeys = new Set<string>();
  private entries: readonly CollabEntry[];

  constructor(app: App, private readonly plugin: OSSPlugin, entries: readonly CollabEntry[]) {
    super(app);
    this.entries = entries;
  }

  onOpen(): void {
    this.modalEl.addClass("oss-manager-modal", "oss-collab-detail-modal");
    this.titleEl.setText(this.plugin.t("collab.sectionOutgoing"));
    void this.load();
  }

  onClose(): void {
    this.contentEl.empty();
  }

  /** 刷新协作关系并重渲染；撤销的条目不显示 */
  private async load(): Promise<void> {
    this.contentEl.empty();
    this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.loading") });
    await this.plugin.collabManager.refresh();
    const username = this.plugin.settings.username;
    const keys = new Set(this.entries.map((entry) => `${entry.vault_id}:${entry.file_id}`));
    this.entries = this.plugin.collabManager.getCollaborations()
      .filter((entry) => entry.collaborator_username !== username && entry.status !== "revoked" && keys.has(`${entry.vault_id}:${entry.file_id}`));
    this.render();
  }

  /** 渲染文章列表与批量取消按钮；空列表给出空态 */
  private render(): void {
    this.contentEl.empty();
    const byArticle = this.groupByArticle();
    if (byArticle.length === 0) {
      this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.empty") });
      return;
    }
    this.renderBulkActions(byArticle);
    for (const group of byArticle) this.renderArticle(group.key, group.entries);
  }

  /** 按 `vault_id:file_id` 聚合文章，保持服务端返回顺序 */
  private groupByArticle(): { key: string; entries: CollabEntry[] }[] {
    const groups = new Map<string, CollabEntry[]>();
    for (const entry of this.entries) {
      const key = `${entry.vault_id}:${entry.file_id}`;
      const list = groups.get(key) ?? [];
      list.push(entry);
      groups.set(key, list);
    }
    return [...groups].map(([key, entries]) => ({ key, entries }));
  }

  /** 渲染批量取消按钮；随勾选数量更新可用性与文案 */
  private renderBulkActions(byArticle: readonly { key: string; entries: CollabEntry[] }[]): void {
    const actions = this.contentEl.createDiv({ cls: "oss-sidebar-actions oss-collab-article-bulk-actions" });
    const cancelSelected = actions.createEl("button", {
      cls: "oss-collab-cancel-selected mod-warning",
      text: this.plugin.t("collab.cancelSelectedArticles", { count: this.selectedArticleKeys.size }),
    });
    cancelSelected.disabled = this.selectedArticleKeys.size === 0;
    cancelSelected.addEventListener("click", () => void this.cancelArticles(byArticle));
  }

  /** 渲染单篇文章；勾选、文章名、协作者数、查看协作者与取消同行 */
  private renderArticle(key: string, entries: readonly CollabEntry[]): void {
    const article = entries[0];
    if (!article) return;
    const row = this.contentEl.createDiv({ cls: "oss-sidebar-collab-row oss-collab-article-row" });
    const label = row.createEl("label", { cls: "oss-manager-select" });
    const checkbox = label.createEl("input", { attr: { type: "checkbox" } });
    checkbox.checked = this.selectedArticleKeys.has(key);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) this.selectedArticleKeys.add(key);
      else this.selectedArticleKeys.delete(key);
      this.render();
    });
    const name = label.createSpan({ cls: "oss-ellipsis" });
    name.setText(article.file_path);
    name.setAttribute("title", article.file_path);
    row.createDiv({ cls: "oss-manager-meta", text: this.plugin.t("collab.collaboratorCount", { count: entries.length }) });
    const actions = row.createDiv({ cls: "oss-sidebar-actions" });
    actions.createEl("button", { cls: "oss-collab-view", text: this.plugin.t("collab.viewCollaborators") })
      .addEventListener("click", () => new CollabCollaboratorsModal(this.app, this.plugin, entries).open());
    actions.createEl("button", { cls: "oss-collab-cancel-article mod-warning", text: this.plugin.t("collab.cancelArticle") })
      .addEventListener("click", () => void this.cancelArticle(entries));
  }

  /** 撤销该文章的全部协作者 */
  private async cancelArticle(entries: readonly CollabEntry[]): Promise<void> {
    await Promise.all(entries.map((entry) => this.plugin.collabManager.revoke(entry)));
    await this.load();
  }

  /** 撤销勾选中的全部文章，完成后清空勾选 */
  private async cancelArticles(byArticle: readonly { key: string; entries: CollabEntry[] }[]): Promise<void> {
    const selected = byArticle.filter((group) => this.selectedArticleKeys.has(group.key));
    await Promise.all(selected.flatMap((group) => group.entries).map((entry) => this.plugin.collabManager.revoke(entry)));
    this.selectedArticleKeys.clear();
    await this.load();
  }
}

/** 单篇文章的协作者列表窗口；多选撤销，并在同一行切换协作者的历史查看权限 */
export class CollabCollaboratorsModal extends Modal {
  private readonly selectedIDs = new Set<number>();
  private entries: readonly CollabEntry[];

  constructor(app: App, private readonly plugin: OSSPlugin, entries: readonly CollabEntry[]) {
    super(app);
    this.entries = entries;
  }

  onOpen(): void {
    this.modalEl.addClass("oss-manager-modal", "oss-collab-detail-modal");
    this.titleEl.setText(this.plugin.t("collab.collaboratorsTitle", { path: this.entries[0]?.file_path ?? "" }));
    void this.load();
  }

  onClose(): void {
    this.contentEl.empty();
  }

  /** 刷新协作关系并重渲染；撤销的条目不显示 */
  private async load(): Promise<void> {
    this.contentEl.empty();
    this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.loading") });
    await this.plugin.collabManager.refresh();
    const ids = new Set(this.entries.map((entry) => entry.id));
    this.entries = this.plugin.collabManager.getCollaborations().filter((entry) => ids.has(entry.id) && entry.status !== "revoked");
    this.render();
  }

  /** 渲染协作者列表与批量撤销按钮；空列表给出空态 */
  private render(): void {
    this.contentEl.empty();
    if (this.entries.length === 0) {
      this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.empty") });
      return;
    }
    const actions = this.contentEl.createDiv({ cls: "oss-sidebar-actions oss-collaborators-actions" });
    const revokeSelected = actions.createEl("button", {
      cls: "oss-collab-revoke-selected mod-warning",
      text: this.plugin.t("collab.revokeSelected", { count: this.selectedIDs.size }),
    });
    revokeSelected.disabled = this.selectedIDs.size === 0;
    revokeSelected.addEventListener("click", () => void this.revokeSelected(revokeSelected));
    for (const entry of this.entries) this.renderEntry(entry, revokeSelected);
  }

  /** 渲染单个协作者；勾选、名称、历史查看开关与撤销按钮同行 */
  private renderEntry(entry: CollabEntry, revokeSelected: HTMLButtonElement): void {
    const row = this.contentEl.createDiv({ cls: "oss-sidebar-collab-row oss-collaborator-row" });
    const label = row.createEl("label", { cls: "oss-manager-select" });
    const checkbox = label.createEl("input", { attr: { type: "checkbox" } });
    checkbox.checked = this.selectedIDs.has(entry.id);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) this.selectedIDs.add(entry.id);
      else this.selectedIDs.delete(entry.id);
      revokeSelected.disabled = this.selectedIDs.size === 0;
      revokeSelected.setText(this.plugin.t("collab.revokeSelected", { count: this.selectedIDs.size }));
    });
    label.createSpan({ text: entry.collaborator_username });
    const toggleHost = row.createDiv({ cls: "oss-collaborator-history-toggle" });
    new Setting(toggleHost)
      .setName(this.plugin.t("collab.historyView"))
      .addToggle((toggle) =>
        toggle
          .setValue(entry.allow_history_view)
          .setDisabled(entry.status !== "accepted")
          .onChange((value) => void this.setHistoryView(entry, value))
      );
    row.createEl("button", { cls: "oss-collab-revoke mod-warning", text: this.plugin.t("collab.revoke") })
      .addEventListener("click", () => void this.revoke(entry));
  }

  /** 切换协作者的历史查看权限；失败时还原列表以反映服务端真实状态 */
  private async setHistoryView(entry: CollabEntry, allow: boolean): Promise<void> {
    try {
      await this.plugin.collabManager.setHistoryView(entry, allow);
    } catch (error) {
      new Notice(this.plugin.t("collab.historyViewFailed", { error: errorMessage(error, this.plugin) }));
      await this.load();
    }
  }

  /** 撤销单条协作者 */
  private async revoke(entry: CollabEntry): Promise<void> {
    await this.plugin.collabManager.revoke(entry);
    this.selectedIDs.delete(entry.id);
    await this.load();
  }

  /** 撤销勾选中的协作者，完成后清空勾选 */
  private async revokeSelected(revokeSelected: HTMLButtonElement): Promise<void> {
    const selected = this.entries.filter((entry) => this.selectedIDs.has(entry.id));
    await Promise.all(selected.map((entry) => this.plugin.collabManager.revoke(entry)));
    this.selectedIDs.clear();
    await this.load();
  }
}

/** 把错误本地化为用户可读文本 */
function errorMessage(error: unknown, plugin: OSSPlugin): string {
  return localizeError(error, plugin.t.bind(plugin), plugin.t("common.unknownError"));
}
