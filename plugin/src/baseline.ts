import type { Vault } from "obsidian";

export const BASELINE_FILENAME = ".oss-sync-state.json";

export interface BaselineEntry {
  serverRevision: number;
  serverHash: string;
  serverDeleted: boolean;
  localHash: string;
  localMTime: number;
  localSize: number;
}

export interface PendingOperation {
  id: string;
  kind: "upsert" | "delete" | "rename";
  path: string;
  oldPath?: string;
  createdAt: number;
}

export interface ConflictEntry {
  path: string;
  localHash: string;
  remoteRevision: number;
  remoteHash: string;
  remoteDeleted: boolean;
  remoteMTime: number;
  remoteSize: number;
  remoteType: "markdown" | "attachment" | "config";
  detectedAt: number;
}

interface SyncStateFile {
  version: 2;
  vaultId: string;
  cursor: number;
  files: Record<string, BaselineEntry>;
  pending: PendingOperation[];
  conflicts: ConflictEntry[];
}

function emptyState(): SyncStateFile {
  return {
    version: 2,
    vaultId: "",
    cursor: 0,
    files: {},
    pending: [],
    conflicts: [],
  };
}

export class BaselineStore {
  private data: SyncStateFile = emptyState();
  private loaded = false;
  private loadPromise: Promise<void> | null = null;
  private saveChain: Promise<void> = Promise.resolve();

  constructor(private vault: Vault) {}

  async load(): Promise<void> {
    if (this.loaded) return;
    if (!this.loadPromise) {
      this.loadPromise = this.loadFromAdapter();
    }
    await this.loadPromise;
  }

  private async loadFromAdapter(): Promise<void> {
    try {
      if (!(await this.vault.adapter.exists(BASELINE_FILENAME))) {
        this.data = emptyState();
        return;
      }
      const parsed = JSON.parse(await this.vault.adapter.read(BASELINE_FILENAME));
      if (parsed?.version === 2 && typeof parsed.files === "object") {
        this.data = {
          ...emptyState(),
          ...parsed,
          files: parsed.files ?? {},
          pending: Array.isArray(parsed.pending) ? parsed.pending : [],
          conflicts: Array.isArray(parsed.conflicts) ? parsed.conflicts : [],
        };
      } else {
        this.data = emptyState();
      }
    } catch {
      this.data = emptyState();
    } finally {
      this.loaded = true;
    }
  }

  async save(): Promise<void> {
    await this.load();
    const raw = JSON.stringify(this.data);
    const pending = this.saveChain.then(() =>
      this.vault.adapter.write(BASELINE_FILENAME, raw)
    );
    this.saveChain = pending.catch(() => undefined);
    await pending;
  }

  bindVault(vaultID: string): boolean {
    if (this.data.vaultId === vaultID) return false;
    this.data = emptyState();
    this.data.vaultId = vaultID;
    return true;
  }

  getVaultID(): string {
    return this.data.vaultId;
  }

  getCursor(): number {
    return this.data.cursor;
  }

  setCursor(cursor: number): void {
    this.data.cursor = Math.max(this.data.cursor, cursor);
  }

  get(path: string): BaselineEntry | null {
    return this.data.files[normalizePath(path)] ?? null;
  }

  set(path: string, entry: BaselineEntry): void {
    this.data.files[normalizePath(path)] = entry;
  }

  remove(path: string): void {
    delete this.data.files[normalizePath(path)];
  }

  paths(): string[] {
    return Object.keys(this.data.files);
  }

  pending(): PendingOperation[] {
    return [...this.data.pending];
  }

  putPending(operation: PendingOperation): void {
    const path = normalizePath(operation.path);
    const oldPath = operation.oldPath ? normalizePath(operation.oldPath) : undefined;

    if (operation.kind === "upsert") {
      const rename = this.data.pending.find((item) => item.kind === "rename" && item.path === path);
      if (rename) return;
    }
    if (operation.kind === "delete") {
      const rename = this.data.pending.find((item) => item.kind === "rename" && item.path === path);
      if (rename?.oldPath) {
        this.data.pending = this.data.pending.filter((item) => item.id !== rename.id);
        this.data.pending.push({
          ...operation,
          path: rename.oldPath,
        });
        return;
      }
    }
    if (operation.kind === "rename" && oldPath) {
      const previousRename = this.data.pending.find(
        (item) => item.kind === "rename" && item.path === oldPath
      );
      if (previousRename?.oldPath) {
        this.data.pending = this.data.pending.filter((item) => item.id !== previousRename.id);
        operation = { ...operation, oldPath: previousRename.oldPath };
      }
    }

    this.data.pending = this.data.pending.filter((item) => {
      if (operation.kind === "rename") {
        return item.path !== path && item.path !== operation.oldPath;
      }
      return item.path !== path;
    });
    this.data.pending.push({
      ...operation,
      path,
      oldPath: operation.oldPath ? normalizePath(operation.oldPath) : undefined,
    });
  }

  removePending(operationID: string): void {
    this.data.pending = this.data.pending.filter((item) => item.id !== operationID);
  }

  removePendingForPath(path: string): void {
    const normalized = normalizePath(path);
    this.data.pending = this.data.pending.filter(
      (item) => item.path !== normalized && item.oldPath !== normalized
    );
  }

  conflicts(): ConflictEntry[] {
    return [...this.data.conflicts];
  }

  putConflict(conflict: ConflictEntry): void {
    this.data.conflicts = this.data.conflicts.filter((item) => item.path !== conflict.path);
    this.data.conflicts.push({ ...conflict, path: normalizePath(conflict.path) });
  }

  removeConflict(path: string): void {
    this.data.conflicts = this.data.conflicts.filter((item) => item.path !== normalizePath(path));
  }

  getConflict(path: string): ConflictEntry | null {
    return this.data.conflicts.find((item) => item.path === normalizePath(path)) ?? null;
  }
}

export function normalizePath(path: string): string {
  return path.replace(/\\/g, "/").replace(/^\.\/+/, "");
}
