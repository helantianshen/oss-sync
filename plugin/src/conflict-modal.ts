import { App, Modal, Notice, Setting, TFile } from "obsidian";
import type OSSPlugin from "./main";
import type { OSSApiClient } from "./api";
import { buildConflictDiff, LineCapacityExceededError } from "./conflict-diff";
import {
  buildMergeRegions,
  hasConflictRegion,
  resolveOrderedMerge,
  type BlockOrder,
  type MergeRegion,
} from "./text-merge";

/** 解决方式；accept_remote 采用远端，force_local 采用本地，keep_both 保留两份 */
export type ConflictResolution =
  | "accept_remote"
  | "force_local"
  | "keep_both"
  | { kind: "ordered_merge"; content: string };

/** 冲突解决弹窗；有祖先文本时提供有序合并视图，否则退化为经典差异视图 */
export class ConflictModal extends Modal {
  private remoteContent = "";
  private resolving = false;

  constructor(
    app: App,
    private plugin: OSSPlugin,
    private api: OSSApiClient,
    private file: TFile,
    remoteContent: string,
    private onResolved: (r: ConflictResolution) => Promise<void>,
    private readonly options: { baseText?: string | null; onClose?: () => void; binary?: boolean } = {},
  ) {
    super(app);
    this.remoteContent = remoteContent;
  }

  async onOpen(): Promise<void> {
    const { contentEl, titleEl } = this;
    this.modalEl.addClass("oss-conflict-modal");
    titleEl.empty();
    appendTextWithPathBreaks(titleEl, this.plugin.t("conflict.title", { path: this.file.path }));

    if (this.options.binary) {
      contentEl.createDiv({ text: this.plugin.t("conflict.binaryDescription") });
      this.renderChoices();
      return;
    }

    const preview = contentEl.createDiv({ cls: "oss-diff-preview" });
    const localContent = await this.app.vault.read(this.file);
    let orderedRegions: MergeRegion[] | null = null;
    let orderedOrders: BlockOrder[] = [];
    let finalPreviewEl: HTMLElement | null = null;
    const baseText = this.options.baseText ?? null;
    if (baseText !== null && baseText !== undefined) {
      try {
        const regions = buildMergeRegions(baseText, localContent, this.remoteContent);
        if (hasConflictRegion(regions)) {
          orderedRegions = regions;
          orderedOrders = regions
            .filter((r) => r.kind === "conflict")
            .map(() => "local_first" as BlockOrder);
        }
      } catch {
        orderedRegions = null;
      }
    }
    if (orderedRegions) {
      const finalWrap = contentEl.createDiv({ cls: "oss-merge-final" });
      finalWrap.createEl("h4", { text: this.plugin.t("conflict.orderedFinalPreview"), cls: "oss-merge-final-title" });
      finalPreviewEl = finalWrap.createDiv({ cls: "oss-merge-final-preview" });
      renderFinalPreview(finalPreviewEl, orderedRegions, orderedOrders, this.plugin);
      renderOrderedBlocks(preview, orderedRegions, orderedOrders, this.plugin, (idx, order) => {
        orderedOrders[idx] = order;
        if (finalPreviewEl) {
          renderFinalPreview(finalPreviewEl, orderedRegions!, orderedOrders, this.plugin);
        }
      });
      new Setting(contentEl)
        .setName(this.plugin.t("conflict.orderedMerge"))
        .setDesc(this.plugin.t("conflict.orderedMergeDesc"))
        .addButton((b) =>
          b
            .setButtonText(this.plugin.t("conflict.orderedMergeButton"))
            .setCta()
            .onClick(() => {
              const content = resolveOrderedMerge(orderedRegions!, (i) => orderedOrders[i]);
              void this.resolve({ kind: "ordered_merge", content });
            }),
        );
    }
    // 有序合并区域和经典差异视图互斥
    if (!orderedRegions) {
      try {
        const rows = buildConflictDiff(localContent, this.remoteContent);
        if (rows.length === 0) {
          preview.createDiv({ cls: "oss-diff-empty", text: this.plugin.t("conflict.noChanges") });
        }
        for (const row of rows) {
          switch (row.kind) {
            case "omitted":
              preview.createDiv({
                cls: "oss-diff-row is-omitted",
                text: this.plugin.t("conflict.omittedLines", { count: row.count }),
              });
              break;
            case "context":
            case "removed":
            case "added": {
              const diffRow = preview.createDiv({ cls: `oss-diff-row is-${row.kind}` });
              const marker = row.kind === "removed" ? "-" : row.kind === "added" ? "+" : "";
              diffRow.createSpan({ cls: "oss-diff-marker", text: marker });
              const textSpan = diffRow.createSpan({ cls: "oss-diff-text" });
              appendTextWithPathBreaks(textSpan, row.text);
              break;
            }
            default:
              assertNever(row);
          }
        }
      } catch (error: unknown) {
        const message = error instanceof LineCapacityExceededError
          ? error.message
          : error instanceof Error
            ? error.message
            : this.plugin.t("common.unknownError");
        const failureText = this.plugin.t("conflict.diffFailed", { error: message });
        new Notice(failureText);
        preview.createDiv({ cls: "oss-diff-empty", text: failureText });
      }
    } else {
      // 有序块下方保留精简的差异提示行
      try {
        const rows = buildConflictDiff(localContent, this.remoteContent);
        if (rows.length > 0) {
          preview.createDiv({ cls: "oss-diff-row is-omitted", text: this.plugin.t("conflict.diffReference") });
        }
      } catch {
        // 参考差异失败不影响下方的冲突解决选项
      }
    }

    this.renderChoices();
  }

