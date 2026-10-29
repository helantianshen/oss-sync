import { App, Modal, Notice, Setting, TFile, TFolder } from "obsidian";
import type OSSPlugin from "./main";
import { renderShareGuideEntry } from "./share-guide-modal";

/** 生成分享链接的模态框；支持文件与文件夹目标，可配置是否允许复制*/
export class ShareModal extends Modal {
  private isFolder: boolean;
  private targetPath: string;
  private allowCopy = true;
  private recursive = false;

  constructor(app: App, private plugin: OSSPlugin, private file: TFile | TFolder) {
    super(app);
    this.isFolder = file instanceof TFolder;
    this.targetPath = file.path;
  }

  onOpen(): void {
    const { contentEl, titleEl } = this;
    titleEl.setText(this.plugin.t("share.title"));

    new Setting(contentEl)
      .setName(this.plugin.t("share.target"))
      .setDesc(this.isFolder ? this.plugin.t("share.folder") : this.plugin.t("share.file", { path: this.targetPath }))
      .addText(() => {});

    new Setting(contentEl)
      .setName(this.plugin.t("share.allowCopy"))
      .addToggle((t) => t.setValue(this.allowCopy).onChange((v) => (this.allowCopy = v)));

    if (!this.isFolder) {
      new Setting(contentEl)
        .setName(this.plugin.t("share.recursive"))
        .setDesc(this.plugin.t("share.recursiveDesc"))
        .addToggle((t) => t.setValue(this.recursive).onChange((v) => (this.recursive = v)));
    }

    new Setting(contentEl)
      .setName(this.plugin.t("share.hint"))
      .setDesc(this.plugin.t("share.hintDesc"));

    renderShareGuideEntry(contentEl, this.app, this.plugin);

    new Setting(contentEl)
      .addButton((btn) =>
        btn
          .setButtonText(this.plugin.t("common.cancel"))
          .setWarning()
          .onClick(() => this.close())
      )
      .addButton((btn) =>
        btn.setButtonText(this.plugin.t("share.generate")).onClick(async () => {
          await this.create();
        })
      );
  }

  onClose(): void {
    this.contentEl.empty();
  }

  private async create(): Promise<void> {
    try {
      const res = await this.plugin.api.createShare({
        targetPath: this.targetPath,
        isFolder: this.isFolder,
        allowCopy: this.allowCopy,
        recursiveBacklinks: this.recursive,
      });
      const fullUrl = this.plugin.settings.serverUrl.replace(/\/$/, "") + res.url;
      await navigator.clipboard.writeText(fullUrl);
      new Notice(this.plugin.t("share.copied", { url: fullUrl }), 8000);
      if (res.extra && res.extra.length > 0) {
        new Notice(this.plugin.t("share.extra", { count: res.extra.length }), 6000);
      }
      this.plugin.sidebarView?.reloadShares();
      this.close();
    } catch (error) {
      const message = error instanceof Error ? error.message : this.plugin.t("common.unknownError");
      new Notice(this.plugin.t("share.failed", { error: message }));
    }
  }
}
