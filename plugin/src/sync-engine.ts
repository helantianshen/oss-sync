import debounce from "lodash.debounce";
import { Notice, TFile } from "obsidian";
import type { App, Vault } from "obsidian";
import { OSSApiClient, OSSApiError, type SyncFileMeta } from "./api.js";
import {
  BaselineStore,
  normalizePath,
  type BaselineEntry,
  type ConflictEntry,
  type PendingOperation,
} from "./baseline.js";
import { shouldSync } from "./blacklist.js";
import type { ConflictResolution } from "./conflict-modal.js";
import type { Diagnostics } from "./diagnostics.js";
import { localizeError } from "./localized-error.js";
import type OSSPlugin from "./main.js";
import { NoticeGuard } from "./notice-guard.js";
import { baselineFromAcknowledgement } from "./ordinary-sync-baseline.js";
import { isOrdinarySyncConflict409 } from "./ordinary-sync-conflict-409.js";
import { OrdinarySyncConflictResolver } from "./ordinary-sync-conflict-resolver.js";
import {
  OrdinarySyncFileAccess,
  type LocalReadResult,
} from "./ordinary-sync-file-access.js";
import {
  ordinarySyncCandidatePaths,
  planOrdinarySyncActions,
} from "./ordinary-sync-action-planner.js";
import {
  assertNeverOrdinarySyncAction,
  type ExpectedLocalState,
  type OrdinarySyncAction,
  type OrdinarySyncActionOutcome,
  type OrdinarySyncLocalMeta,
  type OrdinarySyncPlanResult,
} from "./ordinary-sync-actions.js";
import { SyncRunCoordinator } from "./sync-run-coordinator.js";
import { SyncStrategyManager } from "./strategy.js";
import { TaskPool, type TaskResult } from "./task-pool.js";
import { buildTransferNoticePlan, type TransferNotice } from "./sync-notices.js";

export type SyncState = "idle" | "syncing" | "error";

type RemoteSnapshot = {
  readonly files: Map<string, SyncFileMeta>;
  readonly nextCursor: number;
  readonly recoverySnapshot: boolean;
};

type LiveAcknowledgement = {
  readonly path: string;
  readonly server: SyncFileMeta;
  readonly local: LocalReadResult | null;
  readonly bytes: Uint8Array;
  readonly bytesHash: string;
};

/** 同一 Vault 的普通同步由持久化基线、操作队列和修订游标共同驱动 */
export class SyncEngine {
  private readonly api: OSSApiClient;
  private readonly baseline: BaselineStore;
  private readonly vault: Vault;
  private readonly strategy: SyncStrategyManager;
  private readonly runCoordinator = new SyncRunCoordinator();
  private readonly ordinaryFileAccess: OrdinarySyncFileAccess;
  private readonly suppressed = new Set<string>();
  private enqueueChain: Promise<void> = Promise.resolve();
  private debounceFn: (() => void) & { cancel: () => void };
  private pollTimer: number | null = null;
  private stopped = false;
  private effectiveMode: "short_poll" | "long_poll" = "short_poll";
  private longPollGen = 0;
  private longPollController: AbortController | null = null;
  /** 同步失败提示的冷却抑制，成功一次后清零，使下一次失败能立即反馈 */
  private readonly errorNotices = new NoticeGuard();

  constructor(
    app: App,
    api: OSSApiClient,
    baseline: BaselineStore,
    private readonly plugin: OSSPlugin,
    private readonly diagnostics?: Diagnostics,
  ) {
    this.api = api;
    this.baseline = baseline;
    this.vault = app.vault;
    this.strategy = new SyncStrategyManager(api);
    this.ordinaryFileAccess = new OrdinarySyncFileAccess(this.vault, (path) => this.suppress(path));
    this.debounceFn = this.createDebounce();
  }

  start(): void {
    this.stopped = false;
    this.resetPolling();
  }

  stop(): void {
    this.stopped = true;
    this.debounceFn.cancel();
    this.stopCurrentPolling();
  }

  resetDebounce(): void {
    this.debounceFn.cancel();
    this.debounceFn = this.createDebounce();
  }

  async flushPendingQueue(): Promise<void> {
    await this.enqueueChain;
  }

  resetPolling(): void {
    this.stopCurrentPolling();
    if (this.stopped || !this.plugin.settings.vaultId || !this.api.hasToken()) return;
    if (this.effectiveMode === "long_poll") return this.startLongPoll();
    const seconds = Math.max(3, this.plugin.settings.remotePollIntervalSec);
    this.pollTimer = window.setInterval(() => {
      if (!this.stopped && this.plugin.settings.vaultId && this.api.hasToken()) {
        void this.runOnce({ forceFull: false });
      }
    }, seconds * 1000);
  }

  async refreshPollingStrategy(): Promise<void> {
    await this.applyStrategy();
  }

