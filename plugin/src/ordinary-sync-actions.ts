import type { BaselineEntry, PendingOperation } from "./baseline.js";
import type { SyncFileMeta } from "./api.js";
import type { OrdinarySyncConflict409 } from "./ordinary-sync-conflict-409.js";

/** 本地写入前预期的状态；hash 为内容校验，absent 要求文件不存在 */
export type ExpectedLocalState =
  | { readonly kind: "absent" }
  | { readonly kind: "hash"; readonly hash: string };

/** 动作执行结果；deferred_retry 表示本轮放弃，留给下一轮同步重试 */
export type OrdinarySyncActionOutcome =
  | { readonly kind: "resolved" }
  | { readonly kind: "conflicted" }
  | {
      readonly kind: "deferred_retry";
      readonly reason: "stale_local" | "second_conflict" | "transport";
      readonly cause?: Error | OrdinarySyncConflict409;
    };

/** 本地文件元信息，来自读取时刻的快照 */
export type OrdinarySyncLocalMeta = {
  readonly path: string;
  readonly hash: string;
  readonly mtime: number;
  readonly size: number;
};

/** 普通同步可执行的动作；upload 与 delete_remote 由本地发起，其余为拉取远端结果 */
export type OrdinarySyncAction =
  | {
      readonly kind: "upload";
      readonly path: string;
      readonly local: OrdinarySyncLocalMeta;
      readonly baseRevision: number;
      readonly operationID: string;
      readonly operation?: PendingOperation;
    }
  | {
      readonly kind: "delete_remote";
      readonly path: string;
      readonly baseRevision: number;
      readonly operationID: string;
      readonly operation?: PendingOperation;
    }
  | {
      readonly kind: "download";
      readonly path: string;
      readonly remote: SyncFileMeta;
      readonly expectedLocal: ExpectedLocalState;
    }
  | {
      readonly kind: "delete_local";
      readonly path: string;
      readonly remote: SyncFileMeta;
      readonly expectedLocal: ExpectedLocalState;
    }
  | {
      readonly kind: "delete_local_absent";
      readonly path: string;
      readonly expectedLocal: ExpectedLocalState;
    }
  | {
      readonly kind: "adopt";
      readonly path: string;
      readonly local: OrdinarySyncLocalMeta | null;
      readonly remote: SyncFileMeta;
      readonly expectedLocal: ExpectedLocalState;
    }
  | {
      readonly kind: "conflict";
      readonly path: string;
      readonly local: OrdinarySyncLocalMeta | null;
      readonly remote: SyncFileMeta;
      readonly expectedLocal: ExpectedLocalState;
    }
  | {
      readonly kind: "reconcile";
      readonly path: string;
      readonly local: OrdinarySyncLocalMeta;
      readonly remote: SyncFileMeta;
      readonly expectedLocal: ExpectedLocalState;
    };

/** 计划附带清理项；执行动作前先删除失效的待传输操作与基线记录 */
export type OrdinarySyncPlanEffects = {
  readonly obsoletePendingIds: readonly string[];
  readonly removedBaselinePaths: readonly string[];
};

/** 普通同步计划 */
export type OrdinarySyncPlanResult = {
  readonly actions: readonly OrdinarySyncAction[];
  readonly obsoletePendingIds: readonly string[];
  readonly removedBaselinePaths: readonly string[];
};

/** 规划器输入；remote 仅在完整快照或本地有基线时可信，故以 forceFull 区分 */
export type OrdinarySyncPlannerInput = {
  readonly configDir: string;
  readonly forceFull: boolean;
  readonly recoverySnapshot: boolean;
  readonly remote: ReadonlyMap<string, SyncFileMeta>;
  readonly baseline: ReadonlyMap<string, BaselineEntry>;
  readonly pending: readonly PendingOperation[];
  readonly localByPath: ReadonlyMap<string, OrdinarySyncLocalMeta | null>;
  readonly conflicts: ReadonlySet<string>;
  readonly vaultPaths: readonly string[];
  readonly createOperationId: () => string;
};

/** 穷尽检查；新增动作类型未处理时应在此抛错而不是静默忽略 */
export function assertNeverOrdinarySyncAction(
  value: never,
): never {
  throw new Error(`unhandled OrdinarySyncAction ${String(value)}`);
}
