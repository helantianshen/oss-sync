import { App, Modal, Notice, Setting, TFile } from "obsidian";
import { diff_match_patch } from "diff-match-patch";
import type OSSPlugin from "./main";
import type { OSSApiClient } from "./api";

export type ConflictResolution = "accept_remote" | "force_local" | "keep_both";

export class ConflictModal extends Modal {
  private remoteContent = "";
  private localContent = "";
  private diffHtml = "";

  constructor(
    app: App,
    private plugin: OSSPlugin,
    private api: OSSApiClient,
    private file: TFile,
    remoteContent: string,
    private onResolved: (r: ConflictResolution) => Promise<void>
  ) {
    super(app);
    this.remoteContent = remoteContent;
  }

  async onOpen(): Promise<void> {
    const { contentEl, titleEl } = this;
    titleEl.setText(`冲突解决：${this.file.path}`);

    this.localContent = await this.app.vault.read(this.file);
    this.diffHtml = this.buildDiff(this.localContent, this.remoteContent);

    const preview = contentEl.createDiv({ cls: "oss-diff-preview" });
    preview.style.cssText =
      "max-height:400px;overflow:auto;border:1px solid var(--background-modifier-border);" +
      "padding:8px;font-family:var(--font-monospace);font-size:12px;white-space:pre-wrap;";
    preview.innerHTML = this.diffHtml;

    new Setting(contentEl)
      .setName("选择解决方式")
      .setHeading();

    new Setting(contentEl)
      .setName("接受云端覆盖本地")
      .setDesc("用云端最新版本替换本地文件")
      .addButton((b) =>
        b.setButtonText("Accept Remote").onClick(() => this.resolve("accept_remote"))
      );

    new Setting(contentEl)
      .setName("强制本地覆盖云端")
      .setDesc("用本地版本上传覆盖服务端")
      .addButton((b) =>
        b.setButtonText("Force Push Local").setWarning().onClick(() => this.resolve("force_local"))
      );

    new Setting(contentEl)
      .setName("保留双方并产生副本")
      .setDesc("本地修改另存为 _conflict_时间戳.md，原文件取云端版本")
      .addButton((b) =>
        b.setButtonText("Keep Both").onClick(() => this.resolve("keep_both"))
      );

    new Setting(contentEl)
      .addButton((b) =>
        b.setButtonText("稍后处理").setWarning().onClick(() => this.close())
      );
  }

  onClose(): void {
    this.contentEl.empty();
  }

  private async resolve(r: ConflictResolution): Promise<void> {
    try {
      await this.onResolved(r);
      this.close();
    } catch (e) {
      new Notice("OSS 冲突解决失败: " + (e as Error).message);
    }
  }

  private buildDiff(local: string, remote: string): string {
    const dmp = new diff_match_patch();
    const diffs = dmp.diff_main(local, remote);
    dmp.diff_cleanupSemantic(diffs);
    let html = "";
    for (const [op, text] of diffs) {
      const esc = this.escape(text);
      if (op === 0) {
        html += esc;
      } else if (op === 1) {
        html += `<ins style="background:#dfd;color:#050">${esc}</ins>`;
      } else {
        html += `<del style="background:#fdd;color:#500">${esc}</del>`;
      }
    }
    return html;
  }

  private escape(s: string): string {
    return s
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
  }
}
