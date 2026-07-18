import debounce from "lodash.debounce";
import { App, Notice, TFile, Vault } from "obsidian";
import type OSSPlugin from "./main";
import {
  BaselineEntry,
  BaselineStore,
  ConflictEntry,
  normalizePath,
  PendingOperation,
} from "./baseline";
import { OSSApiClient, OSSApiError, SyncFileMeta } from "./api";
import { TaskPool } from "./task-pool";
import { shouldSync } from "./blacklist";
import { SyncRunCoordinator } from "./sync-run-coordinator";
import type { ConflictResolution } from "./conflict-modal";

export type SyncState = "idle" | "syncing" | "error";

interface LocalMeta {
  path: string;
  hash: string;
  size: number;
  mtime: number;
}

type SyncAction =
  | {
      kind: "upload";
      path: string;
      local: LocalMeta;
      baseRevision: number;
      operationID: string;
      operation?: PendingOperation;
    }
  | {
      kind: "delete_remote";
      path: string;
      baseRevision: number;
      operationID: string;
      operation?: PendingOperation;
    }
  | { kind: "download"; path: string; remote: SyncFileMeta }
  | { kind: "delete_local"; path: string; remote: SyncFileMeta }
  | { kind: "delete_local_absent"; path: string }
  | { kind: "adopt"; path: string; local: LocalMeta | null; remote: SyncFileMeta }
  | { kind: "conflict"; path: string; local: LocalMeta | null; remote: SyncFileMeta };

export class SyncEngine {
  private readonly api: OSSApiClient;
  private readonly baseline: BaselineStore;
  private readonly vault: Vault;
  private readonly runCoordinator = new SyncRunCoordinator();
  private readonly suppressed = new Set<string>();
  private debounceFn: (() => void) & { cancel: () => void };
  private enqueueChain: Promise<void> = Promise.resolve();
  private pollTimer: number | null = null;
  private stopped = false;

  constructor(
    app: App,
    api: OSSApiClient,
    baseline: BaselineStore,
    private plugin: OSSPlugin
  ) {
    this.api = api;
    this.baseline = baseline;
    this.vault = app.vault;
    this.debounceFn = this.createDebounce();
  }

  start(): void {
    this.stopped = false;
    this.resetPolling();
  }