  private renderChoices(): void {
    const { contentEl } = this;
    new Setting(contentEl)
      .setName(this.plugin.t("conflict.choose"))
      .setHeading();

    new Setting(contentEl)
      .setName(this.plugin.t("conflict.acceptRemote"))
      .setDesc(this.plugin.t("conflict.acceptRemoteDesc"))
      .addButton((b) =>
        b.setButtonText(this.plugin.t("conflict.acceptRemoteButton")).onClick(() => this.resolve("accept_remote"))
      );

    new Setting(contentEl)
      .setName(this.plugin.t("conflict.forceLocal"))
      .setDesc(this.plugin.t("conflict.forceLocalDesc"))
      .addButton((b) =>
        b.setButtonText(this.plugin.t("conflict.forceLocalButton")).setWarning().onClick(() => this.resolve("force_local"))
      );

    new Setting(contentEl)
      .setName(this.plugin.t("conflict.keepBoth"))
      .setDesc(this.plugin.t("conflict.keepBothDesc"))
      .addButton((b) =>
        b.setButtonText(this.plugin.t("conflict.keepBothButton")).onClick(() => this.resolve("keep_both"))
      );

    new Setting(contentEl)
      .addButton((b) =>
        b.setButtonText(this.plugin.t("conflict.later")).setWarning().onClick(() => this.close())
      );
  }

  onClose(): void {
    this.contentEl.empty();
    this.plugin.sidebarView?.refresh();
    this.options.onClose?.();
  }

  /** 提交解决方式；resolving 期间重复点击被忽略，失败时保留弹窗供重试 */
  private async resolve(r: ConflictResolution): Promise<void> {
    if (this.resolving) return;
    this.resolving = true;
    try {
      await this.onResolved(r);
      this.close();
    } catch (error) {
      const message = error instanceof Error ? error.message : this.plugin.t("common.unknownError");
      new Notice(this.plugin.t("conflict.failed", { error: message }));
    } finally {
      this.resolving = false;
    }
  }
}

