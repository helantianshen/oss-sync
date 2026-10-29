import debounce from "lodash.debounce";
import { Notice } from "obsidian";
import type { App, Vault } from "obsidian";
import { OSSApiClient, OSSApiError, type SyncFileMeta } from "./api.js";
import {
  BaselineStore,
  normalizePath,
  type BaselineEntry,
  type ConflictEntry,
  type PendingOperation,
} from "./baseline.js";
import { shouldSync, classifySyncPath } from "./blacklist.js";
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

/** 同步引擎对插件暴露的运行状态，用于状态栏展示 */
export type SyncState = "idle" | "syncing" | "error";

/** 单次运行拉取到的远端快照，files 以规范化路径为键，recoverySnapshot 表示该快照为历史压缩后的全量兜底 */
type RemoteSnapshot = {
  readonly files: Map<string, SyncFileMeta>;
  readonly nextCursor: number;
  readonly recoverySnapshot: boolean;
};

/** 传输成功后生成基线条目的输入，bytesHash 为实际写入本地的内容哈希 */
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
  private localCheckTimer: number | null = null;
  private localDiscoveryReady = false;
  private lastContentCheck = 0;
  private started = false;
  private stopped = false;
  private effectiveMode: "short_poll" | "long_poll" = "short_poll";
  private longPollGen = 0;
  private longPollController: AbortController | null = null;
  /** 同步失败提示的冷却抑制，成功一次后清零，使下一次失败能立即反馈 */
  private readonly errorNotices = new NoticeGuard();

  /**
   * 构造同步引擎，策略、文件访问与防抖句柄在此初始化，随后由 start 启动轮询
   * @param app Obsidian 应用实例，仅取其 vault
   * @param api 已配置凭据的服务端客户端
   * @param baseline 持久化基线与操作队列
   * @param plugin 插件实例，用于读取设置、输出提示与状态
   * @param diagnostics 可选的诊断事件收集器
   */
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

  /** 恢复同步：清除停止标记并按当前策略重建轮询 */
  start(): void {
    this.stopped = false;
    this.started = true;
    this.lastContentCheck = 0;
    this.resetPolling();
  }

  /** Vault 索引就绪后允许本地对账，避免启动期间将尚未加载的文件判作删除 */
  enableLocalDiscovery(): void {
    this.localDiscoveryReady = true;
  }

  /** 停止同步：取消待触发防抖与进行中的长轮询，不影响已入队未落盘的 pending */
  stop(): void {
    this.stopped = true;
    this.started = false;
    this.debounceFn.cancel();
    this.stopCurrentPolling();
  }

  /** 丢弃旧防抖句柄并以当前策略重建，用于间隔或服务端下发的防抖时长变化 */
  resetDebounce(): void {
    this.debounceFn.cancel();
    this.debounceFn = this.createDebounce();
  }

  /** 等待入队链收敛，确保所有已捕获的本地变更都已写入基线后再执行后续操作 */
  async flushPendingQueue(): Promise<void> {
    await this.enqueueChain;
  }

  /** 重建轮询：无令牌或未绑定 vault 时不启动，长轮询模式下直接进入长轮询 */
  resetPolling(): void {
    this.stopCurrentPolling();
    if (this.stopped || !this.plugin.settings.vaultId || !this.api.hasToken()) return;
    this.localCheckTimer = window.setInterval(() => {
      if (!this.stopped) void this.runOnce({ forceFull: false });
    }, 60_000);
    if (this.effectiveMode === "long_poll") return this.startLongPoll();
    const seconds = Math.max(3, this.plugin.settings.remotePollIntervalSec);
    this.pollTimer = window.setInterval(() => {
      if (!this.stopped && this.plugin.settings.vaultId && this.api.hasToken()) {
        void this.runOnce({ forceFull: false });
      }
    }, seconds * 1000);
  }

  /** 重新拉取同步策略并按结果应用轮询模式，供设置页切换模式时调用 */
  async refreshPollingStrategy(): Promise<void> {
    await this.applyStrategy();
  }

  /** 清理短轮询定时器与长轮询，两者互斥，stopCurrentPolling 保证不会并存 */
  private stopCurrentPolling(): void {
    if (this.localCheckTimer !== null) {
      window.clearInterval(this.localCheckTimer);
      this.localCheckTimer = null;
    }
    if (this.pollTimer !== null) {
      window.clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    this.stopLongPoll();
  }

  /** 启动长轮询，用递增 generation 标识本次循环，旧循环因 generation 失配自行退出 */
  private startLongPoll(): void {
    const generation = ++this.longPollGen;
    const controller = new AbortController();
    this.longPollController = controller;
    void this.longPollLoop(generation, controller);
  }

  /** 递增 generation 并中断在途请求，双保险确保旧长轮询不会继续处理结果 */
  private stopLongPoll(): void {
    this.longPollGen += 1;
    this.longPollController?.abort();
    this.longPollController = null;
  }

  /** 长轮询主循环：凭据失效时静默退出；每轮失败休眠 3 秒后重试，提示按 key 冷却 */
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

  /**
   * 拉取服务端同步策略并应用；仅防抖或轮询参数变化时才重建对应句柄
   * OSSApiError 视为策略协商失败，保留既有配置继续同步
   */
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

  /** 路径当前是否处于抑制窗口，抑制由本引擎自身写入触发，避免自产变更再次入队 */
  isSuppressed(path: string): boolean {
    return this.suppressed.has(normalizePath(path));
  }

  /** 当前生效轮询模式的本地化名称，供侧边栏展示 */
  getEffectiveModeLabel(): string {
    return this.plugin.t(this.effectiveMode === "long_poll" ? "common.longPoll" : "common.shortPoll");
  }

  /** 记录一次文件创建或修改，path 为 vault 相对路径 */
  enqueueUpsert(path: string): void {
    this.enqueue({ id: operationID(), kind: "upsert", path, createdAt: Date.now() });
  }

  /** 记录一次文件删除 */
  enqueueDelete(path: string): void {
    this.enqueue({ id: operationID(), kind: "delete", path, createdAt: Date.now() });
  }

  /**
   * 记录一次文件夹删除，展开为目录下全部基线与待办路径的删除操作
   * 直接追加到入队链末尾而非复用 enqueue，避免逐个路径重复触发基线 load/save
   */
  enqueueDeleteTree(folderPath: string): void {
    const root = normalizePath(folderPath).replace(/\/+$/, "");
    if (!root || !shouldSync(root, this.vault.configDir)) return;
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
        if (shouldSync(path, this.vault.configDir)) {
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

  /** 记录一次文件重命名，oldPath 为原路径，path 为新路径 */
  enqueueRename(oldPath: string, newPath: string): void {
    this.enqueueRenames([{ oldPath, path: newPath }]);
  }

  /** 同一次目录移动的子文件共享入队链与一次状态保存 */
  enqueueRenames(moves: readonly { oldPath: string; path: string }[]): void {
    const operations: PendingOperation[] = [];
    for (const move of moves) {
      const oldPath = normalizePath(move.oldPath);
      const path = normalizePath(move.path);
      if ([oldPath, path].some((value) => value === "协作oss" || value.startsWith("协作oss/"))) {
        this.recordDiscovery("collaboration_boundary", 1);
        continue;
      }
      const from = shouldSync(oldPath, this.vault.configDir);
      const to = shouldSync(path, this.vault.configDir);
      if (!from && !to) continue;
      operations.push({ id: operationID(), kind: from && to ? "rename" : from ? "delete" : "upsert", path: to ? path : oldPath, oldPath: from && to ? oldPath : undefined, createdAt: Date.now() });
    }
    if (operations.length === 0) return;
    this.enqueueChain = this.enqueueChain.then(async () => {
      await this.baseline.load();
      for (const operation of operations) this.baseline.putPending(operation);
      await this.baseline.save();
      this.recordDiscovery("event_batch", operations.length, operations[0].id);
      this.diagnostics?.record({ kind: "sync_queue", at: Date.now(), phase: "persisted", operation: operations[0].kind, pendingCount: this.baseline.pending().length });
      this.debounceFn();
    }).catch((error: unknown) => {
      new Notice(this.plugin.t("sync.saveQueueFailed", { error: errorMessage(error, this.plugin.t("common.unknownError")) }));
    });
  }

  /**
   * 串行化入队：黑名单与抑制窗口内的操作直接丢弃
   * 入队失败仅提示，不让后续本地事件因一次持久化失败而丢失
   */
  private enqueue(operation: PendingOperation): void {
    if (!shouldSync(operation.path, this.vault.configDir)) {
      this.recordDiscovery("event_filtered", 1, operation.id);
      return;
    }
    if (operation.oldPath && !shouldSync(operation.oldPath, this.vault.configDir)) return;
    if (this.isSuppressed(operation.path) || (operation.oldPath !== undefined && this.isSuppressed(operation.oldPath))) {
      this.recordDiscovery("event_suppressed", 1, operation.id);
      return;
    }
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

  /**
   * 执行一轮同步，返回是否全部动作成功
   * 并发调用由 runCoordinator 合并，同刻只有一个 executeRun 在跑，避免重复应用同一批 pending
   */
  async runOnce(opts: { readonly forceFull: boolean }): Promise<boolean> {
    let succeeded = true;
    await this.runCoordinator.run(opts.forceFull, async (forceFull) => {
      succeeded = await this.executeRun(forceFull);
    });
    return succeeded;
  }

  /**
   * 单轮同步主流程：协商策略、加载基线、拉远端快照、应用重命名与同步动作、确认游标
   * 任何未处理异常都收敛为 error 状态与一次冷却提示，不向调用方抛出
   */
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
      for (const operation of this.baseline.pending()) {
        if (!shouldSync(operation.path, this.vault.configDir) || operation.oldPath && !shouldSync(operation.oldPath, this.vault.configDir)) {
          this.baseline.removePending(operation.id);
        }
      }
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
      // 首次绑定或切换 vault 时基线为空，必须全量对齐后才能安全做增量
      if (this.baseline.bindVault(vaultID)) {
        full = true;
        await this.baseline.save();
      }
      await this.discoverLocalChanges(forceFull || Date.now() - this.lastContentCheck >= 600_000);
      // 待办删除但基线无对应条目，说明该文件从未同步过，只有全量快照能判断远端是否已不存在
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
      pending = this.baseline.pending();
      const plan = await this.planOrdinaryActions(full || remote.recoverySnapshot, remote.files, pending, remote.recoverySnapshot);
      await this.applyPlanEffects(plan);
      const pool = new TaskPool({
        maxConcurrency: this.plugin.settings.maxConcurrency,
        maxRetries: 2,
        baseDelayMs: 500,
      });
      const first = plan.actions.filter((action) => !(action.kind === "delete_remote" && action.operation?.source === "scan"));
      const deferred = plan.actions.filter((action) => action.kind === "delete_remote" && action.operation?.source === "scan");
      const firstResults = await pool.run(first, (action) => this.applyAction(vaultID, action));
      await this.baseline.save();
      const deleteResults = await pool.run(deferred, (action) => this.applyAction(vaultID, action));
      const ordered = [...first, ...deferred];
      const outcomes = [...firstResults, ...deleteResults];
      const resultsByAction = new Map(ordered.map((action, index) => [action, outcomes[index]!]));
      const results = plan.actions.map((action) => resultsByAction.get(action)!);
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
      // 先向服务端确认游标再落库：确认失败时基线保持旧游标，下一轮重新拉取同一批变更
      await this.api.acknowledge(vaultID, remote.nextCursor);
      this.baseline.setCursor(remote.nextCursor);
      await this.baseline.save();
      this.diagnostics?.record({
        kind: "sync_run",
        at: Date.now(),
        phase: "complete",
        pendingCount: this.baseline.pending().length,
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
      if (this.started && this.baseline.pending().some((operation) => !this.baseline.getConflict(operation.path)
        && (!operation.oldPath || !this.baseline.getConflict(operation.oldPath)))) this.debounceFn();
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

  /** 把任意异常转为当前语言的可读文案，同步链路与提示统一走此入口 */
  private localizedError(error: unknown): string {
    return localizeError(error, this.plugin.t.bind(this.plugin), this.plugin.t("common.unknownError"));
  }

  /** 存在失败或待重试动作时置 error 状态，动作本身的明细由 reportTransferNotifications 输出 */
  private reportActionFailures(results: readonly TaskResult<OrdinarySyncActionOutcome>[]): void {
    const count = results.filter((result) => !result.ok || result.result?.kind === "deferred_retry").length;
    const message = this.plugin.t("notice.syncFailures", { count });
    this.plugin.setSyncState("error", message);
  }

  /** 按 actions 与 results 的同序对应关系归类上传、下载与失败路径，交由 notice plan 合并条数 */
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

  /** 按合并后的通知类别输出单条 Notice，失败提示停留更久 */
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

  /**
   * 分页拉取远端快照，forceFull 时从 manifest 全量拉取，否则从基线游标增量拉取
   * 增量游标被服务端压缩（410 history_compacted）时清空重来并标记 recoverySnapshot，
   * 让上层按首次全量对齐处理，而非当作普通增量差异
   */
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
      for (const file of page.files) {
        if (shouldSync(file.path, this.vault.configDir)) files.set(normalizePath(file.path), file);
      }
      recoverySnapshot = recoverySnapshot || page.recovery_snapshot;
      cursor = page.next_cursor;
      if (!page.has_more) return { files, nextCursor: cursor, recoverySnapshot };
    }
  }

  /**
   * 恢复快照前置处理：源路径在基线与远端都已不存在时，重命名降级为新路径上传
   * 避免把一条指向已消失文件的 rename 发给服务端而必然失败
   */
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

  /**
   * 逐条应用待办重命名，并把远端快照推进到 rename 结果
   * 目标或源端在服务端已变化时走冲突或保留双方分支，不用本地修订号覆盖远端新版本
   */
  private async processRenames(vaultID: string, operations: readonly PendingOperation[], remote: Map<string, SyncFileMeta>): Promise<void> {
    for (const operation of operations) {
      if (operation.kind !== "rename" || !operation.oldPath) continue;
      if (this.baseline.getConflict(operation.oldPath) || this.baseline.getConflict(operation.path)) continue;
      const oldBaseline = this.baseline.get(operation.oldPath);
      const newBaseline = this.baseline.get(operation.path);
      const target = await this.ordinaryFileAccess.readExact(operation.path);
      const local = localMeta(operation.path, target);
      if (!oldBaseline || !local || !target) {
        this.recordDiscovery("rename_fallback", 1, operation.id);
        this.baseline.removePending(operation.id);
        if (local) this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
        continue;
      }
      const sourceRemote = remote.get(operation.oldPath);
      const targetRemote = remote.get(operation.path);
      if (sourceRemote && sourceRemote.revision !== oldBaseline.serverRevision) {
        this.recordDiscovery("rename_conflict", 1, operation.id);
        await this.preserveBothAfterRenameSourceConflict(vaultID, operation, sourceRemote, remote);
        continue;
      }
      if (targetRemote && targetRemote.revision !== (newBaseline?.serverRevision ?? 0)) {
        this.recordDiscovery("rename_conflict", 1, operation.id);
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
        this.recordDiscovery("rename_applied", 1, operation.id);
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
        if (current && current.hash !== result.new.hash) {
          this.baseline.putPending({ id: operationID(), kind: "upsert", path: operation.path, createdAt: Date.now() });
        }
      } catch (error: unknown) {
        if (!isOrdinarySyncConflict409(error)) throw error;
        this.recordDiscovery("rename_conflict", 1, operation.id);
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

  /**
   * 重命名源文件被远端并发修改时，先把远端最新版本落到本地原路径，再把本地改动改为新路径上传
   * 两条历史都保留，rename 待办随之改写为 upsert
   */
  private async preserveBothAfterRenameSourceConflict(vaultID: string, operation: PendingOperation, source: SyncFileMeta, remote: Map<string, SyncFileMeta>): Promise<void> {
    remote.set(source.path, source);
    const expected = expectedFromSnapshot(await this.ordinaryFileAccess.readExact(source.path));
    const outcome = source.deleted ? await this.applyRemoteDelete(source, expected) : await this.applyDownload(vaultID, source, expected);
    if (outcome.kind !== "resolved") return;
    this.baseline.removePending(operation.id);
    this.baseline.putPending({ ...operation, kind: "upsert", oldPath: undefined });
    new Notice(this.plugin.t("sync.remoteRenameConflict", { path: operation.oldPath ?? operation.path }), 8000);
  }

  /**
   * 生成本轮同步计划：先按不含冲突的候选集读取本地元数据，再交给纯规划器产出动作
   * 已标记冲突的路径在第二轮被排除，避免读到冲突现场之外的脏状态
   */
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
      configDir: this.vault.configDir,
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

  /** 只取计划中的动作列表，供不需要计划副作用的调用方使用 */
  private async planActions(forceFull: boolean, remote: Map<string, SyncFileMeta>, pending: readonly PendingOperation[], recoverySnapshot = false): Promise<readonly OrdinarySyncAction[]> {
    return (await this.planOrdinaryActions(forceFull, remote, pending, recoverySnapshot)).actions;
  }

  /** 把基线拍成普通 Map 快照，规划器按此只读视图计算，不触碰 BaselineStore 内部状态 */
  private baselineSnapshot(): Map<string, BaselineEntry> {
    const entries = new Map<string, BaselineEntry>();
    for (const path of this.baseline.paths()) {
      const entry = this.baseline.get(path);
      if (entry && shouldSync(path, this.vault.configDir)) entries.set(path, entry);
    }
    return entries;
  }

  /** 全量模式下返回参与同步的本地文件清单，增量模式下为空集由规划器自行推导 */
  private vaultPaths(forceFull: boolean): readonly string[] {
    if (!forceFull) return [];
    return this.vault.getFiles()
      .filter((file) => shouldSync(file.path, this.vault.configDir))
      .map((file) => normalizePath(file.path));
  }

  /** 只记录事件类别与数量，诊断不包含用户路径或正文 */
  private recordDiscovery(reason: "event_batch" | "collaboration_boundary" | "paths" | "content" | "incomplete" | "delete_deferred" | "event_suppressed" | "event_filtered" | "rename_applied" | "rename_fallback" | "rename_conflict", count: number, operationID?: string, durationMs?: number): void {
    this.diagnostics?.record({ kind: "local_discovery", at: Date.now(), reason, count, operationID, durationMs });
  }

  /** 对照磁盘与 Vault 索引检查可见文件，索引尚未收敛时禁止推导删除 */
  private async indexedLocalPaths(): Promise<Set<string>> {
    const indexed = new Set(this.vault.getFiles().map((file) => normalizePath(file.path))
      .filter((path) => shouldSync(path, this.vault.configDir)));
    const disk = new Set<string>();
    const folders = [""];
    while (folders.length > 0) {
      const folder = folders.pop()!;
      const entries = await this.vault.adapter.list(folder);
      for (const path of entries.files.map(normalizePath)) {
        if (path.split("/").some((part) => part.startsWith("."))) continue;
        if (shouldSync(path, this.vault.configDir)) disk.add(path);
      }
      for (const path of entries.folders.map(normalizePath)) {
        if (path.split("/").some((part) => part.startsWith("."))) continue;
        if (shouldSync(`${path}/`, this.vault.configDir)) folders.push(path);
      }
      if (folders.length > 0) await sleep(0);
    }
    const visible = [...indexed].filter((path) => !path.split("/").some((part) => part.startsWith(".")));
    if (visible.some((path) => !disk.has(path)) || [...disk].some((path) => !indexed.has(path))) {
      throw new Error("Vault index is not ready for local reconciliation");
    }
    return indexed;
  }

  /** 本地枚举与远端游标相互独立，新增及缺失路径始终检查，正文按周期分批读取 */
  private async discoverLocalChanges(content: boolean): Promise<void> {
    if (!this.localDiscoveryReady) return;
    const startedAt = Date.now();
    try {
      const paths = await this.indexedLocalPaths();
      const pending = this.baseline.pending();
      const pendingByPath = new Map(pending.map((operation) => [operation.path, operation]));
      const occupied = new Set(pending.flatMap((operation) => operation.oldPath ? [operation.path, operation.oldPath] : [operation.path]));
      const missing = this.baseline.paths().filter((path) => {
        const base = this.baseline.get(path);
        const operation = pendingByPath.get(path);
        return base && !base.serverDeleted && base.localHash && !paths.has(path) && !this.baseline.getConflict(path)
          && !path.split("/").some((part) => part.startsWith("."))
          && shouldSync(path, this.vault.configDir)
          && (!occupied.has(path) || operation?.kind === "delete" && operation.source === "scan");
      });
      // 缺失源可能覆盖已有目标，必须检查正文后才能安全规划源删除
      content ||= missing.length > 0;
      const dependencies = new Set([...paths].filter((path) => {
        const base = this.baseline.get(path);
        const operation = pendingByPath.get(path);
        return !base || base.serverDeleted || this.baseline.getConflict(path) || operation && operation.kind !== "delete";
      }));
      const operations: PendingOperation[] = [];
      let checked = 0;
      for (const path of paths) {
        if (occupied.has(path) || this.baseline.getConflict(path)) continue;
        const base = this.baseline.get(path);
        if (!base || base.serverDeleted || content) {
          const local = await this.readLocalMeta(path);
          if (!local) throw new Error("Local file changed during reconciliation");
          if (!base || base.serverDeleted || local.hash !== base.serverHash) {
            dependencies.add(path);
            operations.push({ id: operationID(), kind: "upsert", path, source: "scan", createdAt: Date.now() });
          }
          if (++checked % 32 === 0) await sleep(0);
        }
      }
      for (const path of missing) {
        if (await this.vault.adapter.exists(path)) throw new Error("Missing local path is still present on disk");
        const previous = pendingByPath.get(path);
        operations.push({ id: previous?.id ?? operationID(), kind: "delete", path, source: "scan", waitForPaths: [...dependencies], createdAt: previous?.createdAt ?? Date.now() });
      }
      // 读取期间捕获的新事件优先，扫描结果不覆盖已经更新的待办
      const currentPending = this.baseline.pending();
      const currentByPath = new Map(currentPending.map((operation) => [operation.path, operation]));
      const current = new Set(currentPending.flatMap((operation) => operation.oldPath ? [operation.path, operation.oldPath] : [operation.path]));
      for (const operation of operations) {
        const existing = currentByPath.get(operation.path);
        if (!current.has(operation.path)) this.baseline.putPending(operation);
        else if (operation.kind === "delete" && existing?.id === operation.id && existing.source === "scan") {
          existing.waitForPaths = operation.waitForPaths;
        }
      }
      await this.baseline.save();
      if (content) this.lastContentCheck = Date.now();
      this.recordDiscovery(content ? "content" : "paths", paths.size, undefined, Date.now() - startedAt);
    } catch (error) {
      this.recordDiscovery("incomplete", 0);
      throw error;
    }
  }

  /** 扫描删除依赖已确认的目标正文；源重新出现或目标未收敛时保留原路径 */
  private async canApplyScannedDelete(operation: PendingOperation): Promise<boolean> {
    try {
      if (!Array.isArray(operation.waitForPaths) || operation.waitForPaths.some((path) => typeof path !== "string")) return false;
      if (await this.ordinaryFileAccess.readExact(operation.path) || await this.vault.adapter.exists(operation.path)) return false;
      for (const path of operation.waitForPaths) {
        const base = this.baseline.get(path);
        if (!base || base.serverDeleted || this.baseline.getConflict(path) || this.baseline.pending().some((item) => item.path === path || item.oldPath === path)) return false;
        const local = await this.readLocalMeta(path);
        if (!local || local.hash !== base.serverHash) return false;
      }
      return true;
    } catch {
      return false;
    }
  }

  /** 落盘计划副作用：清理过期待办与被移除的基线条目，保证动作执行前基线已反映计划 */
  private async applyPlanEffects(plan: OrdinarySyncPlanResult): Promise<void> {
    for (const id of plan.obsoletePendingIds) this.baseline.removePending(id);
    for (const path of plan.removedBaselinePaths) this.baseline.remove(path);
    await this.baseline.save();
  }

  /** 待办删除但基线无条目时必须全量拉取，否则无法区分远端已删除与本地新增 */
  private needsFullSnapshotForDeletes(pending: readonly PendingOperation[]): boolean {
    return pending.some((operation) => operation.kind === "delete" && this.baseline.get(operation.path) === null);
  }

  /**
   * 执行单个同步动作，异常在此统一兜底
   * 409 冲突转由冲突处理器结算，其余异常上抛给 TaskPool 记为该动作失败
   */
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

  /** 上传遇 409 走冲突解决器，其余动作直接按远端当前版本登记冲突 */
  private async handleActionFailure(vaultID: string, action: OrdinarySyncAction, error: unknown): Promise<OrdinarySyncActionOutcome> {
    if (!isOrdinarySyncConflict409(error)) throw error;
    if (action.kind === "upload") {
      return this.createResolver().resolve({ vaultId: vaultID, path: action.path, expectedHash: action.local.hash, remote: error.current });
    }
    const local = await this.ordinaryFileAccess.readExact(action.path);
    await this.recordConflict(action.path, local, error.current);
    return { kind: "conflicted" };
  }

  /**
   * 上传本地文件；执行前重读校验内容未变
   * 若读取结果与计划时的哈希不一致，返回 deferred_retry 由下一轮重新规划，不把旧内容推上去
   */
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

  /** 请求服务端删除；删除后本地又出现同名文件时补一条 upsert 待办，下一轮重新上传 */
  private async applyRemoteDeleteRequest(vaultID: string, action: Extract<OrdinarySyncAction, { readonly kind: "delete_remote" }>): Promise<OrdinarySyncActionOutcome> {
    if (action.operation?.source === "scan" && !(await this.canApplyScannedDelete(action.operation))) {
      this.recordDiscovery("delete_deferred", 1);
      return { kind: "deferred_retry", reason: "stale_local" };
    }
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

  /**
   * 下载远端版本写入本地；写入前按 expected 做本地状态校验
   * 校验失败（stale）说明写入期间本地已变更，转为冲突交给用户处理，不覆盖本地新内容
   */
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

  /** 删除本地文件以镜像远端删除，同样先做 expected 校验，失败转冲突 */
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

  /**
   * 本地文件缺失时的删除动作，远端元数据由路径推导
   * 历史压缩后远端已无该文件记录，无法比对修订号，直接移除基线与待办
   */
  private async applyCompactedDelete(action: Extract<OrdinarySyncAction, { readonly kind: "delete_local_absent" }>): Promise<OrdinarySyncActionOutcome> {
    const remote = compactedDeleteMeta(action.path, this.vault.configDir);
    const deleted = await this.ordinaryFileAccess.deleteExact(action.path, action.expectedLocal);
    if (deleted.kind === "stale") {
      await this.recordConflict(action.path, deleted.actual, remote);
      return { kind: "conflicted" };
    }
    this.baseline.remove(action.path);
    this.baseline.removePendingForPath(action.path);
    return { kind: "resolved" };
  }

  /** 远端已同步但本地无基线记录时，校验本地内容一致后直接采纳为基线，不产生传输 */
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

  /** 规划阶段已判定为冲突的动作，执行时按本地当前状态登记冲突 */
  private async applyConflict(action: Extract<OrdinarySyncAction, { readonly kind: "conflict" }>): Promise<OrdinarySyncActionOutcome> {
    return this.conflictOutcome(action.path, await this.ordinaryFileAccess.readExact(action.path), action.remote);
  }

  /** 需按远端最新版本重新计算的动作，交给冲突解决器在计划期读取的期望哈希上结算 */
  private resolveReconciliation(vaultID: string, action: Extract<OrdinarySyncAction, { readonly kind: "reconcile" }>): Promise<OrdinarySyncActionOutcome> {
    return this.createResolver().resolve({ vaultId: vaultID, path: action.path, expectedHash: action.local.hash, remote: action.remote });
  }

  /** 构造冲突解决器，统一注入基线与文件访问，冲突登记回调指回本引擎 */
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

  /** 登记冲突并返回 conflicted 结果，供各动作分支统一收敛 */
  private async conflictOutcome(path: string, local: LocalReadResult | null, remote: SyncFileMeta): Promise<OrdinarySyncActionOutcome> {
    await this.recordConflict(path, local, remote);
    return { kind: "conflicted" };
  }

  /**
   * 生成传输成功后的基线条目
   * 服务端返回的哈希与实际上传内容不一致时不写入 baseText，只保留修订号与哈希
   */
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

  /** 生成删除确认后的基线条目，标记该路径在服务端已不存在 */
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

  /**
   * 传输期间本地又发生变化时补一条待办，下一轮重新同步
   * sent 为传输时的快照，current 为传输后重读结果，据此判断该文件是被删除还是被改写
   */
  private queueCurrentLocalChange(path: string, sent: LocalReadResult | null, current: LocalReadResult | null): void {
    if (!current && sent) {
      this.baseline.putPending({ id: operationID(), kind: "delete", path, createdAt: Date.now() });
    } else if (current && (!sent || current.hash !== sent.hash)) {
      this.baseline.putPending({ id: operationID(), kind: "upsert", path, createdAt: Date.now() });
    }
  }

  /**
   * 登记冲突并落盘；首次发现的 markdown 冲突直接弹出解决窗口
   * 二进制或已删除场景只提示，因为无法做文本合并
   */
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

  /**
   * 应用用户在冲突窗口选择的解决方案，成功后移除冲突记录
   * 附件在两侧内容一致时走解决器的保留双方路径；有序合并仅适用于文本
   */
  async resolveConflict(path: string, resolution: ConflictResolution): Promise<void> {
    if (!shouldSync(path, this.vault.configDir)) throw new Error(this.plugin.t("sync.conflictNotFound"));
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

  /**
   * 有序合并方案：先落一条 upsert 待办再写本地，保证合并结果即使在上传前崩溃也会被重传
   * 上传以冲突时的远端修订号为基准，期间远端再变则退回冲突
   */
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

  /** 强制本地方案：本地文件不存在时请求服务端删除，存在时以冲突时的远端修订号为基准上传 */
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

  /** 保留双方方案：本地内容另存为同目录副本，原路径再下载远端版本 */
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

  /** 读取指定路径的冲突记录，供冲突窗口与侧边栏展示 */
  getConflict(path: string): ConflictEntry | null {
    return this.baseline.getConflict(path);
  }

  /** 读取指定路径的基线条目，含合并所需的 baseText */
  getBaseline(path: string): BaselineEntry | null {
    return this.baseline.get(path);
  }

  /** 用户忽略该冲突，直接移除记录不改变任何文件内容 */
  dismissConflict(path: string): void {
    this.baseline.removeConflict(path);
    void this.baseline.save();
  }

  /** 读取路径的本地元数据，文件不存在时返回 null */
  private async readLocalMeta(path: string): Promise<OrdinarySyncLocalMeta | null> {
    return localMeta(path, await this.ordinaryFileAccess.readExact(path));
  }

  /** 写入路径进入抑制窗口 1.5 秒，覆盖本引擎自身触发 vault 事件的典型延迟 */
  private suppress(path: string): void {
    const normalized = normalizePath(path);
    this.suppressed.add(normalized);
    window.setTimeout(() => this.suppressed.delete(normalized), 1500);
  }

  /** 防抖触发一次增量同步，实际间隔取服务端策略与本地设置中的较大值 */
  private createDebounce(): (() => void) & { cancel: () => void } {
    return debounce(() => void this.runOnce({ forceFull: false }), Math.max(this.strategy.getMinDebounceSec(), this.plugin.settings.syncIntervalSec) * 1000);
  }
}

/** 结果中任一动作异常、无结果或待重试时判定本轮未完成，据此跳过游标确认 */
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

/** 把本地读取结果折算为规划器使用的元数据，文件不存在时返回 null */
function localMeta(path: string, local: LocalReadResult | null): OrdinarySyncLocalMeta | null {
  return local ? { path, hash: local.hash, mtime: local.mtime, size: local.size } : null;
}

/** 由本地快照推导期望状态，缺失即期望文件不存在 */
function expectedFromSnapshot(local: LocalReadResult | null): ExpectedLocalState {
  return local ? { kind: "hash", hash: local.hash } : { kind: "absent" };
}

/** 由哈希推导期望状态，空哈希表示期望文件不存在 */
function expectedFromHash(hash: string): ExpectedLocalState {
  return hash ? { kind: "hash", hash } : { kind: "absent" };
}

/** 校验本地读取结果是否仍与期望状态一致，是各方案执行前的统一前置检查 */
function matchesExpected(local: LocalReadResult | null, expected: ExpectedLocalState): boolean {
  return expected.kind === "absent" ? local === null : local !== null && local.hash === expected.hash;
}

/** 把冲突记录还原为服务端元数据，供冲突方案按远端修订号提交 */
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

/** 历史压缩后构造删除元数据，修订号为 0 表示无可比对版本 */
function compactedDeleteMeta(path: string, configDir: string): SyncFileMeta {
  return { path, type: classifySyncPath(path, configDir), hash: "", size: 0, mtime: 0, revision: 0, deleted: true };
}

/** 拷贝为独立 ArrayBuffer，避免把可能被复用的底层缓冲区交给传输层 */
function toArrayBuffer(bytes: Uint8Array): ArrayBuffer {
  const copy = new Uint8Array(bytes.byteLength);
  copy.set(bytes);
  return copy.buffer;
}

/** 计算内容的 SHA-256 十六进制摘要，用于基线与服务端哈希比对 */
async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", bytes.slice());
  return Array.from(new Uint8Array(digest)).map((value) => value.toString(16).padStart(2, "0")).join("");
}

/** 生成本次操作的幂等标识，优先使用 crypto.randomUUID，不可用时退化为时间加随机数 */
function operationID(): string {
  return typeof crypto.randomUUID === "function"
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

/** 提取异常的可读文案，非 Error 值回退到调用方提供的兜底文案 */
function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error ? error.message : fallback;
}

/** 穷尽校验占位，SyncEngine 内新增动作类型未处理时在编译期暴露 */
function assertNeverOutcome(value: never): never {
  throw new Error(`Unhandled ordinary sync outcome: ${String(value)}`);
}

/** 穷尽校验占位，新增冲突解决方案未处理时在编译期暴露 */
function assertNeverResolution(value: never): never {
  throw new Error(`Unhandled conflict resolution: ${String(value)}`);
}

/** 长轮询失败后的固定退避，避免服务端异常时高频重试 */
function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}
