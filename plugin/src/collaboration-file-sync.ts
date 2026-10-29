import { Notice } from "obsidian";
import { normalizePath } from "./baseline.js";
import type { CollaborationBaselineEntry } from "./collaboration-state.js";
import { CollaborationFileVault, isCollabPath } from "./collaboration-file-vault.js";
export { CollaborationFileVault } from "./collaboration-file-vault.js";
import type { CollaborationUploadInput, SyncFileMeta } from "./api.js";
import { localizeError } from "./localized-error.js";
import type { TranslationKey, TranslationParams } from "./i18n.js";
import { CollaborationSyncCoordinator } from "./collaboration-sync-coordinator.js";
export { CollaborationSyncCoordinator };
import {
  isCollaborationConflictError,
  resolveCollaboration409,
} from "./collaboration-cas-resolver.js";
import {
  createOperationID as createValidOperationID,
  isValidOperationID,
  isValidServerRevision,
} from "./operation-id.js";
import { decodeMergeableText } from "./text-merge.js";
import { decideCollaborationReconciliation } from "./collaboration-file-reconcile.js";

/** 已接受的协作文件 */
export interface AcceptedCollaborationFile {
  readonly vaultId: string;
  readonly fileId: number;
  readonly localPath: string;
}

/** 文件同步所需的协作上传能力；downloadCollabContent 可选，缺失时 409 无法自动处理 */
export interface CollaborationFileSyncApi {
  readonly collabUpload: (vaultId: string, fileId: number, input: CollaborationUploadInput) => Promise<SyncFileMeta>;
  readonly downloadCollabContent?: (vaultId: string, fileId: number) => Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null>;
}

/** 翻译能力；只取 t 方法，避免依赖整个插件实例 */
export interface CollaborationFileSyncPlugin {
  readonly t: (key: TranslationKey, params?: TranslationParams) => string;
}

/** 文件同步所需的基线读写能力 */
export interface CollaborationBaseline {
  readonly getCollaboration: (vaultId: string, fileId: number) => CollaborationBaselineEntry | null;
  readonly setCollaboration: (vaultId: string, fileId: number, entry: CollaborationBaselineEntry) => void;
  readonly save: () => Promise<void>;
}

/** 文件同步依赖；coordinator 保证同一时刻只有一个上传在跑 */
export interface CollaborationFileSyncDeps {
  readonly baseline: CollaborationBaseline;
  readonly vault: CollaborationFileVault;
  readonly api: CollaborationFileSyncApi;
  readonly plugin: CollaborationFileSyncPlugin;
  readonly getAccepted: () => readonly AcceptedCollaborationFile[];
  readonly onChange: () => void;
  readonly now: () => number;
  readonly createOperationID: () => string;
  readonly coordinator: CollaborationSyncCoordinator;
}

const UPLOAD_DEBOUNCE_MS = 2000;

export class CollaborationFileSync {
  private readonly timers = new Map<string, number>();

  constructor(private readonly deps: CollaborationFileSyncDeps) {}

/** 本地协作文件被编辑后防抖上传；非协作路径与抑制窗口内的编辑被忽略 */
  handleLocalEdit(path: string): void {
    if (!isCollabPath(path)) return;
    const key = normalizePath(path);
    if (this.deps.vault.isSuppressed(key)) return;
    const existing = this.timers.get(key);
    if (existing !== undefined) window.clearTimeout(existing);
    const timer = window.setTimeout(() => {
      this.timers.delete(key);
      void this.deps.coordinator.run(() => this.doUpload(key));
    }, UPLOAD_DEBOUNCE_MS);
    this.timers.set(key, timer);
  }

  /** 取消全部待执行上传并清除抑制状态 */
  stop(): void {
    for (const timer of this.timers.values()) window.clearTimeout(timer);
    this.timers.clear();
    this.deps.vault.clearSuppressed();
  }

  /** 按本地路径在已接受列表中定位协作文件 */
  private findAccepted(localPath: string): AcceptedCollaborationFile | null {
    const key = normalizePath(localPath);
    for (const accepted of this.deps.getAccepted()) {
      if (normalizePath(accepted.localPath) === key) return accepted;
    }
    return null;
  }

