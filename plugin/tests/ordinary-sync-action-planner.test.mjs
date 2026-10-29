import assert from "node:assert/strict";
import test from "node:test";
import { loadSource } from "./helpers/source-loader.mjs";

function base(hash = "base-hash") {
  return {
    serverRevision: 7, serverHash: hash, serverDeleted: false,
    localHash: hash, localMTime: 100, localSize: 12, baseText: "祖先\n",
  };
}
function local(path, hash = "base-hash") {
  return { path, hash, mtime: 200, size: 20 };
}
function remote(path, extra = {}) {
  return { path, type: "markdown", hash: "remote-hash", size: 30, mtime: 300, revision: 8, deleted: false, ...extra };
}
function input(extra = {}) {
  return {
    configDir: ".obsidian", forceFull: false, recoverySnapshot: false,
    remote: new Map(), baseline: new Map(), pending: [], localByPath: new Map(),
    conflicts: new Set(), vaultPaths: [],
    createOperationId: () => { throw new Error("不能为已有待办分配新操作 ID"); },
    ...extra,
  };
}

test("远端增量缺失不代表删除，本地待办使用原基线版本上传且保留幂等 ID", async (t) => {
  const { planOrdinarySyncActions } = await loadSource(t, "ordinary-sync-action-planner");
  const upload = { id: "persisted-upload", kind: "upsert", path: "edited.md", createdAt: 123 };
  const edited = local("edited.md", "local-edit");
  const incoming = remote("incoming.md");
  const result = planOrdinarySyncActions(input({
    baseline: new Map([["edited.md", base()], ["unchanged.md", base()]]),
    localByPath: new Map([["edited.md", edited], ["unchanged.md", local("unchanged.md")]]),
    vaultPaths: ["edited.md", "unchanged.md"],
    remote: new Map([["incoming.md", incoming]]),
    pending: [upload],
  }));
  assert.deepEqual(result, {
    actions: [
      { kind: "upload", path: "edited.md", local: edited, baseRevision: 7, operationID: "persisted-upload", operation: upload },
      { kind: "download", path: "incoming.md", remote: incoming, expectedLocal: { kind: "absent" } },
    ],
    obsoletePendingIds: [],
    removedBaselinePaths: [],
  });
});

test("远端删除只能删除未改动的本地文件，本地新修改必须进入冲突而非清理待办", async (t) => {
  const { planOrdinarySyncActions } = await loadSource(t, "ordinary-sync-action-planner");
  const edited = local("edited.md", "unsent-local-edit");
  const unchanged = local("unchanged.md");
  const editedDelete = remote("edited.md", { deleted: true, hash: "", size: 0 });
  const unchangedDelete = remote("unchanged.md", { deleted: true, hash: "", size: 0 });
  const pending = { id: "local-edit", kind: "upsert", path: "edited.md", createdAt: 123 };
  const result = planOrdinarySyncActions(input({
    baseline: new Map([["edited.md", base()], ["unchanged.md", base()]]),
    localByPath: new Map([["edited.md", edited], ["unchanged.md", unchanged]]),
    remote: new Map([["edited.md", editedDelete], ["unchanged.md", unchangedDelete]]),
    pending: [pending],
  }));
  assert.deepEqual(result, {
    actions: [
      { kind: "conflict", path: "edited.md", local: edited, remote: editedDelete, expectedLocal: { kind: "hash", hash: "unsent-local-edit" } },
      { kind: "delete_local", path: "unchanged.md", remote: unchangedDelete, expectedLocal: { kind: "hash", hash: "base-hash" } },
    ],
    obsoletePendingIds: [],
    removedBaselinePaths: [],
  });
});

test("完整扫描仍排除移动的两端和已有冲突，不下载覆盖或丢弃它们的待办", async (t) => {
  const { planOrdinarySyncActions } = await loadSource(t, "ordinary-sync-action-planner");
  const safe = local("safe.md", "safe-edit");
  const safePending = { id: "safe-upload", kind: "upsert", path: "safe.md", createdAt: 123 };
  const pending = [
    { id: "move", kind: "rename", path: "new.md", oldPath: "old.md", createdAt: 123 },
    { id: "conflict-edit", kind: "upsert", path: "conflict.md", createdAt: 123 },
    safePending,
  ];
  const result = planOrdinarySyncActions(input({
    forceFull: true,
    baseline: new Map([["old.md", base()], ["conflict.md", base()], ["safe.md", base()]]),
    localByPath: new Map([["new.md", local("new.md", "moved-edit")], ["conflict.md", local("conflict.md", "unresolved-edit")], ["safe.md", safe]]),
    remote: new Map([
      ["old.md", remote("old.md")], ["new.md", remote("new.md")],
      ["conflict.md", remote("conflict.md")], ["safe.md", remote("safe.md", { hash: "base-hash", revision: 7 })],
    ]),
    conflicts: new Set(["conflict.md"]),
    vaultPaths: ["new.md", "conflict.md", "safe.md"],
    pending,
  }));
  assert.deepEqual(result, {
    actions: [{ kind: "upload", path: "safe.md", local: safe, baseRevision: 7, operationID: "safe-upload", operation: safePending }],
    obsoletePendingIds: [],
    removedBaselinePaths: [],
  });
});