  stop(): void {
    this.stopped = true;
    this.debounceFn.cancel();
    if (this.pollTimer !== null) {
      window.clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  resetDebounce(): void {
    this.debounceFn.cancel();
    this.debounceFn = this.createDebounce();
  }

  resetPolling(): void {
    if (this.pollTimer !== null) window.clearInterval(this.pollTimer);
    const seconds = Math.max(10, this.plugin.settings.remotePollIntervalSec);
    this.pollTimer = window.setInterval(() => {
      if (!this.stopped && this.plugin.settings.vaultId && this.api.hasToken()) {
        void this.runOnce({ forceFull: false });
      }
    }, seconds * 1000);
  }

  isSuppressed(path: string): boolean {
    return this.suppressed.has(normalizePath(path));
  }

  enqueueUpsert(path: string): void {
    this.enqueue({
      id: operationID(),
      kind: "upsert",
      path,
      createdAt: Date.now(),
    });
  }

  enqueueDelete(path: string): void {
    this.enqueue({
      id: operationID(),
      kind: "delete",
      path,
      createdAt: Date.now(),
    });
  }

  enqueueDeleteTree(folderPath: string): void {
    const root = normalizePath(folderPath).replace(/\/+$/, "");
    if (!root || !shouldSync(root, this.plugin.settings.syncPoisonObsidianFiles)) return;
    const prefix = `${root}/`;
    this.enqueueChain = this.enqueueChain
      .then(async () => {
        await this.baseline.load();
        const paths = new Set<string>();
        for (const path of this.baseline.paths()) {
          if (path.startsWith(prefix)) paths.add(path);
        }
        for (const operation of this.baseline.pending()) {
          if (operation.path.startsWith(prefix)) paths.add(operation.path);
          if (operation.oldPath?.startsWith(prefix)) paths.add(operation.oldPath);
        }
        for (const path of paths) {
          if (!shouldSync(path, this.plugin.settings.syncPoisonObsidianFiles)) continue;
          this.baseline.putPending({
            id: operationID(),
            kind: "delete",
            path,
            createdAt: Date.now(),
          });
        }
        await this.baseline.save();
        if (paths.size > 0) this.debounceFn();
      })
      .catch((error: unknown) => {
        new Notice("OSS: 无法保存文件夹删除队列 " + errorMessage(error));
      });
  }

  enqueueRename(oldPath: string, newPath: string): void {
    this.enqueue({
      id: operationID(),
      kind: "rename",
      path: newPath,
      oldPath,
      createdAt: Date.now(),
    });
  }

  private enqueue(operation: PendingOperation): void {
    if (!shouldSync(operation.path, this.plugin.settings.syncPoisonObsidianFiles)) return;
    if (operation.oldPath && !shouldSync(operation.oldPath, this.plugin.settings.syncPoisonObsidianFiles)) return;
    if (this.isSuppressed(operation.path) || (operation.oldPath && this.isSuppressed(operation.oldPath))) return;
    this.enqueueChain = this.enqueueChain.then(async () => {
      await this.baseline.load();
      this.baseline.putPending({
        ...operation,
        path: normalizePath(operation.path),
        oldPath: operation.oldPath ? normalizePath(operation.oldPath) : undefined,
      });
      await this.baseline.save();
      this.debounceFn();
    }).catch((error: unknown) => {
      new Notice("OSS: 无法保存同步队列 " + errorMessage(error));
    });
  }

  async runOnce(opts: { forceFull: boolean }): Promise<void> {
    await this.runCoordinator.run(opts.forceFull, async (forceFull) => {
      await this.executeRun(forceFull);
    });
  }

  private async executeRun(forceFull: boolean): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    if (!this.api.hasToken() || !vaultID) {
      this.plugin.setSyncState("error", !this.api.hasToken() ? "not logged in" : "vault not bound");
      return;
    }
    this.plugin.setSyncState("syncing");
    await this.enqueueChain;
    forceFull = forceFull || !this.plugin.settings.incrementalCheck;

    try {
      await this.baseline.load();
      if (this.baseline.bindVault(vaultID)) {
        forceFull = true;
        await this.baseline.save();
      }

      const queuedBeforeFetch = this.baseline.pending();
      if (this.needsFullSnapshotForDeletes(queuedBeforeFetch)) {
        forceFull = true;
      }
      const remote = await this.fetchRemote(vaultID, forceFull);
      const recoveryFull = forceFull || remote.recoverySnapshot;
      let pending = this.baseline.pending();
      if (remote.recoverySnapshot) {
        await this.prepareRecoveryRenames(pending, remote.files);
        pending = this.baseline.pending();
      }
      await this.processRenames(vaultID, pending, remote.files);

      const remainingPending = this.baseline.pending();
      const actions = await this.planActions(
        recoveryFull,
        remote.files,
        remainingPending,
        remote.recoverySnapshot
      );

      const pool = new TaskPool({
        maxConcurrency: this.plugin.settings.maxConcurrency,
        maxRetries: 2,
        baseDelayMs: 500,
      });
      const results = await pool.run(actions, async (action) => {
        try {
          await this.applyAction(vaultID, action);
        } catch (error: unknown) {
          if (error instanceof OSSApiError && error.status === 409 && error.current) {
            const local = await this.localMeta(action.path);
            await this.recordConflict(action.path, local, error.current);
            return;
          }
          throw error;
        }
      });

      const failures = results.filter((result) => !result.ok);
      if (failures.length === 0) {
        this.baseline.setCursor(remote.nextCursor);
        await this.baseline.save();
        await this.api.acknowledge(vaultID, remote.nextCursor);
      } else {
        await this.baseline.save();
      }

      if (this.api.isClockDriftLarge()) {
        new Notice(
          `OSS: 本地时钟与服务端偏差 ${Math.round(this.api.getTimeOffset() / 1000)}s。`,
          8000
        );
      }
      if (failures.length > 0) {
        this.plugin.setSyncState("error", `${failures.length} failed`);
        new Notice(`OSS: ${failures.length} 个同步任务失败，将在下次重试`, 6000);
      } else {
        this.plugin.setSyncState("idle");
      }
    } catch (error: unknown) {
      this.plugin.setSyncState("error", errorMessage(error));
      new Notice("OSS sync error: " + errorMessage(error), 8000);
    }
  }

  private async fetchRemote(
    vaultID: string,
    forceFull: boolean
  ): Promise<{
    files: Map<string, SyncFileMeta>;
    nextCursor: number;
    recoverySnapshot: boolean;
  }> {
    const files = new Map<string, SyncFileMeta>();
    let useManifest = forceFull;
    let cursor = useManifest ? 0 : this.baseline.getCursor();
    let recoverySnapshot = false;
    while (true) {
      let page;
      try {
        page = useManifest
          ? await this.api.manifest(vaultID, cursor)
          : await this.api.changes(vaultID, cursor);
      } catch (error: unknown) {
        if (
          !useManifest &&
          error instanceof OSSApiError &&
          error.status === 410 &&
          error.code === "history_compacted"
        ) {
          files.clear();
          cursor = 0;
          useManifest = true;
          recoverySnapshot = true;
          continue;
        }
        throw error;
      }
      for (const file of page.files) files.set(normalizePath(file.path), file);
      recoverySnapshot = recoverySnapshot || page.recovery_snapshot;
      cursor = page.next_cursor;
      if (!page.has_more) return { files, nextCursor: cursor, recoverySnapshot };
    }
  }

  private async prepareRecoveryRenames(
    operations: PendingOperation[],
    remote: Map<string, SyncFileMeta>
  ): Promise<void> {
    for (const operation of operations) {
      if (operation.kind !== "rename" || !operation.oldPath) continue;
      if (!this.baseline.get(operation.oldPath) || remote.has(operation.oldPath)) continue;
      const local = await this.localMeta(operation.path);
      this.baseline.remove(operation.oldPath);
      this.baseline.removePending(operation.id);
      if (local) {
        this.baseline.putPending({
          ...operation,
          kind: "upsert",
          oldPath: undefined,
        });
      }
    }
    await this.baseline.save();
  }

  private async processRenames(
    vaultID: string,
    operations: PendingOperation[],
    remote: Map<string, SyncFileMeta>
  ): Promise<void> {
    for (const operation of operations) {
      if (operation.kind !== "rename" || !operation.oldPath) continue;
      if (
        this.baseline.getConflict(operation.oldPath) ||
        this.baseline.getConflict(operation.path)
      ) {
        continue;
      }
      const oldBaseline = this.baseline.get(operation.oldPath);
      const newBaseline = this.baseline.get(operation.path);
      const local = await this.localMeta(operation.path);
      if (!oldBaseline || !local) {
        this.baseline.removePending(operation.id);
        if (local) {
          this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
        }
        continue;
      }
      const oldRemote = remote.get(operation.oldPath);
      const newRemote = remote.get(operation.path);
      if (oldRemote && oldRemote.revision !== oldBaseline.serverRevision) {
        await this.preserveBothAfterRenameSourceConflict(
          vaultID,
          operation,
          oldRemote,
          remote
        );
        continue;
      }
      if (newRemote && newRemote.revision !== (newBaseline?.serverRevision ?? 0)) {
        await this.recordConflict(operation.path, local, newRemote);
        continue;
      }
      try {
        const result = await this.api.renameV2(vaultID, {
          oldPath: operation.oldPath,
          newPath: operation.path,
          baseRevision: oldBaseline.serverRevision,
          targetRevision: newBaseline?.serverRevision ?? 0,
          operationID: operation.id,
          mtime: local.mtime,
        });
        remote.set(operation.oldPath, result.old);
        remote.set(operation.path, result.new);
        this.baseline.set(operation.oldPath, baselineFromRemote(result.old, null));
        this.baseline.set(operation.path, baselineFromRemote(result.new, local));
        this.baseline.removePending(operation.id);
      } catch (error: unknown) {
        if (error instanceof OSSApiError && error.status === 409 && error.current) {
          const conflictPath = normalizePath(error.current.path || operation.path);
          if (conflictPath === operation.oldPath) {
            await this.preserveBothAfterRenameSourceConflict(
              vaultID,
              operation,
              error.current,
              remote
            );
            continue;
          }
          await this.recordConflict(
            conflictPath,
            local,
            error.current
          );
          continue;
        }
        throw error;
      }
    }
    await this.baseline.save();
  }

  private async preserveBothAfterRenameSourceConflict(
    vaultID: string,
    operation: PendingOperation,
    currentSource: SyncFileMeta,
    remote: Map<string, SyncFileMeta>
  ): Promise<void> {
    remote.set(currentSource.path, currentSource);
    if (currentSource.deleted) {
      await this.applyRemoteDelete(currentSource);
    } else {
      await this.applyDownload(vaultID, currentSource);
    }
    this.baseline.removePending(operation.id);
    this.baseline.putPending({
      ...operation,
      kind: "upsert",
      oldPath: undefined,
    });
    new Notice(
      `OSS: ${operation.oldPath} 在其他设备已更新，已保留远端原路径和本地新路径`,
      8000
    );
  }

  private async planActions(
    forceFull: boolean,
    remote: Map<string, SyncFileMeta>,
    pending: PendingOperation[],
    recoverySnapshot = false
  ): Promise<SyncAction[]> {
    const pendingByPath = new Map<string, PendingOperation>();
    const renamePaths = new Set<string>();
    for (const operation of pending) {
      if (operation.kind === "rename") {
        renamePaths.add(operation.path);
        if (operation.oldPath) renamePaths.add(operation.oldPath);
      } else {
        pendingByPath.set(operation.path, operation);
      }
    }

    const paths = new Set<string>();
    for (const path of remote.keys()) paths.add(path);
    for (const path of pendingByPath.keys()) paths.add(path);
    if (forceFull) {
      for (const path of this.baseline.paths()) paths.add(path);
      for (const file of this.vault.getFiles()) {
        if (shouldSync(file.path, this.plugin.settings.syncPoisonObsidianFiles)) {
          paths.add(normalizePath(file.path));
        }
      }
    }

    const actions: SyncAction[] = [];
    for (const path of paths) {
      if (renamePaths.has(path) || this.baseline.getConflict(path)) continue;
      const baseline = this.baseline.get(path);
      const local = await this.localMeta(path, baseline);
      const submittedRemote = remote.get(path);
      const server = submittedRemote ?? (!forceFull && baseline ? remoteFromBaseline(path, baseline) : undefined);
      const operation = pendingByPath.get(path);

      if (!baseline) {
        if (operation?.kind === "delete") {
          if (server && !server.deleted) {
            actions.push({
              kind: "delete_remote",
              path,
              baseRevision: server.revision,
              operationID: operation.id,
              operation,
            });
          } else if (server?.deleted) {
            actions.push({ kind: "adopt", path, local: null, remote: server });
          } else {
            this.baseline.removePending(operation.id);
          }
        } else if (local && !server) {
          actions.push({
            kind: "upload",
            path,
            local,
            baseRevision: 0,
            operationID: operation?.id ?? operationID(),
            operation,
          });
        } else if (!local && server && !server.deleted) {
          actions.push({ kind: "download", path, remote: server });
        } else if (local && server && !server.deleted && local.hash === server.hash) {
          actions.push({ kind: "adopt", path, local, remote: server });
        } else if (local && server) {
          actions.push({ kind: "conflict", path, local, remote: server });
        } else if (!local && server?.deleted) {
          actions.push({ kind: "adopt", path, local: null, remote: server });
        } else if (operation) {
          this.baseline.removePending(operation.id);
        }
        continue;
      }

      if (recoverySnapshot && !submittedRemote) {
        if (baseline.serverDeleted) {
          this.baseline.remove(path);
          if (local) {
            actions.push({
              kind: "upload",
              path,
              local,
              baseRevision: 0,
              operationID: operation?.id ?? operationID(),
              operation,
            });
          } else if (operation) {
            this.baseline.removePending(operation.id);
          }
        } else if (!local) {
          this.baseline.remove(path);
          if (operation) this.baseline.removePending(operation.id);
        } else if (local.hash === baseline.serverHash) {
          actions.push({ kind: "delete_local_absent", path });
        } else {
          actions.push({
            kind: "conflict",
            path,
            local,
            remote: compactedDeleteMeta(path),
          });
        }
        continue;
      }

      const localChanged = baseline.serverDeleted
        ? local !== null
        : local === null || local.hash !== baseline.serverHash;
      const remoteChanged = server
        ? server.revision !== baseline.serverRevision ||
          server.hash !== baseline.serverHash ||
          server.deleted !== baseline.serverDeleted
        : forceFull;

      if (!localChanged && !remoteChanged) {
        if (operation) this.baseline.removePending(operation.id);
        continue;
      }
      if (localChanged && !remoteChanged) {
        if (local) {
          actions.push({
            kind: "upload",
            path,
            local,
            baseRevision: baseline.serverRevision,
            operationID: operation?.id ?? operationID(),
            operation,
          });
        } else {
          actions.push({
            kind: "delete_remote",
            path,
            baseRevision: baseline.serverRevision,
            operationID: operation?.id ?? operationID(),
            operation,
          });
        }
        continue;
      }
      if (!localChanged && remoteChanged && server) {
        actions.push(
          server.deleted
            ? { kind: "delete_local", path, remote: server }
            : { kind: "download", path, remote: server }
        );
        continue;
      }
      if (local && server && !server.deleted && local.hash === server.hash) {
        actions.push({ kind: "adopt", path, local, remote: server });
      } else if (server) {
        actions.push({ kind: "conflict", path, local, remote: server });
      }
    }
    return actions;
  }

  private needsFullSnapshotForDeletes(pending: PendingOperation[]): boolean {
    return pending.some(
      (operation) =>
        operation.kind === "delete" && this.baseline.get(operation.path) === null
    );
  }

  private async applyAction(vaultID: string, action: SyncAction): Promise<void> {
    switch (action.kind) {
      case "upload": {
        const file = this.vault.getAbstractFileByPath(action.path);
        if (!(file instanceof TFile)) throw new Error("local file vanished: " + action.path);
        const content = await this.vault.readBinary(file);
        const result = await this.api.uploadV2(vaultID, {
          path: action.path,
          baseRevision: action.baseRevision,
          hash: action.local.hash,
          mtime: action.local.mtime,
          operationID: action.operationID,
          content,
        });
        this.baseline.set(action.path, baselineFromRemote(result, action.local));
        if (action.operation) this.baseline.removePending(action.operation.id);
        return;
      }
      case "delete_remote": {
        const result = await this.api.deleteV2(vaultID, {
          path: action.path,
          baseRevision: action.baseRevision,
          operationID: action.operationID,
          mtime: Date.now(),
        });
        this.baseline.set(action.path, baselineFromRemote(result, null));
        if (action.operation) this.baseline.removePending(action.operation.id);
        return;
      }
      case "download":
        await this.applyDownload(vaultID, action.remote);
        return;
      case "delete_local":
        await this.applyRemoteDelete(action.remote);
        return;
      case "delete_local_absent": {
        const existing = this.vault.getAbstractFileByPath(action.path);
        if (existing) {
          this.suppress(action.path);
          await this.vault.delete(existing, true);
        }
        this.baseline.remove(action.path);
        this.baseline.removePendingForPath(action.path);
        return;
      }
      case "adopt":
        this.baseline.set(action.path, baselineFromRemote(action.remote, action.local));
        this.baseline.removePendingForPath(action.path);
        return;
      case "conflict":
        await this.recordConflict(action.path, action.local, action.remote);
    }
  }

  private async applyDownload(vaultID: string, remote: SyncFileMeta): Promise<void> {
    const result = await this.api.downloadV2(vaultID, remote.path, remote.revision);
    await this.ensureParentFolders(remote.path);
    this.suppress(remote.path);
    const existing = this.vault.getAbstractFileByPath(remote.path);
    if (existing instanceof TFile) {
      await this.vault.modifyBinary(existing, result.content, { mtime: result.meta.mtime });
    } else {
      await this.vault.createBinary(remote.path, result.content, { mtime: result.meta.mtime });
    }
    this.baseline.set(remote.path, baselineFromRemote(result.meta, {
      path: remote.path,
      hash: result.meta.hash,
      size: result.meta.size,
      mtime: result.meta.mtime,
    }));
    this.baseline.removePendingForPath(remote.path);
  }

  private async applyRemoteDelete(remote: SyncFileMeta): Promise<void> {
    const existing = this.vault.getAbstractFileByPath(remote.path);
    if (existing) {
      this.suppress(remote.path);
      await this.vault.delete(existing, true);
    }
    this.baseline.set(remote.path, baselineFromRemote(remote, null));
    this.baseline.removePendingForPath(remote.path);
  }

  private async recordConflict(
    path: string,
    local: LocalMeta | null,
    remote: SyncFileMeta
  ): Promise<void> {
    const conflict: ConflictEntry = {
      path,
      localHash: local?.hash ?? "",
      remoteRevision: remote.revision,
      remoteHash: remote.hash,
      remoteDeleted: remote.deleted,
      remoteMTime: remote.mtime,
      remoteSize: remote.size,
      remoteType: remote.type,
      detectedAt: Date.now(),
    };
    const existed = this.baseline.getConflict(path) !== null;
    this.baseline.putConflict(conflict);
    await this.baseline.save();
    if (!existed && !remote.deleted && remote.type === "markdown" && local) {
      this.plugin.openConflictModal(path);
    } else if (!existed) {
      new Notice(`OSS: ${path} 存在同步冲突，已暂停该文件`, 8000);
    }
  }

  async resolveConflict(path: string, resolution: ConflictResolution): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    const conflict = this.baseline.getConflict(path);
    if (!vaultID || !conflict) throw new Error("conflict not found");
    const file = this.vault.getAbstractFileByPath(path);

    if (resolution === "accept_remote") {
      if (conflict.remoteDeleted) {
        await this.applyRemoteDelete(conflictToMeta(conflict));
      } else {
        await this.applyDownload(vaultID, conflictToMeta(conflict));
      }
    } else if (resolution === "force_local") {
      if (file instanceof TFile) {
        const local = await this.localMeta(path);
        if (!local) throw new Error("local file vanished");
        const result = await this.api.uploadV2(vaultID, {
          path,
          baseRevision: conflict.remoteRevision,
          hash: local.hash,
          mtime: local.mtime,
          operationID: operationID(),
          content: await this.vault.readBinary(file),
        });
        this.baseline.set(path, baselineFromRemote(result, local));
      } else {
        const result = await this.api.deleteV2(vaultID, {
          path,
          baseRevision: conflict.remoteRevision,
          operationID: operationID(),
          mtime: Date.now(),
        });
        this.baseline.set(path, baselineFromRemote(result, null));
      }
    } else {
      if (!(file instanceof TFile)) throw new Error("local file vanished");
      const copyPath = conflictCopyPath(path);
      await this.ensureParentFolders(copyPath);
      const copy = await this.vault.createBinary(copyPath, await this.vault.readBinary(file));
      if (conflict.remoteDeleted) {
        await this.applyRemoteDelete(conflictToMeta(conflict));
      } else {
        await this.applyDownload(vaultID, conflictToMeta(conflict));
      }
      this.enqueueUpsert(copy.path);
    }
    this.baseline.removeConflict(path);
    this.baseline.removePendingForPath(path);
    await this.baseline.save();
  }

