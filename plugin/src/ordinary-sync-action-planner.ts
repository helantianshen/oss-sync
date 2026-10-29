import { classifySyncPath } from "./blacklist.js";
import type { BaselineEntry, PendingOperation } from "./baseline.js";
import type { SyncFileMeta } from "./api.js";
import type { ExpectedLocalState, OrdinarySyncAction, OrdinarySyncLocalMeta, OrdinarySyncPlannerInput, OrdinarySyncPlanResult } from "./ordinary-sync-actions.js";

/** 候选路径输入；与规划器输入同源，但不需要本地元信息 */
export type OrdinarySyncCandidatePathsInput = Pick<
  OrdinarySyncPlannerInput,
  "forceFull" | "remote" | "baseline" | "pending" | "conflicts" | "vaultPaths"
>;

function remoteFromBaseline(path: string, baseline: BaselineEntry, configDir: string): SyncFileMeta {
  return { path, type: classifySyncPath(path, configDir), hash: baseline.serverHash, size: baseline.localSize, mtime: baseline.localMTime, revision: baseline.serverRevision, deleted: baseline.serverDeleted };
}
function compactedDelete(path: string, configDir: string): SyncFileMeta {
  return { path, type: classifySyncPath(path, configDir), hash: "", size: 0, mtime: 0, revision: 0, deleted: true };
}
function expected(local: OrdinarySyncLocalMeta | null): ExpectedLocalState {
  if (local === null) return { kind: "absent" };
  return { kind: "hash", hash: local.hash };
}

/** 忽略冲突与重命名路径，完整同步时纳入本地和基线路径 */
export function ordinarySyncCandidatePaths(
  input: OrdinarySyncCandidatePathsInput,
): readonly string[] {
  const renamePaths = new Set<string>();
  const paths = new Set<string>();

  for (const operation of input.pending) {
    if (operation.kind === "rename") {
      // 重命名路径单独收集，整条链路都不参与普通动作规划
      renamePaths.add(operation.path);
      if (operation.oldPath) renamePaths.add(operation.oldPath);
    } else {
      paths.add(operation.path);
    }
  }
  for (const path of input.remote.keys()) paths.add(path);
  if (input.forceFull) {
    // 增量同步的 remote 只包含变更路径，本地与基线的全量路径仅在完整快照时纳入
    for (const path of input.baseline.keys()) paths.add(path);
    for (const path of input.vaultPaths) paths.add(path);
  }

  return [...paths].filter(
    (path) => !renamePaths.has(path) && !input.conflicts.has(path),
  );
}