/** 渲染压缩后的上下文；靠冲突的边界保留更多行，远离冲突的边界大幅省略 */
function renderCompressedContext(
  container: HTMLElement,
  lines: readonly string[],
  plugin: OSSPlugin,
  isFirst = false,
  isLast = false,
): void {
  // context 不足 5 行时全部展示，不做省略
  if (lines.length <= 5) {
    const pre = container.createEl("pre", { cls: "oss-merge-text is-context" });
    appendTextWithPathBreaks(pre, lines.join("\n"));
    return;
  }
  if (isFirst && !isLast) {
    // 首段仅保留靠近冲突的尾部 5 行
    const tail = lines.slice(-5).join("\n");
    const omitted = container.createDiv({ cls: "oss-diff-row is-omitted" });
    omitted.setText(plugin.t("conflict.omittedLines", { count: lines.length - 5 }));
    const preTail = container.createEl("pre", { cls: "oss-merge-text is-context" });
    appendTextWithPathBreaks(preTail, tail);
    return;
  }
  if (!isFirst && isLast) {
    // 尾段仅保留靠近冲突的头部 5 行
    const head = lines.slice(0, 5).join("\n");
    const preHead = container.createEl("pre", { cls: "oss-merge-text is-context" });
    appendTextWithPathBreaks(preHead, head);
    const omitted = container.createDiv({ cls: "oss-diff-row is-omitted" });
    omitted.setText(plugin.t("conflict.omittedLines", { count: lines.length - 5 }));
    return;
  }
  if (isFirst && isLast) {
    const head = lines.slice(0, 2).join("\n");
    const tail = lines.slice(-2).join("\n");
    const preHead = container.createEl("pre", { cls: "oss-merge-text is-context" });
    appendTextWithPathBreaks(preHead, head);
    const omitted = container.createDiv({ cls: "oss-diff-row is-omitted" });
    omitted.setText(plugin.t("conflict.omittedLines", { count: lines.length - 4 }));
    const preTail = container.createEl("pre", { cls: "oss-merge-text is-context" });
    appendTextWithPathBreaks(preTail, tail);
    return;
  }
  const head = lines.slice(0, 2).join("\n");
  const tail = lines.slice(-3).join("\n");
  const preHead = container.createEl("pre", { cls: "oss-merge-text is-context" });
  appendTextWithPathBreaks(preHead, head);
  const omitted = container.createDiv({ cls: "oss-diff-row is-omitted" });
  omitted.setText(plugin.t("conflict.omittedLines", { count: lines.length - 5 }));
  const preTail = container.createEl("pre", { cls: "oss-merge-text is-context" });
  appendTextWithPathBreaks(preTail, tail);
}

/** 渲染有序合并块；每个冲突块可切换本地或远端在前，索引按冲突序号而非区域序号 */
function renderOrderedBlocks(
  container: HTMLElement,
  regions: readonly MergeRegion[],
  orders: readonly BlockOrder[],
  plugin: OSSPlugin,
  onToggle: (blockIndex: number, order: BlockOrder) => void,
): void {
  let conflictIdx = 0;
  for (let idx = 0; idx < regions.length; idx++) {
    const region = regions[idx];
    if (region.kind === "resolved") {
      if (region.lines.length === 0) continue;
      const ctx = container.createDiv({ cls: "oss-merge-context" });
      ctx.createDiv({ cls: "oss-merge-label", text: plugin.t("conflict.contextBlock") });
      const isFirst = idx === 0;
      const isLast = idx === regions.length - 1;
      renderCompressedContext(ctx, region.lines, plugin, isFirst, isLast);
    } else {
      const idx = conflictIdx;
      const block = container.createDiv({ cls: "oss-merge-block" });
      block.createDiv({ cls: "oss-merge-label", text: plugin.t("conflict.conflictBlock", { index: idx + 1 }) });
      const localWrap = block.createDiv({ cls: "oss-merge-side is-local" });
      localWrap.createDiv({ cls: "oss-merge-side-label", text: plugin.t("conflict.localBlock") });
      const localPre = localWrap.createEl("pre", { cls: "oss-merge-text is-removed" });
      appendTextWithPathBreaks(localPre, region.localLines.join("\n"));
      const remoteWrap = block.createDiv({ cls: "oss-merge-side is-remote" });
      remoteWrap.createDiv({ cls: "oss-merge-side-label", text: plugin.t("conflict.remoteBlock") });
      const remotePre = remoteWrap.createEl("pre", { cls: "oss-merge-text is-added" });
      appendTextWithPathBreaks(remotePre, region.remoteLines.join("\n"));
      const actions = block.createDiv({ cls: "oss-merge-actions" });
      const localBtn = actions.createEl("button", { text: plugin.t("conflict.localFirst"), cls: "oss-merge-order-btn" });
      const remoteBtn = actions.createEl("button", { text: plugin.t("conflict.remoteFirst"), cls: "oss-merge-order-btn" });
      const syncActive = (active: BlockOrder): void => {
        localBtn.toggleClass("is-active", active === "local_first");
        remoteBtn.toggleClass("is-active", active === "remote_first");
      };
      syncActive(orders[idx] ?? "local_first");
      localBtn.addEventListener("click", () => {
        syncActive("local_first");
        onToggle(idx, "local_first");
      });
      remoteBtn.addEventListener("click", () => {
        syncActive("remote_first");
        onToggle(idx, "remote_first");
      });
      conflictIdx += 1;
    }
  }
}