  getConflict(path: string): ConflictEntry | null {
    return this.baseline.getConflict(path);
  }

  dismissConflict(path: string): void {
    this.baseline.removeConflict(path);
    void this.baseline.save();
  }

  private async localMeta(path: string, baseline?: BaselineEntry | null): Promise<LocalMeta | null> {
    const file = this.vault.getAbstractFileByPath(path);
    if (!(file instanceof TFile)) return null;
    if (
      baseline &&
      baseline.localHash &&
      baseline.localMTime === file.stat.mtime &&
      baseline.localSize === file.stat.size
    ) {
      return {
        path,
        hash: baseline.localHash,
        size: file.stat.size,
        mtime: file.stat.mtime,
      };
    }
    const content = await this.vault.readBinary(file);
    return {
      path,
      hash: await sha256Hex(content),
      size: file.stat.size,
      mtime: file.stat.mtime,
    };
  }

  private async ensureParentFolders(path: string): Promise<void> {
    const parts = normalizePath(path).split("/");
    parts.pop();
    let current = "";
    for (const part of parts) {
      current = current ? `${current}/${part}` : part;
      if (!this.vault.getAbstractFileByPath(current)) {
        try {
          await this.vault.createFolder(current);
        } catch {
          if (!this.vault.getAbstractFileByPath(current)) throw new Error("cannot create folder: " + current);
        }
      }
    }
  }