/** 根据远端、基线和待处理操作生成普通同步动作 */
export function planOrdinarySyncActions(input: OrdinarySyncPlannerInput): OrdinarySyncPlanResult {
  const { forceFull, recoverySnapshot, remote, baseline, pending, localByPath, conflicts, vaultPaths, createOperationId } = input;
  const pendingByPath = new Map<string, PendingOperation>();
  for (const op of pending) {
    if (op.kind !== "rename") pendingByPath.set(op.path, op);
  }
  const actions: OrdinarySyncAction[] = [];
  const obsoleteSet = new Set<string>();
  const removedSet = new Set<string>();
  for (const path of ordinarySyncCandidatePaths({
    forceFull,
    remote,
    baseline,
    pending,
    conflicts,
    vaultPaths,
  })) {
    const base = baseline.get(path) ?? null;
    const local = localByPath.get(path) ?? null;
    const submitted = remote.get(path);
    // 增量同步下 remote 不含未变更路径，用基线还原一份服务端视图；完整快照时不能补，否则会把已删除的远端当作存在
    const server = submitted ?? (!forceFull && base !== null ? remoteFromBaseline(path, base, input.configDir) : undefined);
    const operation = pendingByPath.get(path);
    if (base === null) {
      if (operation?.kind === "delete") {
        if (server !== undefined && !server.deleted) {
          actions.push({ kind: "delete_remote", path, baseRevision: server.revision, operationID: operation.id, operation });
        } else if (server?.deleted === true && local === null) {
          actions.push({ kind: "adopt", path, local: null, remote: server, expectedLocal: expected(local) });
        } else if (server?.deleted === true) {
          actions.push({ kind: "conflict", path, local, remote: server, expectedLocal: expected(local) });
        } else obsoleteSet.add(operation.id);
      } else if (local !== null && server === undefined) {
        actions.push({ kind: "upload", path, local, baseRevision: 0, operationID: operation?.id ?? createOperationId(), operation });
      } else if (local === null && server !== undefined && !server.deleted) {
        actions.push({ kind: "download", path, remote: server, expectedLocal: expected(local) });
      } else if (local !== null && server !== undefined && !server.deleted && local.hash === server.hash) {
        actions.push({ kind: "adopt", path, local, remote: server, expectedLocal: expected(local) });
      } else if (local !== null && server !== undefined && !server.deleted) {
        actions.push({ kind: "reconcile", path, local, remote: server, expectedLocal: expected(local) });
      } else if (local !== null && server !== undefined) {
        actions.push({ kind: "conflict", path, local, remote: server, expectedLocal: expected(local) });
      } else if (local === null && server?.deleted === true) {
        actions.push({ kind: "adopt", path, local: null, remote: server, expectedLocal: expected(local) });
      } else if (operation !== undefined) obsoleteSet.add(operation.id);
      continue;
    }
    if (recoverySnapshot && submitted === undefined) {
      // 服务端历史已压缩，未出现在快照中的路径无法判断远端是否仍存在
      if (base.serverDeleted) {
        removedSet.add(path);
        if (local !== null) actions.push({ kind: "upload", path, local, baseRevision: 0, operationID: operation?.id ?? createOperationId(), operation });
        else if (operation !== undefined) obsoleteSet.add(operation.id);
      } else if (local === null) {
        removedSet.add(path);
        if (operation !== undefined) obsoleteSet.add(operation.id);
      } else if (local.hash === base.serverHash) {
        actions.push({ kind: "delete_local_absent", path, expectedLocal: expected(local) });
      } else {
        actions.push({ kind: "conflict", path, local, remote: compactedDelete(path, input.configDir), expectedLocal: expected(local) });
      }
      continue;
    }
    // 删除态下本地只要存在即为变化；未变更判定同时比对 revision、hash 与删除标记
    const localChanged = base.serverDeleted ? local !== null : local === null || local.hash !== base.serverHash;
    // remote 缺失时仅在完整快照下视为远端变化，增量同步下缺失表示未变更
    const remoteChanged = server !== undefined ? server.revision !== base.serverRevision || server.hash !== base.serverHash || server.deleted !== base.serverDeleted : forceFull;
    if (!localChanged && !remoteChanged) {
      if (operation !== undefined) obsoleteSet.add(operation.id);
      continue;
    }
    if (localChanged && !remoteChanged) {
      if (local !== null) actions.push({ kind: "upload", path, local, baseRevision: base.serverRevision, operationID: operation?.id ?? createOperationId(), operation });
      else actions.push({ kind: "delete_remote", path, baseRevision: base.serverRevision, operationID: operation?.id ?? createOperationId(), operation });
      continue;
    }
    if (!localChanged && remoteChanged && server !== undefined) {
      if (server.deleted) actions.push({ kind: "delete_local", path, remote: server, expectedLocal: expected(local) });
      else actions.push({ kind: "download", path, remote: server, expectedLocal: expected(local) });
      continue;
    }
    if (local !== null && server !== undefined && !server.deleted && local.hash === server.hash) {
      actions.push({ kind: "adopt", path, local, remote: server, expectedLocal: expected(local) });
    } else if (local !== null && server !== undefined && !server.deleted) {
      actions.push({ kind: "reconcile", path, local, remote: server, expectedLocal: expected(local) });
    } else if (server !== undefined) {
      actions.push({ kind: "conflict", path, local, remote: server, expectedLocal: expected(local) });
    } else if (operation !== undefined) obsoleteSet.add(operation.id);
  }
  return { actions, obsoletePendingIds: [...obsoleteSet], removedBaselinePaths: [...removedSet] };
}
