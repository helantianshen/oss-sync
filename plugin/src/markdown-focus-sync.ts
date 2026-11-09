import { ViewPlugin, type EditorView, type ViewUpdate } from "@codemirror/view";
import { editorInfoField, MarkdownView, type TFile } from "obsidian";

export function markdownFocusSync(onLeave: (file: TFile, markdown: MarkdownView) => void) {
  return ViewPlugin.fromClass(class {
    private edited?: { file: TFile; markdown: MarkdownView };

    constructor(private readonly editor: EditorView) {}

    update(update: ViewUpdate): void {
      const markdown = update.state.field(editorInfoField, false);
      if (this.edited && (!this.editor.hasFocus || markdown?.file !== this.edited.file)) {
        this.flush();
      }
      if (update.docChanged && this.editor.hasFocus && markdown instanceof MarkdownView && markdown.file) {
        this.edited = { file: markdown.file, markdown };
      }
    }

    destroy(): void {
      this.flush();
    }

    private flush(): void {
      const edited = this.edited;
      this.edited = undefined;
      // 保存可能触发编辑器更新，等当前更新或销毁结束再调用。
      if (edited) queueMicrotask(() => onLeave(edited.file, edited.markdown));
    }
  });
}
