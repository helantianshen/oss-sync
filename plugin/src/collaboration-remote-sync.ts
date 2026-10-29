import { Notice } from "obsidian";
import type { CollaborationBaselineEntry } from "./collaboration-state.js";
import { CollaborationFileVault } from "./collaboration-file-vault.js";
import type { CollaborationUploadInput, SyncFileMeta } from "./api.js";
import { decodeMergeableText } from "./text-merge.js";
import { decideCollaborationReconciliation, type CollaborationContent } from "./collaboration-file-reconcile.js";
import { collabConflictCopyPath, isCollaborationConflictError, resolveCollaboration409 } from "./collaboration-cas-resolver.js";
import { localizeError } from "./localized-error.js";
import type { TranslationKey, TranslationParams } from "./i18n.js";
import type { AcceptedCollaborationFile } from "./collaboration-file-sync.js";
import { CollaborationSyncCoordinator } from "./collaboration-sync-coordinator.js";
import { isValidOperationID, isValidServerRevision } from "./operation-id.js";
import { prepareCollaborationUpload } from "./collaboration-upload-helpers.js";

export { CollaborationSyncCoordinator };
export type { AcceptedCollaborationFile };

/** 远端同步所需的协作 API 能力 */
export interface CollaborationRemoteSyncApi {
  readonly downloadCollabContent: (v: string, f: number) => Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null>;
  readonly collabUpload: (v: string, f: number, i: CollaborationUploadInput) => Promise<SyncFileMeta>;
}
/** 翻译能力 */
export interface CollaborationRemoteSyncPlugin {
  readonly t: (k: TranslationKey, p?: TranslationParams) => string;
}
/** 远端同步所需的基线读写能力；removeCollaboration 与 load 可选 */
export interface CollaborationRemoteSyncBaseline {
  readonly getCollaboration: (v: string, f: number) => CollaborationBaselineEntry | null;
  readonly setCollaboration: (v: string, f: number, e: CollaborationBaselineEntry) => void;
  readonly removeCollaboration?: (v: string, f: number) => boolean;
  readonly save: () => Promise<void>;
  readonly load: () => Promise<void>;
  readonly bindCollaborationAccount: (a: string) => boolean;
}
/** 远端同步依赖；coordinator 串行化所有刷新，noticeGuard 抑制周期性失败的重复提示 */
export interface CollaborationRemoteSyncDeps {
  readonly baseline: CollaborationRemoteSyncBaseline;
  readonly vault: CollaborationFileVault;
  readonly api: CollaborationRemoteSyncApi;
  readonly plugin: CollaborationRemoteSyncPlugin;
  readonly getAccepted: () => readonly AcceptedCollaborationFile[];
  readonly getUsername: () => string;
  readonly now: () => number;
  readonly createOperationID: () => string;
  readonly onChange: () => void;
  readonly coordinator: CollaborationSyncCoordinator;
  readonly onConflict?: () => Promise<void>;
  /** 通知冷却抑制；缺省时每次失败都提示 */
  readonly noticeGuard?: { shouldShow: (key: string) => boolean; reset: (key?: string) => void };
}
/** 远端正文快照 */
interface RemoteVersion {
  readonly bytes: Uint8Array;
  readonly hash: string;
  readonly revision: number;
}
/** 对账上下文；local 为 null 表示本地文件不存在 */
interface ReconcileContext {
  readonly remote: RemoteVersion;
  readonly local: CollaborationContent | null;
}
/** 穷尽检查；新增决策类型未处理时应抛错 */
function assertNever(value: never): never {
  throw new Error(`unhandled ${String(value)}`);
}
/** 拉取远端并对账每个已接受协作文件；串行执行由 coordinator 保证 */
export class CollaborationRemoteSync {
  constructor(private readonly deps: CollaborationRemoteSyncDeps) {}
  /** 追加到串行链尾执行刷新 */
  refresh(): Promise<void> {
    return this.deps.coordinator.run(() => this.doRefresh());
  }
  private async doRefresh(): Promise<void> {
    await this.deps.baseline.load();
    const didBind = this.deps.baseline.bindCollaborationAccount(this.deps.getUsername());
    if (didBind) await this.deps.baseline.save();
    for (const ac of this.deps.getAccepted()) {
      const b = this.deps.baseline.getCollaboration(ac.vaultId, ac.fileId);
      if (b?.pending) {
        await this.retryPending(ac, b);
        continue;
      }
      if (b?.conflict) continue;
      await this.syncOne(ac, b);
    }
  }
  /** 重试上次未完成的上传；本地已变时改用新哈希与新 operationID */
  private async retryPending(ac: AcceptedCollaborationFile, entry: CollaborationBaselineEntry): Promise<void> {
    if (!isValidServerRevision(entry.serverRevision)) {
      // 哨兵 -1：清掉错误 pending，以无基线方式重新拉取，避免 keep_local 阻塞
      if (this.deps.baseline.removeCollaboration) {
        this.deps.baseline.removeCollaboration(ac.vaultId, ac.fileId);
        await this.deps.baseline.save();
      } else {
        this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, { ...entry, serverRevision: -1, pending: null, conflict: null });
        await this.deps.baseline.save();
      }
      await this.syncOne(ac, null);
      return;
    }
    const local = await this.deps.vault.readExact(ac.localPath);
    if (!local) return;
    const same = local.hash === entry.localHash;
    let op = same && entry.pending && isValidOperationID(entry.pending.id) ? entry.pending.id : this.deps.createOperationID();
    if (!isValidOperationID(op)) op = this.deps.createOperationID();
    const pendingEntry: CollaborationBaselineEntry = same ? { ...entry, pending: { id: op, createdAt: this.deps.now() } } : { ...entry, localHash: local.hash, pending: { id: op, createdAt: this.deps.now() } };
    if (!same) {
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, pendingEntry);
      await this.deps.baseline.save();
    } else if (!entry.pending || !isValidOperationID(entry.pending.id)) {
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, { ...entry, pending: { id: op, createdAt: this.deps.now() } });
      await this.deps.baseline.save();
    }
    const decoded = decodeMergeableText(ac.localPath, local.bytes);
    if (decoded === null) return;
    const prepared = prepareCollaborationUpload(pendingEntry, decoded, () => op);
    if ("error" in prepared) return;
    try {
      const r = await this.deps.api.collabUpload(ac.vaultId, ac.fileId, prepared.input);
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
        vaultId: ac.vaultId, fileId: ac.fileId, localPath: ac.localPath,
        serverRevision: r.revision, serverHash: r.hash, localHash: local.hash, baseText: decoded, pending: null, conflict: null,
      });
      await this.deps.baseline.save();
      new Notice(this.deps.plugin.t("collab.uploaded", { path: ac.localPath }));
      this.deps.onChange();
    } catch (err) {
      if (isCollaborationConflictError(err)) {
        const toResolve: CollaborationBaselineEntry = this.deps.baseline.getCollaboration(ac.vaultId, ac.fileId) ?? { ...pendingEntry, pending: { id: op, createdAt: this.deps.now() } };
        await resolveCollaboration409(
          { baseline: this.deps.baseline, vault: this.deps.vault, api: this.deps.api, plugin: this.deps.plugin, now: this.deps.now, createOperationID: this.deps.createOperationID, onChange: this.deps.onChange },
          { accepted: ac, entry: toResolve },
        );
        return;
      }
      new Notice(this.deps.plugin.t("collab.uploadFailed", { error: localizeError(err, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError")) }));
    }
  }
  /** 拉取单个文件的远端正文并按决策执行 */
  private async syncOne(ac: AcceptedCollaborationFile, entry: CollaborationBaselineEntry | null): Promise<void> {
    let fetched: { content: ArrayBuffer; meta: SyncFileMeta } | null;
    try {
      fetched = await this.deps.api.downloadCollabContent(ac.vaultId, ac.fileId);
    } catch (err) {
      // 刷新由轮询驱动，冷期内重复失败只提示一次；状态栏与侧边栏持续展示
      const guard = this.deps.noticeGuard;
      if (!guard || guard.shouldShow("collab.refresh")) {
        new Notice(this.deps.plugin.t("collab.refreshFailed", { error: localizeError(err, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError")) }));
      }
      return;
    }
    if (!fetched) return;
    const remoteBytes = new Uint8Array(fetched.content);
    const remoteHash = fetched.meta.hash;
    const remoteRev = fetched.meta.revision;
    const remote: CollaborationContent = { path: ac.localPath, bytes: remoteBytes, hash: remoteHash };
    const remoteVersion = { bytes: remoteBytes, hash: remoteHash, revision: remoteRev };
    if (!entry || !isValidServerRevision(entry.serverRevision)) {
      // 无基线或哨兵 revision 时删除本地记录，按首次同步重新建立
      if (entry && !isValidServerRevision(entry.serverRevision) && this.deps.baseline.removeCollaboration) {
        this.deps.baseline.removeCollaboration(ac.vaultId, ac.fileId);
        await this.deps.baseline.save();
      }
      await this.handleFirst(ac, remoteVersion);
      return;
    }
    const local = await this.deps.vault.readExact(ac.localPath);
    const localContent: CollaborationContent | null = local ? { path: ac.localPath, bytes: local.bytes, hash: local.hash } : null;
    const decision = decideCollaborationReconciliation({ path: ac.localPath, baseline: entry, ancestorText: entry.baseText, local: localContent, remote });
    switch (decision.kind) {
      case "adopt_remote":
        await this.handleAdopt(ac, entry, { remote: remoteVersion, local: localContent });
        break;
      case "apply_remote":
        await this.handleApply(ac, { remote: remoteVersion, local: localContent });
        break;
      case "upload_local":
        if (localContent) await this.handleUploadLocal(ac, entry, { local: localContent, content: decision.content });
        break;
      case "upload_merged":
        if (localContent) await this.handleUploadLocal(ac, entry, { local: localContent, content: decision.content });
        break;
      case "persist_text_conflict": {
        const latestHash = localContent ? localContent.hash : entry.localHash;
        this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
          vaultId: entry.vaultId, fileId: entry.fileId, localPath: entry.localPath,
          serverRevision: entry.serverRevision, serverHash: entry.serverHash, localHash: latestHash, baseText: entry.baseText,
          pending: null, conflict: { remoteRevision: remoteVersion.revision, remoteHash: remoteVersion.hash, remoteText: decision.remoteText, detectedAt: this.deps.now() },
        });
        await this.deps.baseline.save();
        this.deps.onChange();
        new Notice(`协作冲突：${ac.localPath} 存在重叠编辑，已在右侧边栏等待解决`, 8000);
        await this.showConflictPopup();
        break;
      }
      case "preserve_both": {
        if (!localContent) {
          await this.persistDownload(ac, remoteVersion, remoteVersion.hash);
          break;
        }
        const stashFresh = await this.deps.vault.readExact(ac.localPath);
        if (!stashFresh || stashFresh.hash !== localContent.hash) break;
        const sibling = collabConflictCopyPath(ac.localPath, this.deps.now());
        await this.deps.vault.writeCanonical(sibling, localContent.bytes);
        await this.persistDownload(ac, remoteVersion, remoteVersion.hash);
        break;
      }
      case "keep_local":
        break;
      default:
        assertNever(decision);
    }
  }
  /** 首次同步：本地不存在时直接落地远端，内容一致时仅建立基线 */
  private async handleFirst(ac: AcceptedCollaborationFile, remote: RemoteVersion): Promise<void> {
    const local = await this.deps.vault.readExact(ac.localPath);
    if (!local) {
      await this.persistDownload(ac, remote, remote.hash);
      return;
    }
    if (local.hash === remote.hash) {
      const baseText = decodeMergeableText(ac.localPath, remote.bytes) ?? "";
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
        vaultId: ac.vaultId, fileId: ac.fileId, localPath: ac.localPath,
        serverRevision: remote.revision, serverHash: remote.hash, localHash: local.hash, baseText, pending: null, conflict: null,
      });
      await this.deps.baseline.save();
    }
  }
  /** 采纳远端：本地缺失或与远端一致时只更新基线，否则落地远端正文 */
  private async handleAdopt(ac: AcceptedCollaborationFile, entry: CollaborationBaselineEntry, ctx: ReconcileContext): Promise<void> {
    const { remote, local } = ctx;
    if (!local) {
      await this.persistDownload(ac, remote, remote.hash);
      return;
    }
    if (local.hash === remote.hash) {
      const baseText = decodeMergeableText(ac.localPath, remote.bytes) ?? entry.baseText;
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
        vaultId: entry.vaultId, fileId: entry.fileId, localPath: entry.localPath,
        serverRevision: remote.revision, serverHash: remote.hash, localHash: local.hash, baseText, pending: null, conflict: null,
      });
      await this.deps.baseline.save();
      return;
    }
    const fresh = await this.deps.vault.readExact(ac.localPath);
    if (!fresh || fresh.hash !== local.hash) return;
    await this.persistDownload(ac, remote, remote.hash);
  }
  /** 应用远端：写入前再次读取确认本地未被改动，避免覆盖并发编辑 */
  private async handleApply(ac: AcceptedCollaborationFile, ctx: ReconcileContext): Promise<void> {
    const { remote, local } = ctx;
    if (!local) return;
    const fresh = await this.deps.vault.readExact(ac.localPath);
    if (!fresh || fresh.hash !== local.hash) return;
    await this.persistDownload(ac, remote, remote.hash);
  }
  /** 上传本地或合并后的正文；先落 pending 再上传，崩溃后仍可重试 */
  private async handleUploadLocal(ac: AcceptedCollaborationFile, entry: CollaborationBaselineEntry, ctx: { local: CollaborationContent; content: string }): Promise<void> {
    if (!isValidServerRevision(entry.serverRevision)) {
      if (this.deps.baseline.removeCollaboration) {
        this.deps.baseline.removeCollaboration(ac.vaultId, ac.fileId);
        await this.deps.baseline.save();
      }
      await this.syncOne(ac, null);
      return;
    }
    const { local, content } = ctx;
    let op = entry.pending && entry.localHash === local.hash && isValidOperationID(entry.pending.id) ? entry.pending.id : this.deps.createOperationID();
    if (!isValidOperationID(op)) op = this.deps.createOperationID();
    const pendingEntry: CollaborationBaselineEntry = { ...entry, localHash: local.hash, pending: { id: op, createdAt: this.deps.now() } };
    this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, pendingEntry);
    await this.deps.baseline.save();
    const prepared = prepareCollaborationUpload(pendingEntry, content, () => op);
    if ("error" in prepared) return;
    try {
      const r = await this.deps.api.collabUpload(ac.vaultId, ac.fileId, prepared.input);
      this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
        vaultId: ac.vaultId, fileId: ac.fileId, localPath: ac.localPath,
        serverRevision: r.revision, serverHash: r.hash, localHash: local.hash, baseText: content, pending: null, conflict: null,
      });
      await this.deps.baseline.save();
      new Notice(this.deps.plugin.t("collab.uploaded", { path: ac.localPath }));
      this.deps.onChange();
    } catch (err) {
      if (isCollaborationConflictError(err)) {
        await resolveCollaboration409(
          { baseline: this.deps.baseline, vault: this.deps.vault, api: this.deps.api, plugin: this.deps.plugin, now: this.deps.now, createOperationID: this.deps.createOperationID, onChange: this.deps.onChange },
          { accepted: ac, entry: pendingEntry },
        );
        return;
      }
      new Notice(this.deps.plugin.t("collab.uploadFailed", { error: localizeError(err, this.deps.plugin.t.bind(this.deps.plugin), this.deps.plugin.t("common.unknownError")) }));
    }
  }
  /** 落地远端正文并更新基线；localHash 记录写入后的本地哈希 */
  private async persistDownload(ac: AcceptedCollaborationFile, remote: RemoteVersion, localHash: string): Promise<void> {
    await this.deps.vault.writeCanonical(ac.localPath, remote.bytes);
    const baseText = decodeMergeableText(ac.localPath, remote.bytes) ?? "";
    this.deps.baseline.setCollaboration(ac.vaultId, ac.fileId, {
      vaultId: ac.vaultId, fileId: ac.fileId, localPath: ac.localPath,
      serverRevision: remote.revision, serverHash: remote.hash, localHash, baseText, pending: null, conflict: null,
    });
    await this.deps.baseline.save();
    new Notice(this.deps.plugin.t("collab.downloaded", { path: ac.localPath }));
    this.deps.onChange();
  }

  /** 冲突已持久化，侧边栏打开失败不影响同步结果 */
  private async showConflictPopup(): Promise<void> {
    try {
      await this.deps.onConflict?.();
    } catch {
      // 用户仍可通过侧边栏命令查看已保存的冲突
    }
  }
}