  private stopCurrentPolling(): void {
    if (this.pollTimer !== null) {
      window.clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    this.stopLongPoll();
  }

  private startLongPoll(): void {
    const generation = ++this.longPollGen;
    const controller = new AbortController();
    this.longPollController = controller;
    void this.longPollLoop(generation, controller);
  }

  private stopLongPoll(): void {
    this.longPollGen += 1;
    this.longPollController?.abort();
    this.longPollController = null;
  }

  private async longPollLoop(generation: number, controller: AbortController): Promise<void> {
    const wait = this.strategy.getLongPollWaitSec();
    while (!this.stopped && !controller.signal.aborted && generation === this.longPollGen) {
      const vaultID = this.plugin.settings.vaultId;
      if (!vaultID || !this.api.hasToken()) return;
      const startedAt = Date.now();
      try {
        const result = await this.api.changes(vaultID, this.baseline.getCursor(), wait);
        if (this.stopped || controller.signal.aborted || generation !== this.longPollGen) return;
        const changed = result.files.length > 0 || result.next_cursor > this.baseline.getCursor();
        this.diagnostics?.record({
          kind: "poll",
          at: Date.now(),
          scope: "sync",
          changed,
          durationMs: Date.now() - startedAt,
        });
        if (changed) await this.runOnce({ forceFull: false });
      } catch (error: unknown) {
        if (this.stopped || controller.signal.aborted || generation !== this.longPollGen) return;
        this.diagnostics?.record({
          kind: "poll",
          at: Date.now(),
          scope: "sync",
          durationMs: Date.now() - startedAt,
          failed: true,
        });
        // 轮询失败按 key 抑制冷却，长轮询与 runOnce 共享同一 key 的同一次提示
        if (this.errorNotices.shouldShow("sync.run")) {
          new Notice(this.plugin.t("sync.longPollFailed", { error: this.localizedError(error) }));
        }
        await sleep(3000);
      }
    }
  }

  private async applyStrategy(): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    if (!this.api.hasToken() || !vaultID) return;
    try {
      const beforeDebounce = this.strategy.getMinDebounceSec();
      const beforeWait = this.strategy.getLongPollWaitSec();
      await this.strategy.fetch(vaultID, this.plugin.settings.vaultSyncMode ?? "short_poll");
      const nextMode = this.strategy.getEffectiveMode();
      if (this.strategy.getMinDebounceSec() !== beforeDebounce) this.resetDebounce();
      if (nextMode !== this.effectiveMode || (nextMode === "long_poll" && this.strategy.getLongPollWaitSec() !== beforeWait)) {
        this.effectiveMode = nextMode;
        this.resetPolling();
      }
    } catch (error: unknown) {
      if (error instanceof OSSApiError) return;
      throw error;
    }
  }

  isSuppressed(path: string): boolean {
    return this.suppressed.has(normalizePath(path));
  }

  getEffectiveModeLabel(): string {
    return this.plugin.t(this.effectiveMode === "long_poll" ? "common.longPoll" : "common.shortPoll");
  }

  enqueueUpsert(path: string): void {
    this.enqueue({ id: operationID(), kind: "upsert", path, createdAt: Date.now() });
  }

  enqueueDelete(path: string): void {
    this.enqueue({ id: operationID(), kind: "delete", path, createdAt: Date.now() });
  }

  enqueueDeleteTree(folderPath: string): void {
    const root = normalizePath(folderPath).replace(/\/+$/, "");
    if (!root || !shouldSync(root, this.plugin.settings.syncPoisonObsidianFiles)) return;
    const prefix = `${root}/`;
    this.enqueueChain = this.enqueueChain.then(async () => {
      await this.baseline.load();
      const paths = new Set<string>();
      for (const path of this.baseline.paths()) if (path.startsWith(prefix)) paths.add(path);
      for (const operation of this.baseline.pending()) {
        if (operation.path.startsWith(prefix)) paths.add(operation.path);
        if (operation.oldPath?.startsWith(prefix)) paths.add(operation.oldPath);
      }
      for (const path of paths) {
        if (shouldSync(path, this.plugin.settings.syncPoisonObsidianFiles)) {
          this.baseline.putPending({ id: operationID(), kind: "delete", path, createdAt: Date.now() });
        }
      }
      await this.baseline.save();
      if (paths.size > 0) {
        this.diagnostics?.record({
          kind: "sync_queue",
          at: Date.now(),
          phase: "persisted",
          operation: "delete",
          pendingCount: this.baseline.pending().length,
        });
      }
      if (paths.size > 0) this.debounceFn();
    }).catch((error: unknown) => {
      new Notice(this.plugin.t("sync.saveDeleteQueueFailed", { error: errorMessage(error, this.plugin.t("common.unknownError")) }));
    });
  }

  enqueueRename(oldPath: string, newPath: string): void {
    this.enqueue({ id: operationID(), kind: "rename", path: newPath, oldPath, createdAt: Date.now() });
  }

  private enqueue(operation: PendingOperation): void {
    if (!shouldSync(operation.path, this.plugin.settings.syncPoisonObsidianFiles)) return;
    if (operation.oldPath && !shouldSync(operation.oldPath, this.plugin.settings.syncPoisonObsidianFiles)) return;
    if (this.isSuppressed(operation.path) || (operation.oldPath !== undefined && this.isSuppressed(operation.oldPath))) return;
    this.enqueueChain = this.enqueueChain.then(async () => {
      await this.baseline.load();
      this.baseline.putPending({
        ...operation,
        path: normalizePath(operation.path),
        oldPath: operation.oldPath === undefined ? undefined : normalizePath(operation.oldPath),
      });
      await this.baseline.save();
      this.diagnostics?.record({
        kind: "sync_queue",
        at: Date.now(),
        phase: "persisted",
        operation: operation.kind,
        pendingCount: this.baseline.pending().length,
      });
      this.debounceFn();
    }).catch((error: unknown) => {
      new Notice(this.plugin.t("sync.saveQueueFailed", { error: errorMessage(error, this.plugin.t("common.unknownError")) }));
    });
  }

  async runOnce(opts: { readonly forceFull: boolean }): Promise<boolean> {
    let succeeded = true;
    await this.runCoordinator.run(opts.forceFull, async (forceFull) => {
      succeeded = await this.executeRun(forceFull);
    });
    return succeeded;
  }