  private suppress(path: string): void {
    const normalized = normalizePath(path);
    this.suppressed.add(normalized);
    window.setTimeout(() => this.suppressed.delete(normalized), 1500);
  }

  private createDebounce(): (() => void) & { cancel: () => void } {
    return debounce(
      () => void this.runOnce({ forceFull: false }),
      Math.max(5, this.plugin.settings.syncIntervalSec) * 1000
    );
  }
}

function baselineFromRemote(remote: SyncFileMeta, local: LocalMeta | null): BaselineEntry {
  return {
    serverRevision: remote.revision,
    serverHash: remote.hash,
    serverDeleted: remote.deleted,
    localHash: local?.hash ?? "",
    localMTime: local?.mtime ?? 0,
    localSize: local?.size ?? 0,
  };
}

function remoteFromBaseline(path: string, baseline: BaselineEntry): SyncFileMeta {
  return {
    path,
    type: classifyPath(path),
    hash: baseline.serverHash,
    size: baseline.localSize,
    mtime: baseline.localMTime,
    revision: baseline.serverRevision,
    deleted: baseline.serverDeleted,
  };
}

function conflictToMeta(conflict: ConflictEntry): SyncFileMeta {
  return {
    path: conflict.path,
    type: conflict.remoteType,
    hash: conflict.remoteHash,
    size: conflict.remoteSize,
    mtime: conflict.remoteMTime,
    revision: conflict.remoteRevision,
    deleted: conflict.remoteDeleted,
  };
}

function compactedDeleteMeta(path: string): SyncFileMeta {
  return {
    path,
    type: classifyPath(path),
    hash: "",
    size: 0,
    mtime: 0,
    revision: 0,
    deleted: true,
  };
}

function classifyPath(path: string): "markdown" | "attachment" | "config" {
  const lower = path.toLowerCase();
  if (lower.endsWith(".md")) return "markdown";
  if (lower.startsWith(".obsidian/")) return "config";
  return "attachment";
}

function operationID(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function conflictCopyPath(path: string): string {
  const slash = path.lastIndexOf("/");
  const directory = slash >= 0 ? path.slice(0, slash + 1) : "";
  const filename = slash >= 0 ? path.slice(slash + 1) : path;
  const dot = filename.lastIndexOf(".");
  const base = dot > 0 ? filename.slice(0, dot) : filename;
  const extension = dot > 0 ? filename.slice(dot) : "";
  const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
  return `${directory}${base}_conflict_${timestamp}${extension}`;
}

async function sha256Hex(buffer: ArrayBuffer): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", buffer);
  return Array.from(new Uint8Array(digest))
    .map((value) => value.toString(16).padStart(2, "0"))
    .join("");
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "unknown error";
}
