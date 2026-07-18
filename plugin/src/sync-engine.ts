// 同步引擎：把 black list + baseline + api + task pool 串起来。
//
// 工作流（决策 1 三态判定在 check 侧闭环）：
//   1. 收集 vault 文件树（按黑名单过滤）。
//   2. 调 /api/sync/check，传 mode=incremental|full。
//   3. 对 check 返回的状态做客户端基线兜底判定（决策 1）：
//      - 服务端返回 upload_needed → 直接进上传队列。
//      - 服务端返回 download_needed → 进下载队列。
//      - 服务端返回 conflict_detected → Phase 5 弹 Diff Modal；Phase 3 先把文件加入「待人工处理」列表。
//      - 服务端返回 in_sync → 跳过。
//      - 服务端返回 assume_in_sync → 跳过（决策 6.5 占位）。
//   4. 用 TaskPool 并发执行上传/下载。
//   5. 仅在成功后更新基线（决策 1）。
//
// 决策 7.1：上传时 mtime = 本地真实 mtime + api.timeOffset。
// 决策 7.2：TaskPool maxConcurrency = settings.maxConcurrency。
// 决策 6.5：增量模式下，仅提交本端 mtime 有变动（相对基线）的文件；
//          force full 时提交全部。

import debounce from "lodash.debounce";
import { App, Notice, TFile, TAbstractFile, Vault } from "obsidian";
import type OSSPlugin from "./main";
import { BaselineStore, normalizePath } from "./baseline";
import { OSSApiClient } from "./api";
import { TaskPool } from "./task-pool";
import { shouldSync } from "./blacklist";
import { SyncRunCoordinator } from "./sync-run-coordinator";

export type SyncState = "idle" | "syncing" | "error";

interface PendingChange {
  path: string;
  kind: "modify" | "create" | "delete";
}

export class SyncEngine {
  private api: OSSApiClient;
  private baseline: BaselineStore;
  private vault: Vault;
  private settings: () => any;
  private setStatus: (state: SyncState, label?: string) => void;

  private pending = new Set<string>();
  private debounceFn: (() => void) & { cancel: () => void };

  private conflictQueue = new Set<string>();
  private runCoordinator = new SyncRunCoordinator();

  constructor(
    app: App,
    api: OSSApiClient,
    baseline: BaselineStore,
    private plugin: OSSPlugin
  ) {
    this.api = api;
    this.baseline = baseline;
    this.vault = app.vault;
    this.settings = () => plugin.settings;
    this.setStatus = (state, label) => plugin.setSyncState(state, label);

    this.debounceFn = debounce(
      () => this.runOnce({ forceFull: false }),
      Math.max(5, plugin.settings.syncIntervalSec) * 1000
    );
  }

  /** 文件变动事件入队。Phase 3 三监听器调。 */
  enqueue(path: string): void {
    if (!shouldSync(path, this.settings().syncPoisonObsidianFiles)) return;
    this.pending.add(normalizePath(path));
    this.debounceFn();
  }

  /** 防抖时间变更时，重置 debounce。 */
  resetDebounce(): void {
    this.debounceFn.cancel();
    this.debounceFn = debounce(
      () => this.runOnce({ forceFull: false }),
      Math.max(5, this.settings().syncIntervalSec) * 1000
    );
  }

  /**
   * 执行一次同步。
   * @param forceFull 决策 6.5：true 时提交全部文件（mode=full）；
   *                  false 时只提交 pending（mode=incremental）。
   */
  async runOnce(opts: { forceFull: boolean }): Promise<void> {
    await this.runCoordinator.run(opts.forceFull, async (forceFull) => {
      await this.executeRun(forceFull);
    });
  }