  private async executeRun(forceFull: boolean): Promise<boolean> {
    const vaultID = this.plugin.settings.vaultId;
    if (!this.api.hasToken() || !vaultID) {
      this.plugin.setSyncState("error", this.plugin.t(!this.api.hasToken() ? "sync.notLoggedIn" : "sync.vaultNotBound"));
      return false;
    }
    this.plugin.setSyncState("syncing");
    await this.enqueueChain;
    try {
      await this.applyStrategy();
      await this.baseline.load();
      const pendingAtStart = this.baseline.pending();
      this.diagnostics?.record({
        kind: "sync_run",
        at: Date.now(),
        phase: "start",
        forceFull,
        cursor: this.baseline.getCursor(),
        pendingCount: pendingAtStart.length,
      });
      if (pendingAtStart.length > 0) {
        this.diagnostics?.record({
          kind: "sync_queue",
          at: Date.now(),
          phase: "resumed",
          operation: pendingAtStart[0].kind,
          pendingCount: pendingAtStart.length,
        });
      }
      let full = forceFull || !this.plugin.settings.incrementalCheck;
      if (this.baseline.bindVault(vaultID)) {
        full = true;
        await this.baseline.save();
      }
      if (this.needsFullSnapshotForDeletes(this.baseline.pending())) full = true;
      const remote = await this.fetchRemote(vaultID, full);
      let pending = this.baseline.pending();
      this.diagnostics?.record({
        kind: "sync_run",
        at: Date.now(),
        phase: "remote",
        forceFull: full,
        cursor: this.baseline.getCursor(),
        nextCursor: remote.nextCursor,
        remoteFiles: remote.files.size,
        pendingCount: pending.length,
      });
      if (remote.recoverySnapshot) {
        await this.prepareRecoveryRenames(pending, remote.files);
        pending = this.baseline.pending();
      }
      await this.processRenames(vaultID, pending, remote.files);
      const plan = await this.planOrdinaryActions(full || remote.recoverySnapshot, remote.files, pending, remote.recoverySnapshot);
      await this.applyPlanEffects(plan);
      const results = await new TaskPool({
        maxConcurrency: this.plugin.settings.maxConcurrency,
        maxRetries: 2,
        baseDelayMs: 500,
      }).run([...plan.actions], (action) => this.applyAction(vaultID, action));
      this.reportTransferNotifications(plan.actions, results);
      if (hasUnresolvedAction(results)) {
        await this.baseline.save();
        this.diagnostics?.record({
          kind: "sync_run",
          at: Date.now(),
          phase: "failed",
          forceFull: full,
          cursor: this.baseline.getCursor(),
          nextCursor: remote.nextCursor,
          remoteFiles: remote.files.size,
          actionCount: plan.actions.length,
          resolvedCount: results.filter((result) => result.ok && result.result?.kind === "resolved").length,
        });
        this.reportActionFailures(results);
        return false;
      }
      await this.api.acknowledge(vaultID, remote.nextCursor);
      this.baseline.setCursor(remote.nextCursor);
      await this.baseline.save();
      this.diagnostics?.record({
        kind: "sync_run",
        at: Date.now(),
        phase: "complete",
        forceFull: full,
        cursor: this.baseline.getCursor(),
        nextCursor: remote.nextCursor,
        remoteFiles: remote.files.size,
        actionCount: plan.actions.length,
        resolvedCount: results.filter((result) => result.ok && result.result?.kind === "resolved").length,
      });
      if (this.api.isClockDriftLarge()) {
        new Notice(this.plugin.t("sync.clockDrift", { seconds: Math.round(this.api.getTimeOffset() / 1000) }), 8000);
      }
      this.errorNotices.reset();
      this.plugin.setSyncState("idle");
      return true;
    } catch (error: unknown) {
      this.diagnostics?.record({
        kind: "sync_run",
        at: Date.now(),
        phase: "failed",
        forceFull,
        cursor: this.baseline.getCursor(),
      });
      const message = this.localizedError(error);
      this.plugin.setSyncState("error", message);
      // 轮询与手动同步共用该路径，冷却期内重复失败只提示一次，状态栏持续展示
      if (this.errorNotices.shouldShow("sync.run")) {
        new Notice(this.plugin.t("sync.error", { error: message }), 8000);
      }
      return false;
    }
  }

  private localizedError(error: unknown): string {
    return localizeError(error, this.plugin.t.bind(this.plugin), this.plugin.t("common.unknownError"));
  }

  private reportActionFailures(results: readonly TaskResult<OrdinarySyncActionOutcome>[]): void {
    const count = results.filter((result) => !result.ok || result.result?.kind === "deferred_retry").length;
    const message = this.plugin.t("notice.syncFailures", { count });
    this.plugin.setSyncState("error", message);
  }

  private reportTransferNotifications(
    actions: readonly OrdinarySyncAction[],
    results: readonly TaskResult<OrdinarySyncActionOutcome>[],
  ): void {
    const uploaded: string[] = [];
    const downloaded: string[] = [];
    const failed: string[] = [];
    for (const [index, action] of actions.entries()) {
      const result = results[index];
      const resolved = result?.ok === true && result.result?.kind === "resolved";
      if (!resolved) {
        failed.push(action.path);
        continue;
      }
      switch (action.kind) {
        case "upload":
          uploaded.push(action.path);
          break;
        case "download":
          downloaded.push(action.path);
          break;
        default:
          break;
      }
    }
    for (const notice of buildTransferNoticePlan(uploaded, downloaded, failed)) {
      this.showTransferNotice(notice);
    }
  }