/** 渲染合并后的最终预览，随冲突块顺序切换实时更新 */
function renderFinalPreview(
  container: HTMLElement,
  regions: readonly MergeRegion[],
  orders: readonly BlockOrder[],
  plugin: OSSPlugin,
): void {
  container.empty();
  container.addClass("oss-merge-final-preview--colored");
  let conflictIdx = 0;
  for (let rIdx = 0; rIdx < regions.length; rIdx++) {
    const region = regions[rIdx];
    if (region.kind === "resolved") {
      if (region.lines.length === 0) continue;
      const block = container.createDiv({ cls: "oss-final-block is-context" });
      const isFirst = rIdx === 0;
      const isLast = rIdx === regions.length - 1;
      if (region.lines.length <= 5) {
        const pre = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(pre, region.lines.join("\n"));
      } else if (isFirst && !isLast) {
        const tail = region.lines.slice(-5).join("\n");
        const omitted = block.createDiv({ cls: "oss-diff-row is-omitted" });
        omitted.setText(plugin.t("conflict.omittedLines", { count: region.lines.length - 5 }));
        const preTail = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preTail, tail);
      } else if (!isFirst && isLast) {
        const head = region.lines.slice(0, 5).join("\n");
        const preHead = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preHead, head);
        const omitted = block.createDiv({ cls: "oss-diff-row is-omitted" });
        omitted.setText(plugin.t("conflict.omittedLines", { count: region.lines.length - 5 }));
      } else if (isFirst && isLast) {
        const head = region.lines.slice(0, 2).join("\n");
        const tail = region.lines.slice(-2).join("\n");
        const preHead = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preHead, head);
        const omitted = block.createDiv({ cls: "oss-diff-row is-omitted" });
        omitted.setText(plugin.t("conflict.omittedLines", { count: region.lines.length - 4 }));
        const preTail = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preTail, tail);
      } else {
        const head = region.lines.slice(0, 2).join("\n");
        const tail = region.lines.slice(-3).join("\n");
        const preHead = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preHead, head);
        const omitted = block.createDiv({ cls: "oss-diff-row is-omitted" });
        omitted.setText(plugin.t("conflict.omittedLines", { count: region.lines.length - 5 }));
        const preTail = block.createEl("pre", { cls: "oss-merge-text is-context" });
        appendTextWithPathBreaks(preTail, tail);
      }
    } else {
      const order = orders[conflictIdx] ?? "local_first";
      conflictIdx += 1;
      const block = container.createDiv({ cls: "oss-final-block is-conflict" });
      const firstLines = order === "local_first" ? region.localLines : region.remoteLines;
      const secondLines = order === "local_first" ? region.remoteLines : region.localLines;
      const firstCls = order === "local_first" ? "is-removed" : "is-added";
      const secondCls = order === "local_first" ? "is-added" : "is-removed";
      if (firstLines.length > 0) {
        const pre1 = block.createEl("pre", { cls: `oss-merge-text ${firstCls}` });
        appendTextWithPathBreaks(pre1, firstLines.join("\n"));
      }
      if (secondLines.length > 0) {
        const pre2 = block.createEl("pre", { cls: `oss-merge-text ${secondCls}` });
        appendTextWithPathBreaks(pre2, secondLines.join("\n"));
      }
    }
  }
}

/** 按路径分隔符插入软换行点，避免长路径撑破容器 */
function appendTextWithPathBreaks(element: HTMLElement, text: string): void {
  for (const part of text.split(/(?=[/\\])/)) {
    if (part.length === 0) continue;
    if (part[0] === "/" || part[0] === "\\") {
      element.createEl("wbr");
    }
    element.appendChild(document.createTextNode(part));
  }
}

/** 穷尽检查；新增差异行类型未处理时应抛错 */
function assertNever(value: never): never {
  throw new Error(`Unexpected conflict diff row: ${String(value)}`);
}