  private async executeRun(forceFull: boolean): Promise<void> {
    if (!this.api.hasToken()) {
      this.setStatus("error", "not logged in");
      return;
    }
    this.setStatus("syncing");

    try {
      await this.baseline.load();
      // 收集要 check 的文件集合
      let paths: string[];
      if (forceFull) {
        paths = await this.collectAllPaths();
      } else {
        paths = Array.from(this.pending);
        this.pending.clear();
      }

      // 算 hash + 本地 mtime
      const checkPayload = await Promise.all(
        paths.map(async (p) => {
          const file = this.vault.getAbstractFileByPath(p);
          if (!(file instanceof TFile)) return null;
          const buf = await this.vault.readBinary(file as TFile);
          const hash = await sha256Hex(buf);
          const localMtime = (file as TFile).stat.mtime; // ms
          // 决策 7.1：上传时把本地 mtime 校准到服务器时间基。
          const adjustedMtime = this.api.getAdjustedMtime(localMtime);
          return { path: p, mtime: adjustedMtime, hash };
        })
      );
      const valid = checkPayload.filter((x): x is { path: string; mtime: number; hash: string } => x !== null);

      const mode = forceFull || !this.settings().incrementalCheck ? "full" : "incremental";
      const resp = await this.api.check(valid, mode);

      // 决策 7.1：时钟漂移超阈值警告
      if (this.api.isClockDriftLarge()) {
        const secs = Math.round(this.api.getTimeOffset() / 1000);
        new Notice(`OSS: 检测到本地时钟与服务端偏差 ${secs}s，已自动校正；建议同步系统时间。`, 8000);
      }

      // 分发到上传/下载/冲突队列
      const toUpload: string[] = [];
      const toDownload: string[] = [];
      for (const r of resp.results) {
        switch (r.status) {
          case "upload_needed":
            toUpload.push(r.path);
            break;
          case "download_needed":
            toDownload.push(r.path);
            break;
          case "conflict_detected":
            this.conflictQueue.add(r.path);
            this.plugin.openConflictModal(r.path);
            break;
          case "in_sync":
          case "assume_in_sync":
            break;
        }
      }

      // 执行并发上传
      if (toUpload.length > 0) {
        const pool = new TaskPool({
          maxConcurrency: this.settings().maxConcurrency,
          maxRetries: 3,
          baseDelayMs: 500,
        });
        const results = await pool.run(toUpload, async (path) => {
          const file = this.vault.getAbstractFileByPath(path);
          if (!(file instanceof TFile)) throw new Error("file vanished: " + path);
          const buf = await this.vault.readBinary(file);
          const adjusted = this.api.getAdjustedMtime((file as TFile).stat.mtime);
          const res = await this.api.upload(path, adjusted, buf);
          // 决策 1：仅成功后更新基线
          this.baseline.set(path, { serverMTime: res.mtime, hash: res.hash });
          return res;
        });
        // 收尾：基线落盘
        await this.baseline.save();
        const failed = results.filter((r) => !r.ok);
        if (failed.length > 0) {
          new Notice(`OSS: ${failed.length} 个文件上传失败，已入重试队列`, 6000);
          failed.forEach((r) => {
            // 失败的文件放回 pending，下次自动重试
            if (r.error && r.error.message.includes(":")) {
              // path 在闭包外取不到，这里我们简单地把所有上传项重新入队
            }
          });
          // 简化：失败文件全部重新入 pending
          toUpload.forEach((p) => this.pending.add(p));
        }
      }

      // 执行并发下载
      if (toDownload.length > 0) {
        const pool = new TaskPool({
          maxConcurrency: this.settings().maxConcurrency,
          maxRetries: 3,
          baseDelayMs: 500,
        });
        const results = await pool.run(toDownload, async (path) => {
          const res = await this.api.download(path);
          // 写回 vault
          await this.writeDownloadedFile(path, res.content);
          // 决策 1：服务端 mtime 已对齐，直接入基线
          this.baseline.set(path, { serverMTime: res.mtime, hash: res.hash });
          return res;
        });
        await this.baseline.save();
        const failed = results.filter((r) => !r.ok);
        if (failed.length > 0) {
          new Notice(`OSS: ${failed.length} 个文件下载失败`, 6000);
          toDownload.forEach((p, i) => {
            if (!results[i].ok) this.pending.add(p);
          });
        }
      }

      this.setStatus("idle");
    } catch (e) {
      this.setStatus("error", (e as Error).message);
      new Notice("OSS sync error: " + (e as Error).message, 8000);
    }
  }

  private async writeDownloadedFile(path: string, content: ArrayBuffer): Promise<void> {
    const existing = this.vault.getAbstractFileByPath(path);
    if (existing instanceof TFile) {
      await this.vault.modifyBinary(existing, content);
      return;
    }
    try {
      await this.vault.createBinary(path, content);
    } catch (error: unknown) {
      const appeared = this.vault.getAbstractFileByPath(path);
      if (!(appeared instanceof TFile)) throw error;
      await this.vault.modifyBinary(appeared, content);
    }
  }

  /** 收集 vault 全量可同步文件相对路径。 */
  private async collectAllPaths(): Promise<string[]> {
    const all = this.vault.getFiles();
    const allowPoison = this.settings().syncPoisonObsidianFiles;
    return all
      .map((f) => normalizePath(f.path))
      .filter((p) => shouldSync(p, allowPoison));
  }

  /** Phase 5 弹 Diff Modal 用：取冲突队列。 */
  getConflicts(): string[] {
    return Array.from(this.conflictQueue);
  }

  dismissConflict(path: string): void {
    this.conflictQueue.delete(path);
  }
}

// --- 工具函数 ---

async function sha256Hex(buf: ArrayBuffer): Promise<string> {
  // 使用 SubtleCrypto，Obsidian 桌面端基于 Electron 有 crypto.subtle
  const digest = await crypto.subtle.digest("SHA-256", buf);
  return hexFromBuffer(digest);
}

function hexFromBuffer(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let s = "";
  for (let i = 0; i < bytes.length; i++) {
    s += bytes[i].toString(16).padStart(2, "0");
  }
  return s;
}
