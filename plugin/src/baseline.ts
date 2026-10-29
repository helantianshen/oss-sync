import type { Vault } from "obsidian";
import {
  cloneCollaborationEntry,
  collaborationKey,
  sanitizeCollaborationEntry,
} from "./collaboration-state.js";
import type { CollaborationBaselineEntry } from "./collaboration-state.js";

export type {
  CollaborationBaselineEntry,
  CollaborationConflict,
  CollaborationPendingOperation,
} from "./collaboration-state.js";

/** 同步基线文件名，位于 vault 根目录并被黑名单忽略 */
export const BASELINE_FILENAME = ".oss-sync-state.json";

/** 单文件基线；baseText 仅在正文可参与三方合并时保留 */
export interface BaselineEntry {
  serverRevision: number;
  serverHash: string;
  serverDeleted: boolean;
  localHash: string;
  localMTime: number;
  localSize: number;
  baseText?: string;
}

/** 待传输操作；rename 的 oldPath 为来源路径 */
export interface PendingOperation {
  id: string;
  kind: "upsert" | "delete" | "rename";
  path: string;
  oldPath?: string;
  createdAt: number;
  source?: "scan";
  waitForPaths?: string[];
}

/** 未解决冲突记录；remoteType 决定是否可做文本处理 */
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
  version: 3;
  vaultId: string;
  cursor: number;
  files: Record<string, BaselineEntry>;
  pending: PendingOperation[];
  conflicts: ConflictEntry[];
  collaborationAccount: string;
  collaborationFiles: Record<string, CollaborationBaselineEntry>;
}

function emptyState(): SyncStateFile {
  return {
    version: 3,
    vaultId: "",
    cursor: 0,
    files: {},
    pending: [],
    conflicts: [],
    collaborationAccount: "",
    collaborationFiles: {},
  };
}

/** 持久化同步基线、待传输队列与冲突，写入按顺序串行完成 */
export class BaselineStore {
  private data: SyncStateFile = emptyState();
  private loaded = false;
  private loadPromise: Promise<void> | null = null;
  private saveChain: Promise<void> = Promise.resolve();

  constructor(private vault: Vault) {}

  /** 加载基线；重复调用复用同一次加载，读取失败按空状态处理 */
  async load(): Promise<void> {
    if (this.loaded) return;
    if (!this.loadPromise) {
      this.loadPromise = this.loadFromAdapter();
    }
    await this.loadPromise;
  }

