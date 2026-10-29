import { App, Modal, Notice } from "obsidian";
import type OSSPlugin from "./main";
import { renderShareRows } from "./share-section";
import { renderShareGuideEntry } from "./share-guide-modal";

/** 全量分享管理模态框；列出当前账号下所有分享并支持行内操作*/
export class ShareManagerModal extends Modal {
  constructor(app: App, private readonly plugin: OSSPlugin) {
    super(app);
  }

  onOpen(): void {
    this.modalEl.addClass("oss-manager-modal");
    this.titleEl.setText(this.plugin.t("sidebar.shares"));
    void this.load();
  }

  onClose(): void {
    this.contentEl.empty();
  }

  private async load(): Promise<void> {
    this.contentEl.empty();
    renderShareGuideEntry(this.contentEl, this.app, this.plugin);
    const listEl = this.contentEl.createDiv();
    listEl.createDiv({
      cls: "oss-sidebar-empty",
      text: this.plugin.t("sidebar.sharesLoading"),
    });
    try {
      const result = await this.plugin.api.listShares();
      listEl.empty();
      renderShareRows(listEl, this.plugin, {
        shares: result.shares,
        onChanged: () => this.load(),
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : this.plugin.t("common.unknownError");
      listEl.empty();
      listEl.createDiv({
        cls: "oss-sidebar-empty",
        text: this.plugin.t("sidebar.sharesLoadFailed", { error: message }),
      });
      new Notice(this.plugin.t("sidebar.sharesLoadFailed", { error: message }));
    }
  }
}