  /** 读取本地正文并上传；未接受、无基线或被抑制时直接跳过 */
  private async doUpload(path: string): Promise<void> {
    const key = normalizePath(path);
    if (this.deps.vault.isSuppressed(key)) return;
    const accepted = this.findAccepted(key);
    if (!accepted) return;
    const entry = this.deps.baseline.getCollaboration(accepted.vaultId, accepted.fileId);
    if (!entry) return;
    const read = await this.deps.vault.readLocal(key);
    if (!read) return;
    if (!isValidServerRevision(entry.serverRevision)) {
      // 哨兵 revision 会被服务端拒绝，先拉取远端重建基线再重试
      await this.recoverFromInvalidRevision(accepted, entry);
      return;
    }
    const baseRevision = entry.serverRevision;
    const operationID = this.resolveOperationID(entry, read.hash);
    if (!isValidOperationID(operationID)) {
      // 防御：若仍非法则重新生成（理论上 resolve 已保证合法）
      const fallback = this.deps.createOperationID?.() ?? createValidOperationID();
      const safe = isValidOperationID(fallback) ? fallback : createValidOperationID();
      const pendingFix: CollaborationBaselineEntry = {
        vaultId: entry.vaultId,
        fileId: entry.fileId,
        localPath: entry.localPath,
        serverRevision: entry.serverRevision,
        serverHash: entry.serverHash,
        localHash: read.hash,
        baseText: entry.baseText,
        pending: { id: safe, createdAt: this.deps.now() },
        conflict: entry.conflict,
      };
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, pendingFix);
      await this.deps.baseline.save();
      // 以修正后的 ID 重试一次
      try {
        const result = await this.deps.api.collabUpload(accepted.vaultId, accepted.fileId, {
          content: read.content,
          baseRevision,
          operationID: safe,
        });
        const next: CollaborationBaselineEntry = {
          vaultId: accepted.vaultId,
          fileId: accepted.fileId,
          localPath: accepted.localPath,
          serverRevision: result.revision,
          serverHash: result.hash,
          localHash: read.hash,
          baseText: read.content,
          pending: null,
          conflict: null,
        };
        this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, next);
        await this.deps.baseline.save();
        new Notice(this.deps.plugin.t("collab.uploaded", { path: accepted.localPath }));
        this.deps.onChange();
      } catch (error) {
        if (isCollaborationConflictError(error)) {
          const download = this.deps.api.downloadCollabContent;
          if (!download) {
            const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
            new Notice(this.deps.plugin.t("collab.uploadFailed", { error: message }));
            return;
          }
          const casApi: { downloadCollabContent: (v: string, f: number) => Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null>; collabUpload: (v: string, f: number, i: CollaborationUploadInput) => Promise<SyncFileMeta> } = {
            downloadCollabContent: download,
            collabUpload: this.deps.api.collabUpload,
          };
          const pendingFor409: CollaborationBaselineEntry = this.deps.baseline.getCollaboration(accepted.vaultId, accepted.fileId) ?? pendingFix;
          await resolveCollaboration409(
            {
              baseline: this.deps.baseline,
              vault: this.deps.vault,
              api: casApi,
              plugin: this.deps.plugin,
              now: this.deps.now,
              createOperationID: this.deps.createOperationID,
              onChange: this.deps.onChange,
            },
            { accepted, entry: pendingFor409 },
          );
          return;
        }
        const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
        new Notice(this.deps.plugin.t("collab.uploadFailed", { error: message }));
      }
      return;
    }
    const pendingEntry: CollaborationBaselineEntry = {
      vaultId: entry.vaultId,
      fileId: entry.fileId,
      localPath: entry.localPath,
      serverRevision: entry.serverRevision,
      serverHash: entry.serverHash,
      localHash: read.hash,
      baseText: entry.baseText,
      pending: { id: operationID, createdAt: this.deps.now() },
      conflict: entry.conflict,
    };
    this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, pendingEntry);
    await this.deps.baseline.save();
    try {
      const result = await this.deps.api.collabUpload(accepted.vaultId, accepted.fileId, {
        content: read.content,
        baseRevision,
        operationID,
      });
      const next: CollaborationBaselineEntry = {
        vaultId: accepted.vaultId,
        fileId: accepted.fileId,
        localPath: accepted.localPath,
        serverRevision: result.revision,
        serverHash: result.hash,
        localHash: read.hash,
        baseText: read.content,
        pending: null,
        conflict: null,
      };
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, next);
      await this.deps.baseline.save();
      new Notice(this.deps.plugin.t("collab.uploaded", { path: accepted.localPath }));
      this.deps.onChange();
    } catch (error) {
      if (isCollaborationConflictError(error)) {
        const download = this.deps.api.downloadCollabContent;
        if (!download) {
          const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
          new Notice(this.deps.plugin.t("collab.uploadFailed", { error: message }));
          return;
        }
        const casApi: { downloadCollabContent: (v: string, f: number) => Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null>; collabUpload: (v: string, f: number, i: CollaborationUploadInput) => Promise<SyncFileMeta> } = {
          downloadCollabContent: download,
          collabUpload: this.deps.api.collabUpload,
        };
        await resolveCollaboration409(
          {
            baseline: this.deps.baseline,
            vault: this.deps.vault,
            api: casApi,
            plugin: this.deps.plugin,
            now: this.deps.now,
            createOperationID: this.deps.createOperationID,
            onChange: this.deps.onChange,
          },
          { accepted, entry: pendingEntry },
        );
        return;
      }
      const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
      new Notice(this.deps.plugin.t("collab.uploadFailed", { error: message }));
    }
  }

  /** 本地未变时复用 pending 的 operationID 以保留幂等性，否则生成新的 */
  private resolveOperationID(entry: CollaborationBaselineEntry, hash: string): string {
    if (entry.pending && entry.localHash === hash && isValidOperationID(entry.pending.id)) {
      return entry.pending.id;
    }
    const created = this.deps.createOperationID?.() ?? createValidOperationID();
    if (isValidOperationID(created)) return created;
    return createValidOperationID();
  }

  /** 哨兵 revision 恢复：拉取远端正文后按对账决策重建本地与基线 */
  private async recoverFromInvalidRevision(
    accepted: AcceptedCollaborationFile,
    entry: CollaborationBaselineEntry,
  ): Promise<void> {
    const download = this.deps.api.downloadCollabContent;
    if (!download) {
      new Notice(this.deps.plugin.t("collab.refreshFailed", { error: this.deps.plugin.t("common.unknownError") }));
      return;
    }
    let fetched: { content: ArrayBuffer; meta: SyncFileMeta } | null = null;
    try {
      fetched = await download(accepted.vaultId, accepted.fileId);
    } catch (error) {
      const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
      new Notice(this.deps.plugin.t("collab.refreshFailed", { error: message }));
      return;
    }
    if (!fetched) {
      new Notice(this.deps.plugin.t("collab.refreshFailed", { error: this.deps.plugin.t("common.unknownError") }));
      return;
    }
    const remoteBytes = new Uint8Array(fetched.content);
    const remoteHash = fetched.meta.hash;
    const remoteRev = fetched.meta.revision;
    if (!isValidServerRevision(remoteRev)) return;
    const localExact = await this.deps.vault.readExact(accepted.localPath);
    const localContent = localExact ? { path: accepted.localPath, bytes: localExact.bytes, hash: localExact.hash } : null;
    const remoteContent = { path: accepted.localPath, bytes: remoteBytes, hash: remoteHash };
    // 使用当前 entry 的 baseText 作为祖先，若无效则按无祖先处理
    const ancestorText = typeof entry.baseText === "string" ? entry.baseText : null;
    // 构造一个临时 baseline 以复用现有三方合并决策
    // 合成一份以远端为准的基线，使未改动判定与远端比对，而非与已失效的本地 revision 比对
    const synthetic: CollaborationBaselineEntry = {
      vaultId: entry.vaultId,
      fileId: entry.fileId,
      localPath: entry.localPath,
      serverRevision: remoteRev,
      serverHash: remoteHash,
      localHash: remoteHash,
      baseText: decodeMergeableText(accepted.localPath, remoteBytes) ?? "",
      pending: null,
      conflict: null,
    };
    // 若本地不存在则直接采用远端
    if (!localContent) {
      await this.deps.vault.writeCanonical(accepted.localPath, remoteBytes);
      const baseText = decodeMergeableText(accepted.localPath, remoteBytes) ?? "";
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
        vaultId: accepted.vaultId,
        fileId: accepted.fileId,
        localPath: accepted.localPath,
        serverRevision: remoteRev,
        serverHash: remoteHash,
        localHash: remoteHash,
        baseText,
        pending: null,
        conflict: null,
      });
      await this.deps.baseline.save();
      new Notice(this.deps.plugin.t("collab.downloaded", { path: accepted.localPath }));
      this.deps.onChange();
      return;
    }
    // 本地已存在则进入正常 reconcile 流程，防止未校验 revision 导致 400
    // 注意：哨兵恢复时 baseline 必须以远端为准判定本地是否已改动，避免 pending 已更新导致误判 keep_local 为 adopt
    const decision = decideCollaborationReconciliation({
      path: accepted.localPath,
      baseline: synthetic,
      ancestorText,
      local: localContent,
      remote: remoteContent,
    });
    // 简化处理：多数情况走统一的持久化策略，需与 remote-sync 保持一致
    // 若三方合并需要保留冲突，则持久化冲突；否则按决策上传或保留本地
    if (decision.kind === "persist_text_conflict") {
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
        vaultId: entry.vaultId,
        fileId: entry.fileId,
        localPath: entry.localPath,
        serverRevision: remoteRev,
        serverHash: remoteHash,
        localHash: localContent.hash,
        baseText: ancestorText ?? "",
        pending: null,
        conflict: { remoteRevision: remoteRev, remoteHash, remoteText: decision.remoteText, detectedAt: this.deps.now() },
      });
      await this.deps.baseline.save();
      this.deps.onChange();
      new Notice(`协作冲突：${accepted.localPath} 存在重叠编辑，已在右侧边栏等待解决`, 8000);
      return;
    }
    if (decision.kind === "keep_local" || decision.kind === "preserve_both") {
      // 保留本地内容但更新 serverRevision 为远端，避免下次仍以无效 revision 上传
      if (decision.kind === "preserve_both") {
        const sibling = `${accepted.localPath.replace(/(\.[^/.]+)?$/, `_conflict_${new Date(this.deps.now()).toISOString().replace(/[:.]/g, "-")}$1`)}`;
        await this.deps.vault.writeCanonical(sibling, localContent.bytes);
      }
      // 若需保留双方或保持本地，先更新 baseline 至远端可信 revision，但不覆盖本地文件（keep_local）
      // 对于 400 恢复，至少应将 serverRevision 修正为远端，以消除下次 400
      const needsAdopt = decision.kind === "preserve_both";
      if (needsAdopt) {
        await this.deps.vault.writeCanonical(accepted.localPath, remoteBytes);
        const baseText = decodeMergeableText(accepted.localPath, remoteBytes) ?? "";
        this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
          vaultId: accepted.vaultId,
          fileId: accepted.fileId,
          localPath: accepted.localPath,
          serverRevision: remoteRev,
          serverHash: remoteHash,
          localHash: remoteHash,
          baseText,
          pending: null,
          conflict: null,
        });
        await this.deps.baseline.save();
        new Notice(this.deps.plugin.t("collab.downloaded", { path: accepted.localPath }));
      } else {
        this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
          vaultId: entry.vaultId,
          fileId: entry.fileId,
          localPath: entry.localPath,
          serverRevision: remoteRev,
          serverHash: remoteHash,
          localHash: localContent.hash,
          baseText: ancestorText ?? "",
          pending: null,
          conflict: null,
        });
        await this.deps.baseline.save();
      }
      this.deps.onChange();
      return;
    }
    if (decision.kind === "adopt_remote" || decision.kind === "apply_remote") {
      await this.deps.vault.writeCanonical(accepted.localPath, remoteBytes);
      const baseText = decodeMergeableText(accepted.localPath, remoteBytes) ?? "";
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
        vaultId: accepted.vaultId,
        fileId: accepted.fileId,
        localPath: accepted.localPath,
        serverRevision: remoteRev,
        serverHash: remoteHash,
        localHash: remoteHash,
        baseText,
        pending: null,
        conflict: null,
      });
      await this.deps.baseline.save();
      new Notice(this.deps.plugin.t("collab.downloaded", { path: accepted.localPath }));
      this.deps.onChange();
      return;
    }
    // upload_local / upload_merged 需以远端 revision 为 base 重试上传
    if (decision.kind === "upload_local" || decision.kind === "upload_merged") {
      const content = decision.content;
      const op = this.resolveOperationID(entry, localContent.hash);
      this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
        ...entry,
        serverRevision: remoteRev,
        serverHash: remoteHash,
        localHash: localContent.hash,
        pending: { id: op, createdAt: this.deps.now() },
      });
      await this.deps.baseline.save();
      try {
        const result = await this.deps.api.collabUpload(accepted.vaultId, accepted.fileId, {
          content,
          baseRevision: remoteRev,
          operationID: op,
        });
        const encoded = new TextEncoder().encode(content);
        const h = await this.deps.vault.hashBytes(encoded);
        this.deps.baseline.setCollaboration(accepted.vaultId, accepted.fileId, {
          vaultId: accepted.vaultId,
          fileId: accepted.fileId,
          localPath: accepted.localPath,
          serverRevision: result.revision,
          serverHash: result.hash,
          localHash: h,
          baseText: content,
          pending: null,
          conflict: null,
        });
        await this.deps.baseline.save();
        // 若是 merged 需先写入本地
        if (decision.kind === "upload_merged") {
          await this.deps.vault.writeCanonical(accepted.localPath, encoded);
        }
        new Notice(this.deps.plugin.t("collab.uploaded", { path: accepted.localPath }));
        this.deps.onChange();
      } catch (error) {
        if (isCollaborationConflictError(error)) {
          const casApi = {
            downloadCollabContent: download,
            collabUpload: this.deps.api.collabUpload,
          };
          const pendingEntry = this.deps.baseline.getCollaboration(accepted.vaultId, accepted.fileId) ?? entry;
          await resolveCollaboration409(
            { baseline: this.deps.baseline, vault: this.deps.vault, api: casApi, plugin: this.deps.plugin, now: this.deps.now, createOperationID: this.deps.createOperationID, onChange: this.deps.onChange },
            { accepted, entry: pendingEntry },
          );
          return;
        }
        const message = localizeError(error, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError"));
        new Notice(this.deps.plugin.t("collab.uploadFailed", { error: message }));
      }
      return;
    }
  }
}
