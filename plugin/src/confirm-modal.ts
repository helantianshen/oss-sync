import { App, Modal, Setting } from "obsidian";

/** 破坏性操作的确认窗口；关闭或取消均返回 false */
export class ConfirmModal extends Modal {
  private confirmed = false;

  constructor(
    app: App,
    private readonly message: string,
    private readonly confirmLabel: string,
    private readonly cancelLabel: string,
    private readonly resolve: (confirmed: boolean) => void,
  ) {
    super(app);
  }

  onOpen(): void {
    this.contentEl.createEl("p", { text: this.message });
    new Setting(this.contentEl)
      .addButton((button) => button.setButtonText(this.cancelLabel).onClick(() => this.close()))
      .addButton((button) => button.setButtonText(this.confirmLabel).setWarning().onClick(() => {
        this.confirmed = true;
        this.close();
      }));
  }

  onClose(): void {
    this.contentEl.empty();
    this.resolve(this.confirmed);
  }

  /** 等待用户选择，保持调用方的异步操作顺序 */
  static ask(app: App, message: string, confirmLabel: string, cancelLabel: string): Promise<boolean> {
    return new Promise((resolve) => new ConfirmModal(app, message, confirmLabel, cancelLabel, resolve).open());
  }
}