  private showTransferNotice(notice: TransferNotice): void {
    switch (notice.kind) {
      case "uploaded":
        new Notice(this.plugin.t(notice.count === undefined ? "sync.uploaded" : "sync.uploadedMany", notice));
        break;
      case "downloaded":
        new Notice(this.plugin.t(notice.count === undefined ? "sync.downloaded" : "sync.downloadedMany", notice));
        break;
      case "failed":
        new Notice(this.plugin.t("notice.syncFileFailed", notice), 6000);
        break;
    }
  }

  private async fetchRemote(vaultID: string, forceFull: boolean): Promise<RemoteSnapshot> {
    const files = new Map<string, SyncFileMeta>();
    let useManifest = forceFull;
    let cursor = useManifest ? 0 : this.baseline.getCursor();
    let recoverySnapshot = false;
    while (true) {
      let page;
      try {
        page = useManifest ? await this.api.manifest(vaultID, cursor) : await this.api.changes(vaultID, cursor);
      } catch (error: unknown) {
        if (!useManifest && error instanceof OSSApiError && error.status === 410 && error.code === "history_compacted") {
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

  private async prepareRecoveryRenames(operations: readonly PendingOperation[], remote: Map<string, SyncFileMeta>): Promise<void> {
    for (const operation of operations) {
      if (operation.kind !== "rename" || !operation.oldPath) continue;
      if (this.baseline.get(operation.oldPath) === null || remote.has(operation.oldPath)) continue;
      const local = await this.readLocalMeta(operation.path);
      this.baseline.remove(operation.oldPath);
      this.baseline.removePending(operation.id);
      if (local !== null) this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
    }
    await this.baseline.save();
  }

  private async processRenames(vaultID: string, operations: readonly PendingOperation[], remote: Map<string, SyncFileMeta>): Promise<void> {
    for (const operation of operations) {
      if (operation.kind !== "rename" || !operation.oldPath) continue;
      if (this.baseline.getConflict(operation.oldPath) || this.baseline.getConflict(operation.path)) continue;
      const oldBaseline = this.baseline.get(operation.oldPath);
      const newBaseline = this.baseline.get(operation.path);
      const target = await this.ordinaryFileAccess.readExact(operation.path);
      const local = localMeta(operation.path, target);
      if (!oldBaseline || !local || !target) {
        this.baseline.removePending(operation.id);
        if (local) this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
        continue;
      }
      const sourceRemote = remote.get(operation.oldPath);
      const targetRemote = remote.get(operation.path);
      if (sourceRemote && sourceRemote.revision !== oldBaseline.serverRevision) {
        await this.preserveBothAfterRenameSourceConflict(vaultID, operation, sourceRemote, remote);
        continue;
      }
      if (targetRemote && targetRemote.revision !== (newBaseline?.serverRevision ?? 0)) {
        await this.recordConflict(operation.path, target, targetRemote);
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
        const current = await this.ordinaryFileAccess.readExact(operation.path);
        this.baseline.set(operation.oldPath, this.deletedAcknowledgement(result.old, null));
        this.baseline.set(operation.path, await this.liveAcknowledgement({
          path: operation.path,
          server: result.new,
          local: current,
          bytes: target.bytes,
          bytesHash: target.hash,
        }));
        this.baseline.removePending(operation.id);
        this.queueCurrentLocalChange(operation.path, target, current);
      } catch (error: unknown) {
        if (!isOrdinarySyncConflict409(error)) throw error;
        const path = normalizePath(error.current.path || operation.path);
        if (path === operation.oldPath) {
          await this.preserveBothAfterRenameSourceConflict(vaultID, operation, error.current, remote);
        } else {
          await this.recordConflict(path, target, error.current);
        }
      }
    }
    await this.baseline.save();
  }

  private async preserveBothAfterRenameSourceConflict(vaultID: string, operation: PendingOperation, source: SyncFileMeta, remote: Map<string, SyncFileMeta>): Promise<void> {
    remote.set(source.path, source);
    const expected = expectedFromSnapshot(await this.ordinaryFileAccess.readExact(source.path));
    const outcome = source.deleted ? await this.applyRemoteDelete(source, expected) : await this.applyDownload(vaultID, source, expected);
    if (outcome.kind !== "resolved") return;
    this.baseline.removePending(operation.id);
    this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
    new Notice(this.plugin.t("sync.remoteRenameConflict", { path: operation.oldPath ?? operation.path }), 8000);
  }

  private async planOrdinaryActions(forceFull: boolean, remote: Map<string, SyncFileMeta>, pending: readonly PendingOperation[], recoverySnapshot = false): Promise<OrdinarySyncPlanResult> {
    const baseline = this.baselineSnapshot();
    const vaultPaths = this.vaultPaths(forceFull);
    const candidates = ordinarySyncCandidatePaths({ forceFull, remote, baseline, pending, conflicts: new Set(), vaultPaths });
    const conflicts = new Set(candidates.filter((path) => this.baseline.getConflict(path) !== null));
    const localByPath = new Map<string, OrdinarySyncLocalMeta | null>();
    for (const path of ordinarySyncCandidatePaths({ forceFull, remote, baseline, pending, conflicts, vaultPaths })) {
      localByPath.set(path, await this.readLocalMeta(path));
    }
    return planOrdinarySyncActions({
      forceFull,
      recoverySnapshot,
      remote,
      baseline,
      pending,
      localByPath,
      conflicts,
      vaultPaths,
      createOperationId: operationID,
    });
  }

  private async planActions(forceFull: boolean, remote: Map<string, SyncFileMeta>, pending: readonly PendingOperation[], recoverySnapshot = false): Promise<readonly OrdinarySyncAction[]> {
    return (await this.planOrdinaryActions(forceFull, remote, pending, recoverySnapshot)).actions;
  }

  private baselineSnapshot(): Map<string, BaselineEntry> {
    const entries = new Map<string, BaselineEntry>();
    for (const path of this.baseline.paths()) {
      const entry = this.baseline.get(path);
      if (entry) entries.set(path, entry);
    }
    return entries;
  }

  private vaultPaths(forceFull: boolean): readonly string[] {
    if (!forceFull) return [];
    return this.vault.getFiles()
      .filter((file) => shouldSync(file.path, this.plugin.settings.syncPoisonObsidianFiles))
      .map((file) => normalizePath(file.path));
  }

  private async applyPlanEffects(plan: OrdinarySyncPlanResult): Promise<void> {
    for (const id of plan.obsoletePendingIds) this.baseline.removePending(id);
    for (const path of plan.removedBaselinePaths) this.baseline.remove(path);
    await this.baseline.save();
  }

  private needsFullSnapshotForDeletes(pending: readonly PendingOperation[]): boolean {
    return pending.some((operation) => operation.kind === "delete" && this.baseline.get(operation.path) === null);
  }

  private async applyAction(vaultID: string, action: OrdinarySyncAction): Promise<OrdinarySyncActionOutcome> {
    try {
      switch (action.kind) {
        case "upload": return await this.applyUpload(vaultID, action);
        case "delete_remote": return await this.applyRemoteDeleteRequest(vaultID, action);
        case "download": return await this.applyDownload(vaultID, action.remote, action.expectedLocal);
        case "delete_local": return await this.applyRemoteDelete(action.remote, action.expectedLocal);
        case "delete_local_absent": return await this.applyCompactedDelete(action);
        case "adopt": return await this.applyAdopt(action);
        case "conflict": return await this.applyConflict(action);
        case "reconcile": return await this.resolveReconciliation(vaultID, action);
        default: return assertNeverOrdinarySyncAction(action);
      }
    } catch (error: unknown) {
      return this.handleActionFailure(vaultID, action, error);
    }
  }

  private async handleActionFailure(vaultID: string, action: OrdinarySyncAction, error: unknown): Promise<OrdinarySyncActionOutcome> {
    if (!isOrdinarySyncConflict409(error)) throw error;
    if (action.kind === "upload") {
      return this.createResolver().resolve({ vaultId: vaultID, path: action.path, expectedHash: action.local.hash, remote: error.current });
    }
    const local = await this.ordinaryFileAccess.readExact(action.path);
    await this.recordConflict(action.path, local, error.current);
    return { kind: "conflicted" };
  }

  private async applyUpload(vaultID: string, action: Extract<OrdinarySyncAction, { readonly kind: "upload" }>): Promise<OrdinarySyncActionOutcome> {
    const snapshot = await this.ordinaryFileAccess.readExact(action.path);
    if (!snapshot || snapshot.hash !== action.local.hash) return { kind: "deferred_retry", reason: "stale_local" };
    const server = await this.api.uploadV2(vaultID, {
      path: action.path,
      baseRevision: action.baseRevision,
      hash: snapshot.hash,
      mtime: snapshot.mtime,
      operationID: action.operationID,
      content: toArrayBuffer(snapshot.bytes),
    });
    const current = await this.ordinaryFileAccess.readExact(action.path);
    this.baseline.set(action.path, server.deleted
      ? this.deletedAcknowledgement(server, current)
      : await this.liveAcknowledgement({ path: action.path, server, local: current, bytes: snapshot.bytes, bytesHash: snapshot.hash }));
    if (action.operation) this.baseline.removePending(action.operation.id);
    this.queueCurrentLocalChange(action.path, snapshot, current);
    return { kind: "resolved" };
  }

  private async applyRemoteDeleteRequest(vaultID: string, action: Extract<OrdinarySyncAction, { readonly kind: "delete_remote" }>): Promise<OrdinarySyncActionOutcome> {
    const server = await this.api.deleteV2(vaultID, {
      path: action.path,
      baseRevision: action.baseRevision,
      operationID: action.operationID,
      mtime: Date.now(),
    });
    const current = await this.ordinaryFileAccess.readExact(action.path);
    this.baseline.set(action.path, this.deletedAcknowledgement(server, current));
    if (action.operation) this.baseline.removePending(action.operation.id);
    if (current) this.baseline.putPending({ id: operationID(), kind: "upsert", path: action.path, createdAt: Date.now() });
    return { kind: "resolved" };
  }

  private async applyDownload(vaultID: string, remote: SyncFileMeta, expected: ExpectedLocalState): Promise<OrdinarySyncActionOutcome> {
    let downloaded;
    try {
      downloaded = await this.api.downloadV2(vaultID, remote.path, remote.revision);
    } catch (error) {
      if (!isOrdinarySyncConflict409(error)) throw error;
      return this.conflictOutcome(remote.path, await this.ordinaryFileAccess.readExact(remote.path), error.current);
    }
    const bytes = new Uint8Array(downloaded.content).slice();
    const write = expected.kind === "absent"
      ? await this.ordinaryFileAccess.create(remote.path, expected, bytes)
      : await this.ordinaryFileAccess.replace(remote.path, expected, bytes);
    if (write.kind === "stale") {
      await this.recordConflict(remote.path, write.actual, downloaded.meta);
      return { kind: "conflicted" };
    }
    const snapshot = write.snapshot;
    this.baseline.set(remote.path, await this.liveAcknowledgement({
      path: remote.path,
      server: downloaded.meta,
      local: snapshot,
      bytes,
      bytesHash: await sha256Hex(bytes),
    }));
    this.baseline.removePendingForPath(remote.path);
    return { kind: "resolved" };
  }

  private async applyRemoteDelete(remote: SyncFileMeta, expected: ExpectedLocalState): Promise<OrdinarySyncActionOutcome> {
    const deleted = await this.ordinaryFileAccess.deleteExact(remote.path, expected);
    if (deleted.kind === "stale") {
      await this.recordConflict(remote.path, deleted.actual, remote);
      return { kind: "conflicted" };
    }
    const appeared = await this.ordinaryFileAccess.readExact(remote.path);
    this.baseline.set(remote.path, this.deletedAcknowledgement(remote, appeared));
    this.baseline.removePendingForPath(remote.path);
    if (appeared) this.baseline.putPending({ id: operationID(), kind: "upsert", path: remote.path, createdAt: Date.now() });
    return { kind: "resolved" };
  }

  private async applyCompactedDelete(action: Extract<OrdinarySyncAction, { readonly kind: "delete_local_absent" }>): Promise<OrdinarySyncActionOutcome> {
    const remote = compactedDeleteMeta(action.path);
    const deleted = await this.ordinaryFileAccess.deleteExact(action.path, action.expectedLocal);
    if (deleted.kind === "stale") {
      await this.recordConflict(action.path, deleted.actual, remote);
      return { kind: "conflicted" };
    }
    this.baseline.remove(action.path);
    this.baseline.removePendingForPath(action.path);
    return { kind: "resolved" };
  }

  private async applyAdopt(action: Extract<OrdinarySyncAction, { readonly kind: "adopt" }>): Promise<OrdinarySyncActionOutcome> {
    const local = await this.ordinaryFileAccess.readExact(action.path);
    if (!matchesExpected(local, action.expectedLocal)) return this.conflictOutcome(action.path, local, action.remote);
    if (action.remote.deleted) {
      if (local || action.expectedLocal.kind !== "absent") return this.conflictOutcome(action.path, local, action.remote);
      this.baseline.set(action.path, this.deletedAcknowledgement(action.remote, null));
    } else {
      if (!local || local.hash !== action.remote.hash) return this.conflictOutcome(action.path, local, action.remote);
      this.baseline.set(action.path, await this.liveAcknowledgement({
        path: action.path,
        server: action.remote,
        local,
        bytes: local.bytes,
        bytesHash: local.hash,
      }));
    }
    this.baseline.removePendingForPath(action.path);
    return { kind: "resolved" };
  }

  private async applyConflict(action: Extract<OrdinarySyncAction, { readonly kind: "conflict" }>): Promise<OrdinarySyncActionOutcome> {
    return this.conflictOutcome(action.path, await this.ordinaryFileAccess.readExact(action.path), action.remote);
  }

  private resolveReconciliation(vaultID: string, action: Extract<OrdinarySyncAction, { readonly kind: "reconcile" }>): Promise<OrdinarySyncActionOutcome> {
    return this.createResolver().resolve({ vaultId: vaultID, path: action.path, expectedHash: action.local.hash, remote: action.remote });
  }

  private createResolver(): OrdinarySyncConflictResolver {
    return new OrdinarySyncConflictResolver({
      baseline: this.baseline,
      fileAccess: this.ordinaryFileAccess,
      api: this.api,
      recordConflict: (path, local, remote) => this.recordConflict(path, local, remote),
      createOperationID: operationID,
      now: () => Date.now(),
    });
  }

  private async conflictOutcome(path: string, local: LocalReadResult | null, remote: SyncFileMeta): Promise<OrdinarySyncActionOutcome> {
    await this.recordConflict(path, local, remote);
    return { kind: "conflicted" };
  }

  private async liveAcknowledgement(input: LiveAcknowledgement): Promise<BaselineEntry> {
    if (input.server.hash !== input.bytesHash) {
      return {
        serverRevision: input.server.revision,
        serverHash: input.server.hash,
        serverDeleted: false,
        localHash: input.local?.hash ?? "",
        localMTime: input.local?.mtime ?? 0,
        localSize: input.local?.size ?? 0,
      };
    }
    return baselineFromAcknowledgement({
      kind: "live",
      path: input.path,
      bytes: input.bytes,
      serverRevision: input.server.revision,
      serverHash: input.server.hash,
      localHash: input.local?.hash ?? "",
      localMTime: input.local?.mtime ?? 0,
      localSize: input.local?.size ?? 0,
    });
  }

  private deletedAcknowledgement(remote: SyncFileMeta, local: LocalReadResult | null): BaselineEntry {
    return baselineFromAcknowledgement({
      kind: "deleted",
      path: remote.path,
      serverRevision: remote.revision,
      serverHash: remote.hash,
      localHash: local?.hash ?? "",
      localMTime: local?.mtime ?? 0,
      localSize: local?.size ?? 0,
    });
  }

  private queueCurrentLocalChange(path: string, sent: LocalReadResult | null, current: LocalReadResult | null): void {
    if (!current && sent) {
      this.baseline.putPending({ id: operationID(), kind: "delete", path, createdAt: Date.now() });
    } else if (current && (!sent || current.hash !== sent.hash)) {
      this.baseline.putPending({ id: operationID(), kind: "upsert", path, createdAt: Date.now() });
    }
  }

  private async recordConflict(path: string, local: LocalReadResult | null, remote: SyncFileMeta): Promise<void> {
    const existed = this.baseline.getConflict(path) !== null;
    this.baseline.putConflict({
      path,
      localHash: local?.hash ?? "",
      remoteRevision: remote.revision,
      remoteHash: remote.hash,
      remoteDeleted: remote.deleted,
      remoteMTime: remote.mtime,
      remoteSize: remote.size,
      remoteType: remote.type,
      detectedAt: Date.now(),
    });
    await this.baseline.save();
    if (!existed && !remote.deleted && remote.type === "markdown" && local) {
      this.plugin.openConflictModal(path);
    } else if (!existed) {
      new Notice(this.plugin.t("sync.conflictPaused", { path }), 8000);
    }
  }

  async resolveConflict(path: string, resolution: ConflictResolution): Promise<void> {
    const vaultID = this.plugin.settings.vaultId;
    const conflict = this.baseline.getConflict(path);
    if (!vaultID || !conflict) throw new Error(this.plugin.t("sync.conflictNotFound"));
    const expected = expectedFromHash(conflict.localHash);
    let outcome: OrdinarySyncActionOutcome;
    if (resolution === "keep_both" && conflict.remoteType === "attachment" &&
        !conflict.remoteDeleted && conflict.localHash === conflict.remoteHash) {
      try {
        outcome = await this.createResolver().resolve({
          vaultId: vaultID, path, expectedHash: conflict.localHash, remote: conflictToMeta(conflict),
        });
      } catch (error) {
        if (!isOrdinarySyncConflict409(error)) throw error;
        outcome = await this.conflictOutcome(path, await this.ordinaryFileAccess.readExact(path), error.current);
      }
    } else if (typeof resolution === "object" && (resolution as { kind: string }).kind === "ordered_merge") {
      if (conflict.remoteType === "attachment") throw new Error(this.plugin.t("notice.conflictTextUnavailable"));
      outcome = await this.orderedMergeConflict(vaultID, conflict, expected, (resolution as { kind: "ordered_merge"; content: string }).content);
    } else {
      switch (resolution as "accept_remote" | "force_local" | "keep_both") {
        case "accept_remote":
          outcome = conflict.remoteDeleted
            ? await this.applyRemoteDelete(conflictToMeta(conflict), expected)
            : await this.applyDownload(vaultID, conflictToMeta(conflict), expected);
          break;
        case "force_local":
          outcome = await this.forceLocalConflict(vaultID, conflict, expected);
          break;
        case "keep_both":
          outcome = await this.keepBothConflict(vaultID, conflict, expected);
          break;
        default:
          return assertNeverResolution(resolution as never);
      }
    }
    if (outcome.kind !== "resolved") throw new Error(this.plugin.t("sync.conflictNotFound"));
    this.baseline.removeConflict(path);
    await this.baseline.save();
  }

  private async orderedMergeConflict(vaultID: string, conflict: ConflictEntry, expected: ExpectedLocalState, mergedContent: string): Promise<OrdinarySyncActionOutcome> {
    const local = await this.ordinaryFileAccess.readExact(conflict.path);
    if (!matchesExpected(local, expected)) return this.conflictOutcome(conflict.path, local, conflictToMeta(conflict));
    const mergedBytes = new TextEncoder().encode(mergedContent);
    this.baseline.removePendingForPath(conflict.path);
    const op = { id: operationID(), kind: "upsert" as const, path: conflict.path, createdAt: Date.now() };
    this.baseline.putPending(op);
    await this.baseline.save();
    const installed = await this.ordinaryFileAccess.replace(conflict.path, expected, mergedBytes);
    if (installed.kind !== "replaced") {
      this.baseline.removePending(op.id);
      await this.baseline.save();
      return this.conflictOutcome(conflict.path, installed.actual ?? null, conflictToMeta(conflict));
    }
    this.baseline.set(conflict.path, await this.liveAcknowledgement({
      path: conflict.path,
      server: conflictToMeta(conflict),
      local: installed.snapshot,
      bytes: mergedBytes,
      bytesHash: installed.snapshot.hash,
    }));
    await this.baseline.save();
    let acknowledged: import("./api.js").SyncFileMeta;
    try {
      acknowledged = await this.api.uploadV2(vaultID, {
        path: conflict.path,
        baseRevision: conflict.remoteRevision,
        hash: installed.snapshot.hash,
        mtime: installed.snapshot.mtime,
        operationID: op.id,
        content: mergedBytes.buffer as ArrayBuffer,
      });
    } catch (error) {
      if (!isOrdinarySyncConflict409(error)) throw error;
      return this.conflictOutcome(conflict.path, installed.snapshot, error.current);
    }
    const current = await this.ordinaryFileAccess.readExact(conflict.path);
    this.baseline.set(conflict.path, await this.liveAcknowledgement({
      path: conflict.path,
      server: acknowledged,
      local: current,
      bytes: mergedBytes,
      bytesHash: installed.snapshot.hash,
    }));
    this.baseline.removePending(op.id);
    await this.baseline.save();
    return { kind: "resolved" };
  }

  private async forceLocalConflict(vaultID: string, conflict: ConflictEntry, expected: ExpectedLocalState): Promise<OrdinarySyncActionOutcome> {
    const local = await this.ordinaryFileAccess.readExact(conflict.path);
    if (!matchesExpected(local, expected)) return this.conflictOutcome(conflict.path, local, conflictToMeta(conflict));
    this.baseline.removePendingForPath(conflict.path);
    try {
      if (!local) {
        const server = await this.api.deleteV2(vaultID, {
          path: conflict.path,
          baseRevision: conflict.remoteRevision,
          operationID: operationID(),
          mtime: Date.now(),
        });
        const current = await this.ordinaryFileAccess.readExact(conflict.path);
        this.baseline.set(conflict.path, this.deletedAcknowledgement(server, current));
        this.queueCurrentLocalChange(conflict.path, local, current);
        return { kind: "resolved" };
      }
      const server = await this.api.uploadV2(vaultID, {
        path: conflict.path,
        baseRevision: conflict.remoteRevision,
        hash: local.hash,
        mtime: local.mtime,
        operationID: operationID(),
        content: toArrayBuffer(local.bytes),
      });
      const current = await this.ordinaryFileAccess.readExact(conflict.path);
      this.baseline.set(conflict.path, await this.liveAcknowledgement({
        path: conflict.path,
        server,
        local: current,
        bytes: local.bytes,
        bytesHash: local.hash,
      }));
      this.queueCurrentLocalChange(conflict.path, local, current);
      return { kind: "resolved" };
    } catch (error: unknown) {
      if (!isOrdinarySyncConflict409(error)) throw error;
      return this.conflictOutcome(conflict.path, local, error.current);
    }
  }

  private async keepBothConflict(vaultID: string, conflict: ConflictEntry, expected: ExpectedLocalState): Promise<OrdinarySyncActionOutcome> {
    const local = await this.ordinaryFileAccess.readExact(conflict.path);
    if (!local || !matchesExpected(local, expected)) return this.conflictOutcome(conflict.path, local, conflictToMeta(conflict));
    this.baseline.removePendingForPath(conflict.path);
    const copy = await this.ordinaryFileAccess.preserveSibling(conflict.path, expected, local.bytes);
    if (copy.kind !== "preserved") {
      return this.conflictOutcome(conflict.path, copy.kind === "stale" ? copy.actual ?? null : local, conflictToMeta(conflict));
    }
    this.baseline.putPending({ id: operationID(), kind: "upsert", path: copy.siblingPath, createdAt: Date.now() });
    await this.baseline.save();
    const remote = conflictToMeta(conflict);
    const outcome = remote.deleted
      ? await this.applyRemoteDelete(remote, expected)
      : await this.applyDownload(vaultID, remote, expected);
    return outcome;
  }

  getConflict(path: string): ConflictEntry | null {
    return this.baseline.getConflict(path);
  }

  getBaseline(path: string): BaselineEntry | null {
    return this.baseline.get(path);
  }

  dismissConflict(path: string): void {
    this.baseline.removeConflict(path);
    void this.baseline.save();
  }

  private async readLocalMeta(path: string): Promise<OrdinarySyncLocalMeta | null> {
    return localMeta(path, await this.ordinaryFileAccess.readExact(path));
  }

  private suppress(path: string): void {
    const normalized = normalizePath(path);
    this.suppressed.add(normalized);
    window.setTimeout(() => this.suppressed.delete(normalized), 1500);
  }

  private createDebounce(): (() => void) & { cancel: () => void } {
    return debounce(() => void this.runOnce({ forceFull: false }), Math.max(this.strategy.getMinDebounceSec(), this.plugin.settings.syncIntervalSec) * 1000);
  }
}

function hasUnresolvedAction(results: readonly TaskResult<OrdinarySyncActionOutcome>[]): boolean {
  for (const result of results) {
    if (!result.ok || result.result === undefined) return true;
    switch (result.result.kind) {
      case "resolved":
      case "conflicted":
        break;
      case "deferred_retry":
        return true;
      default:
        return assertNeverOutcome(result.result);
    }
  }
  return false;
}

function localMeta(path: string, local: LocalReadResult | null): OrdinarySyncLocalMeta | null {
  return local ? { path, hash: local.hash, mtime: local.mtime, size: local.size } : null;
}

function expectedFromSnapshot(local: LocalReadResult | null): ExpectedLocalState {
  return local ? { kind: "hash", hash: local.hash } : { kind: "absent" };
}

function expectedFromHash(hash: string): ExpectedLocalState {
  return hash ? { kind: "hash", hash } : { kind: "absent" };
}

function matchesExpected(local: LocalReadResult | null, expected: ExpectedLocalState): boolean {
  return expected.kind === "absent" ? local === null : local !== null && local.hash === expected.hash;
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
  return { path, type: classifyPath(path), hash: "", size: 0, mtime: 0, revision: 0, deleted: true };
}

function classifyPath(path: string): "markdown" | "attachment" | "config" {
  if (path.toLowerCase().endsWith(".md")) return "markdown";
  return path.toLowerCase().startsWith(".obsidian/") ? "config" : "attachment";
}

function toArrayBuffer(bytes: Uint8Array): ArrayBuffer {
  const copy = new Uint8Array(bytes.byteLength);
  copy.set(bytes);
  return copy.buffer;
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes.slice());
  return Array.from(new Uint8Array(digest)).map((value) => value.toString(16).padStart(2, "0")).join("");
}

function operationID(): string {
  return typeof crypto.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

function assertNeverOutcome(value: never): never {
  throw new Error(`Unhandled ordinary sync outcome: ${String(value)}`);
}

function assertNeverResolution(value: never): never {
  throw new Error(`Unhandled conflict resolution: ${String(value)}`);
}

function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}