  /** 从 adapter 读取并兼容 version 2 与 3；不认识的版本整体按空状态处理 */
  private async loadFromAdapter(): Promise<void> {
    try {
      if (!(await this.vault.adapter.exists(BASELINE_FILENAME))) {
        this.data = emptyState();
        return;
      }
      const parsed = JSON.parse(await this.vault.adapter.read(BASELINE_FILENAME));
      if (parsed?.version === 3 && typeof parsed.files === "object") {
        const sanitizedCollab = sanitizeCollaborationFiles(parsed.collaborationFiles);
        this.data = {
          ...emptyState(),
          ...parsed,
          files: parsed.files ?? {},
          pending: Array.isArray(parsed.pending) ? parsed.pending : [],
          conflicts: Array.isArray(parsed.conflicts) ? parsed.conflicts : [],
          collaborationAccount:
            typeof parsed.collaborationAccount === "string" ? parsed.collaborationAccount : "",
          collaborationFiles: sanitizedCollab,
        };
      } else if (parsed?.version === 2 && typeof parsed.files === "object") {
        this.data = {
          ...emptyState(),
          vaultId: typeof parsed.vaultId === "string" ? parsed.vaultId : "",
          cursor: typeof parsed.cursor === "number" ? parsed.cursor : 0,
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

  /** 序列化当前状态并串行写入，避免并发保存互相覆盖 */
  async save(): Promise<void> {
    await this.load();
    const raw = JSON.stringify(this.data);
    const pending = this.saveChain.then(() =>
      this.vault.adapter.write(BASELINE_FILENAME, raw)
    );
    this.saveChain = pending.catch(() => undefined);
    await pending;
  }

  /** 切换绑定仓库；协作基线按账户隔离，跨仓库保留 */
  bindVault(vaultID: string): boolean {
    if (this.data.vaultId === vaultID) return false;
    const { collaborationAccount: account, collaborationFiles: files } = this.data;
    this.data = emptyState();
    this.data.vaultId = vaultID;
    this.data.collaborationAccount = account;
    this.data.collaborationFiles = files;
    return true;
  }

  /** 切换协作账户并清空该账户下的协作基线 */
  bindCollaborationAccount(account: string): boolean {
    if (this.data.collaborationAccount === account) return false;
    this.data.collaborationAccount = account;
    this.data.collaborationFiles = {};
    return true;
  }

  getVaultID(): string {
    return this.data.vaultId;
  }

  getCursor(): number {
    return this.data.cursor;
  }

  /** 推进游标；只增不减，避免乱序响应把进度回退 */
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

  conflicts(): ConflictEntry[] {
    return [...this.data.conflicts];
  }

  /** 写入待传输操作；同路径只保留最新，重命名链折叠为一次操作 */
  putPending(operation: PendingOperation): void {
    const path = normalizePath(operation.path);
    const oldPath = operation.oldPath ? normalizePath(operation.oldPath) : undefined;
    const protectedDelete = this.data.pending.find((item) => item.path === path && item.kind === "delete" && item.source === "scan");
    if (operation.kind === "delete" && protectedDelete) {
      operation = { ...operation, source: "scan", waitForPaths: protectedDelete.waitForPaths };
    }
    if (operation.kind === "upsert") {
      // 已有 rename 等待时忽略后续 upsert，rename 会带上最新内容
      const rename = this.data.pending.find((item) => item.kind === "rename" && item.path === path);
      if (rename) return;
    }
    if (operation.kind === "delete") {
      if (operation.source !== "scan") {
        // 明确删除目标的事件同时结束该目标的扫描上传依赖
        for (const item of this.data.pending) {
          if (item.waitForPaths) item.waitForPaths = item.waitForPaths.filter((dependency) => dependency !== path);
        }
      }
      // 删除已排在 rename 之后时改写为删除来源路径，保留净效果
      const rename = this.data.pending.find((item) => item.kind === "rename" && item.path === path);
      if (rename?.oldPath) {
        this.data.pending = this.data.pending.filter((item) => item.id !== rename.id);
        this.data.pending.push({ ...operation, path: rename.oldPath });
        return;
      }
    }
    if (operation.kind === "rename" && oldPath) {
      for (const item of this.data.pending) {
        if (item.waitForPaths) item.waitForPaths = item.waitForPaths.map((dependency) => dependency === oldPath ? path : dependency);
      }
      const duplicate = this.data.pending.find((item) => item.kind === "rename" && item.path === path);
      if (duplicate) return;
      // 连续 rename 折叠为一次，来源取最早的一条
      const previousRename = this.data.pending.find(
        (item) => item.kind === "rename" && item.path === oldPath
      );
      if (previousRename?.oldPath) {
        this.data.pending = this.data.pending.filter((item) => item.id !== previousRename.id);
        operation = { ...operation, oldPath: previousRename.oldPath };
      }
      if (operation.oldPath === path) operation = { ...operation, kind: "upsert", oldPath: undefined };
    }
    this.data.pending = this.data.pending.filter((item) => {
      // rename 会同时占用来源与目标路径，需清掉两条路径上的旧操作
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

  /** 记录冲突；同路径只保留最新一条 */
  putConflict(conflict: ConflictEntry): void {
    this.data.conflicts = this.data.conflicts.filter((item) => item.path !== conflict.path);
    this.data.conflicts.push({ ...conflict, path: normalizePath(conflict.path) });
  }
  removeConflict(path: string): void {
    this.data.conflicts = this.data.conflicts.filter(
      (item) => item.path !== normalizePath(path)
    );
  }

  getConflict(path: string): ConflictEntry | null {
    return this.data.conflicts.find((item) => item.path === normalizePath(path)) ?? null;
  }

  /** 读取协作基线副本，防止调用方直接改动内部状态 */
  getCollaboration(vaultId: string, fileId: number): CollaborationBaselineEntry | null {
    const entry = this.data.collaborationFiles[collaborationKey(vaultId, fileId)];
    return entry ? cloneCollaborationEntry(entry) : null;
  }
  /** 写入协作基线；存入前克隆以隔离调用方引用 */
  setCollaboration(vaultId: string, fileId: number, entry: CollaborationBaselineEntry): void {
    this.data.collaborationFiles[collaborationKey(vaultId, fileId)] = cloneCollaborationEntry(entry);
  }
  /** 删除协作基线；不存在时返回 false */
  removeCollaboration(vaultId: string, fileId: number): boolean {
    const key = collaborationKey(vaultId, fileId);
    if (!(key in this.data.collaborationFiles)) return false;
    delete this.data.collaborationFiles[key];
    return true;
  }
  /** 按本地路径删除协作基线，返回删除条数；同一路径可能命中多个仓库 */
  removeCollaborationByLocalPath(localPath: string): number {
    const normalized = normalizePath(localPath);
    let removed = 0;
    for (const [key, entry] of Object.entries(this.data.collaborationFiles)) {
      if (normalizePath(entry.localPath) === normalized) {
        delete this.data.collaborationFiles[key];
        removed++;
      }
    }
    return removed;
  }
  /** 返回全部协作基线副本 */
  collaborationEntries(): CollaborationBaselineEntry[] {
    return Object.values(this.data.collaborationFiles).map(cloneCollaborationEntry);
  }
}

/** 统一路径分隔符为 / 并去掉前导 ./，作为所有基线与查询的键 */
export function normalizePath(path: string): string {
  return path.replace(/\\/g, "/").replace(/^\.\/+/, "");
}

/** 清洗持久化的协作基线，键一律按内容重建以防篡改 */
function sanitizeCollaborationFiles(raw: unknown): Record<string, CollaborationBaselineEntry> {
  if (!raw || typeof raw !== "object") return {};
  const out: Record<string, CollaborationBaselineEntry> = {};
  for (const value of Object.values(raw as Record<string, unknown>)) {
    const sanitized = sanitizeCollaborationEntry(value);
    if (sanitized) {
      // 确保 key 与内容一致，防止持久化篡改
      const expected = collaborationKey(sanitized.vaultId, sanitized.fileId);
      out[expected] = sanitized;
    }
  }
  return out;
}
