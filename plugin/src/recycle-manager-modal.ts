import { ConfirmModal } from "./confirm-modal";
import { App, Modal, Notice } from "obsidian";
import type { RecycleBinFile } from "./api";
import type OSSPlugin from "./main";

/** 回收站管理弹窗；恢复后重新拉取列表 */
export class RecycleManagerModal extends Modal {
  constructor(app: App, private readonly plugin: OSSPlugin) {
    super(app);
  }

  onOpen(): void {
    this.modalEl.addClass("oss-manager-modal");
    this.titleEl.setText(this.plugin.t("recycle.title"));
    void this.load();
  }

  onClose(): void {
    this.contentEl.empty();
  }

  /** 拉取回收站列表并渲染；未绑定仓库时给出引导 */
  private async load(): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    this.contentEl.empty();
    if (!vaultID) {
      this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("recycle.bindFirst") });
      return;
    }
    this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("recycle.loading") });
    try {
      const result = await this.plugin.api.recycleList(vaultID);
      this.contentEl.empty();
      if (result.files.length === 0) {
        this.contentEl.createDiv({ cls: "oss-sidebar-empty", text: this.plugin.t("recycle.empty") });
        return;
      }
      for (const file of result.files) {
        this.renderFile(vaultID, file);
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : this.plugin.t("common.unknownError");
      this.contentEl.empty();
      this.contentEl.createDiv({
        cls: "oss-sidebar-empty",
        text: this.plugin.t("recycle.loadFailed", { error: message }),
      });
    }
  }

  /** 渲染单行回收站条目；can_restore 为 false 时禁用恢复按钮 */
  private renderFile(vaultID: string, file: RecycleBinFile): void {
    const row = this.contentEl.createDiv({ cls: "oss-sidebar-recycle-row" });
    row.createDiv({ cls: "oss-manager-path", text: file.path });
    row.createDiv({
      cls: "oss-manager-meta",
      text: this.plugin.t("recycle.meta", {
        size: file.size,
        expires: new Date(file.expires_at).toLocaleString(),
      }),
    });
    const actions = row.createDiv({ cls: "oss-sidebar-actions" });
    const restore = actions.createEl("button", {
      cls: "oss-recycle-restore",
      text: this.plugin.t("recycle.restore"),
    });
    if (!file.can_restore) {
      restore.setAttribute("disabled", "true");
    }
    restore.addEventListener("click", () => void this.restore(vaultID, file.id));
    actions.createEl("button", {
      cls: "oss-recycle-delete mod-warning",
      text: this.plugin.t("recycle.delete"),
    }).addEventListener("click", () => void this.delete(vaultID, file.id, file.path));
  }

  private async restore(vaultID: string, fileID: number): Promise<void> {
    try {
      await this.plugin.api.recycleRestore(vaultID, fileID);
      await this.load();
    } catch (error) {
      this.showActionError(error);
    }
  }

  /** 彻底删除前需用户确认，防止误操作不可恢复 */
  private async delete(vaultID: string, fileID: number, path: string): Promise<void> {
    if (!await ConfirmModal.ask(this.app, this.plugin.t("recycle.deleteConfirm", { path }), this.plugin.t("recycle.delete"), this.plugin.t("common.cancel"))) return;
    try {
      await this.plugin.api.recycleDelete(vaultID, fileID);
      await this.load();
    } catch (error) {
      this.showActionError(error);
    }
  }

  private showActionError(error: unknown): void {
    const message = error instanceof Error ? error.message : this.plugin.t("common.unknownError");
    new Notice(this.plugin.t("recycle.actionFailed", { error: message }));
  }
}
