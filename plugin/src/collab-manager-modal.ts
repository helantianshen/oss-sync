import { App, Modal } from "obsidian";
import type { CollabEntry } from "./api";
import { CollabIncomingModal } from "./collab-incoming-modal";
import { CollabOutgoingModal } from "./collab-outgoing-modal";
import type { TranslationKey } from "./i18n";
import type OSSPlugin from "./main";

/** 协作邀请总览弹窗；两个分区只呈现标题、数量与查看按钮，条目在各自的详情窗口中列出 */
export class CollabManagerModal extends Modal {
  constructor(app: App, private readonly plugin: OSSPlugin) {
    super(app);
  }

  onOpen(): void {
    this.modalEl.addClass("oss-manager-modal");
    this.titleEl.setText(this.plugin.t("sidebar.invitations"));
    void this.load();
  }

  onClose(): void {
    this.contentEl.empty();
  }

  /** 刷新协作关系；两个分区只渲染标题行，条目由各自的查看按钮打开详情窗口 */
  private async load(): Promise<void> {
    this.contentEl.empty();
    this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.loading") });
    await this.plugin.collabManager.refresh();
    this.contentEl.empty();
    const collaborations = this.plugin.collabManager.getCollaborations();
    const username = this.plugin.settings.username;
    const incoming = collaborations.filter((entry) => entry.collaborator_username === username && entry.status !== "revoked");
    const outgoing = collaborations.filter((entry) => entry.collaborator_username !== username && entry.status !== "revoked");
    if (incoming.length === 0 && outgoing.length === 0) {
      this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("collab.empty") });
      return;
    }
    if (incoming.length > 0) {
      this.renderSectionHeader("collab.sectionIncoming", "incoming", this.incomingCountLabel(incoming), () =>
        new CollabIncomingModal(this.app, this.plugin, incoming).open()
      );
    }
    if (outgoing.length > 0) {
      this.renderSectionHeader("collab.sectionOutgoing", "outgoing", this.outgoingCountLabel(outgoing), () =>
        new CollabOutgoingModal(this.app, this.plugin, outgoing).open()
      );
    }
  }

  /** 收到的邀请数量：待处理数与正协作数分别计数 */
  private incomingCountLabel(entries: readonly CollabEntry[]): string {
    const pending = entries.filter((entry) => entry.status === "pending").length;
    return this.plugin.t("collab.countIncoming", { pending, accepted: entries.length - pending });
  }

  /** 发出的邀请数量：按文章去重，同一文件的多个协作者只计一次 */
  private outgoingCountLabel(entries: readonly CollabEntry[]): string {
    const articles = new Set(entries.map((entry) => `${entry.vault_id}:${entry.file_id}`));
    return this.plugin.t("collab.countOutgoing", { count: articles.size });
  }

  /** 渲染分区标题行：标题、数量与打开对应窗口的查看按钮 */
  private renderSectionHeader(titleKey: TranslationKey, variant: string, count: string, openDetail: () => void): void {
    const header = this.contentEl.createDiv({ cls: `oss-collab-section-header oss-collab-section-header--${variant}` });
    header.createSpan({ cls: "oss-collab-section-title", text: this.plugin.t(titleKey) });
    header.createSpan({ cls: "oss-collab-section-count", text: count });
    header.createEl("button", { cls: "oss-collab-view", text: this.plugin.t("collab.view") })
      .addEventListener("click", openDetail);
  }
}
