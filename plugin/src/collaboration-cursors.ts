import { StateEffect, type Extension, type Text } from "@codemirror/state";
import { Decoration, type DecorationSet, type EditorView, ViewPlugin, type ViewUpdate, WidgetType } from "@codemirror/view";
import { editorInfoField, setTooltip } from "obsidian";
import type { CollaborationCursor } from "./api";

interface CursorTarget {
  readonly vaultID: string;
  readonly fileID: number;
  readonly canReadOnly: boolean;
}

interface CursorSource {
  readonly getTarget: (path: string) => CursorTarget | null;
  readonly exchange: (target: CursorTarget, offset: number | null, hash: string) => Promise<readonly CollaborationCursor[]>;
}

const setCursors = StateEffect.define<DecorationSet>();
const refreshInterval = 1_800;
const minimumInterval = 700;

class CursorWidget extends WidgetType {
  constructor(private readonly cursor: CollaborationCursor) { super(); }

  eq(other: CursorWidget): boolean {
    return this.cursor.user_id === other.cursor.user_id && this.cursor.username === other.cursor.username;
  }

  toDOM(): HTMLElement {
    const marker = createSpan({ cls: "oss-collaboration-cursor" });
    const caret = marker.createSpan({ cls: "oss-collaboration-caret" });
    setTooltip(caret, this.cursor.username, { placement: "top" });
    caret.style.setProperty("--oss-cursor-color", `hsl(${this.cursor.user_id * 137.508 % 360}, 75%, 57%)`);
    return marker;
  }
}

export function collaborationCursors(source: CursorSource): Extension {
  return ViewPlugin.define((view) => new CursorView(view, source), {
    decorations: (instance) => instance.decorations,
  });
}

class CursorView {
  decorations = Decoration.none;
  private timer: number | undefined;
  private scheduledAt = 0;
  private lastRequest = 0;
  private busy = false;
  private closed = false;
  private hashed?: { doc: Text; hash: string };
  private lastPath: string | undefined;
  private readonly window: Window;

  constructor(private readonly view: EditorView, private readonly source: CursorSource) {
    this.window = view.dom.win;
    this.lastPath = this.path;
    this.schedule(0);
  }

  update(update: ViewUpdate): void {
    for (const transaction of update.transactions) {
      for (const effect of transaction.effects) {
        if (effect.is(setCursors)) this.decorations = effect.value;
      }
    }
    const path = this.path;
    const pathChanged = path !== this.lastPath;
    this.lastPath = path;
    if (update.docChanged || pathChanged) this.decorations = Decoration.none;
    if (update.focusChanged || update.docChanged || update.selectionSet || pathChanged) this.schedule(150);
  }

  destroy(): void {
    this.closed = true;
    this.window.clearTimeout(this.timer);
  }

  private get path(): string | undefined {
    return this.view.state.field(editorInfoField, false)?.file?.path;
  }

  private get visible(): boolean {
    return !this.view.dom.ownerDocument.hidden && this.view.dom.getClientRects().length > 0;
  }

  private schedule(delay: number): void {
    if (this.closed || this.busy) return;
    const now = Date.now();
    const next = Math.max(now + delay, this.lastRequest + minimumInterval);
    if (this.timer !== undefined && this.scheduledAt <= next) return;
    this.window.clearTimeout(this.timer);
    this.scheduledAt = next;
    this.timer = this.window.setTimeout(() => {
      this.timer = undefined;
      void this.refresh();
    }, next - now);
  }

  private async refresh(): Promise<void> {
    if (this.closed) return;
    const path = this.path;
    const target = path && this.visible ? this.source.getTarget(path) : null;
    const { doc } = this.view.state;
    if (!path || !target || (!this.view.hasFocus && !target.canReadOnly) || doc.length > 32 * 1024 * 1024) {
      this.draw(Decoration.none);
      this.schedule(refreshInterval);
      return;
    }
    let offset: number | null = null;
    this.busy = true;
    try {
      if (this.hashed?.doc !== doc) {
        const bytes = new TextEncoder().encode(doc.toString());
        const digest = new Uint8Array(await this.window.crypto.subtle.digest("SHA-256", bytes));
        const hash = Array.from(digest, (value) => value.toString(16).padStart(2, "0")).join("");
        this.hashed = { doc, hash };
      }
      const { hash } = this.hashed;
      if (!this.isCurrent(path, doc, target)) return;
      offset = this.view.hasFocus ? this.view.state.selection.main.head : null;
      if (offset === null && !target.canReadOnly) return;
      this.lastRequest = Date.now();
      const cursors = await this.source.exchange(target, offset, hash);
      if (!this.isCurrent(path, doc, target)) return;
      const ranges = cursors
        .filter((cursor) => cursor.hash === hash && Number.isInteger(cursor.offset) && cursor.offset >= 0 && cursor.offset <= doc.length)
        .map((cursor) => Decoration.widget({ widget: new CursorWidget(cursor), side: 1 }).range(cursor.offset));
      this.draw(Decoration.set(ranges, true));
    } catch {
      if (this.isCurrent(path, doc, target)) this.draw(Decoration.none);
    } finally {
      this.busy = false;
      const currentOffset = this.view.hasFocus ? this.view.state.selection.main.head : null;
      const stillCurrent = this.view.state.doc === doc && currentOffset === offset;
      this.schedule(stillCurrent ? refreshInterval : 150);
    }
  }

  private isCurrent(path: string, doc: Text, target: CursorTarget): boolean {
    if (this.closed || !this.visible || this.path !== path || this.view.state.doc !== doc) return false;
    const current = this.source.getTarget(path);
    return current?.vaultID === target.vaultID && current.fileID === target.fileID;
  }

  private draw(decorations: DecorationSet): void {
    if (this.decorations !== Decoration.none || decorations !== Decoration.none) {
      this.view.dispatch({ effects: setCursors.of(decorations) });
    }
  }
}
