import assert from "node:assert/strict";
import { access, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { loadSource } from "./helpers/source-loader.mjs";

const baseline = {
  serverRevision: 7,
  serverHash: "ancestor-hash",
  serverDeleted: false,
  localHash: "ancestor-hash",
  localMTime: 100,
  localSize: 12,
  baseText: "共同祖先\n",
};
const conflict = {
  path: "notes/conflict.md",
  localHash: "local-edit",
  remoteRevision: 9,
  remoteHash: "remote-edit",
  remoteDeleted: false,
  remoteMTime: 200,
  remoteSize: 18,
  remoteType: "markdown",
  detectedAt: 300,
};
const collaboration = {
  vaultId: "shared-vault",
  fileId: 11,
  localPath: "shared/note.md",
  serverRevision: 4,
  serverHash: "shared-base",
  localHash: "shared-edit",
  baseText: "协作祖先\n",
  pending: { id: "shared-pending", createdAt: 101 },
  conflict: { remoteRevision: 5, remoteHash: "shared-remote", remoteText: "不能覆盖的远端正文\n", detectedAt: 102 },
};

function operation(id, kind, path, extra = {}) {
  return { id, kind, path, createdAt: 123, ...extra };
}

/** 每次重新打开均创建新 Store，adapter 只提供真实临时磁盘读写 */
async function diskStore(t, persist = writeFile) {
  const { BaselineStore } = await loadSource(t, "baseline");
  const dir = await mkdtemp(join(tmpdir(), "oss-baseline-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const vault = {
    adapter: {
      async exists(path) {
        try {
          await access(join(dir, path));
          return true;
        } catch (error) {
          if (error.code === "ENOENT") return false;
          throw error;
        }
      },
      read: (path) => readFile(join(dir, path), "utf8"),
      write: (path, text) => persist(join(dir, path), text, "utf8"),
    },
  };
  return async () => {
    const store = new BaselineStore(vault);
    await store.load();
    return store;
  };
}

test("重启保留基线、待办与最新冲突，确认一个待办不丢失其余状态", async (t) => {
  const open = await diskStore(t);
  let store = await open();
  store.bindVault("personal");
  store.set("notes/conflict.md", baseline);
  store.setCursor(17);
  store.setCursor(8);
  const upload = operation("upload", "upsert", "notes/new.md");
  const deletion = operation("delete", "delete", "notes/old.md");
  store.putPending(upload);
  store.putPending(deletion);
  store.putConflict({ ...conflict, remoteRevision: 8, remoteHash: "stale-remote" });
  store.putConflict(conflict);
  await store.save();

  store = await open();
  assert.equal(store.getVaultID(), "personal");
  assert.equal(store.getCursor(), 17);
  assert.deepEqual(store.get("notes/conflict.md"), baseline);
  assert.deepEqual(store.pending(), [upload, deletion]);
  assert.deepEqual(store.conflicts(), [conflict]);
  store.removePending(upload.id);
  await store.save();

  store = await open();
  assert.deepEqual(store.pending(), [deletion]);
  assert.deepEqual(store.getConflict(conflict.path), conflict);
  store.removeConflict(conflict.path);
  await store.save();
  store = await open();
  assert.deepEqual(store.pending(), [deletion]);
  assert.equal(store.getConflict(conflict.path), null);
});

test("落盘失败必须报告错误，后续保存仍能持久化原有待办与新增修改", async (t) => {
  const failure = new Error("disk full");
  let writes = 0;
  const open = await diskStore(t, async (...args) => {
    if (++writes === 1) throw failure;
    await writeFile(...args);
  });
  let store = await open();
  store.bindVault("personal");
  const first = operation("first", "upsert", "first.md");
  store.putPending(first);
  await assert.rejects(store.save(), (error) => error === failure);

  const second = operation("second", "upsert", "second.md");
  store.putPending(second);
  store.setCursor(12);
  await store.save();
  store = await open();
  assert.deepEqual(store.pending(), [first, second]);
  assert.equal(store.getCursor(), 12);
  assert.equal(store.getVaultID(), "personal");
});

test("连续与重复移动折叠为最早来源到最终目标，重启后不生成重复上传", async (t) => {
  const open = await diskStore(t);
  let store = await open();
  const unrelated = operation("other", "upsert", "other.md");
  store.putPending(unrelated);
  store.putPending(operation("edit", "upsert", "a.md"));
  store.putPending(operation("move-ab", "rename", "b.md", { oldPath: "a.md" }));
  store.putPending(operation("edit-b", "upsert", "b.md"));
  await store.save();

  store = await open();
  const finalMove = operation("move-bc", "rename", "c.md", { oldPath: "b.md" });
  store.putPending(finalMove);
  store.putPending(operation("duplicate-bc", "rename", "c.md", { oldPath: "b.md" }));
  store.putPending(operation("edit-c", "upsert", "c.md"));
  await store.save();
  store = await open();
  assert.deepEqual(store.pending(), [unrelated, { ...finalMove, oldPath: "a.md" }]);
  store.removePendingForPath("a.md");
  await store.save();
  assert.deepEqual((await open()).pending(), [unrelated]);
});

test("移动后返回原路径仍上传最新正文，移动后删除仅删除原远端路径", async (t) => {
  const open = await diskStore(t);
  let store = await open();
  store.putPending(operation("out", "rename", "b.md", { oldPath: "a.md" }));
  await store.save();
  store = await open();
  store.putPending(operation("back", "rename", "a.md", { oldPath: "b.md" }));
  await store.save();
  store = await open();
  assert.deepEqual(store.pending(), [operation("back", "upsert", "a.md")]);
  store.removePending("back");
  store.putPending(operation("move", "rename", "new.md", { oldPath: "old.md" }));
  store.putPending(operation("remove", "delete", "new.md"));
  await store.save();
  assert.deepEqual((await open()).pending(), [operation("remove", "delete", "old.md")]);
});

test("扫描删除的上传依赖跨重启与移动保留，重复删除不能绕开保护", async (t) => {
  const open = await diskStore(t);
  let store = await open();
  store.putPending(operation("new", "upsert", "replacement.md"));
  store.putPending(operation("scan", "delete", "missing.md", {
    source: "scan", waitForPaths: ["replacement.md", "second.md"],
  }));
  await store.save();
  store = await open();
  store.putPending(operation("move", "rename", "renamed.md", { oldPath: "replacement.md" }));
  store.putPending(operation("repeat-delete", "delete", "missing.md"));
  await store.save();
  store = await open();
  assert.deepEqual(store.pending(), [
    operation("move", "rename", "renamed.md", { oldPath: "replacement.md" }),
    operation("repeat-delete", "delete", "missing.md", {
      source: "scan", waitForPaths: ["renamed.md", "second.md"],
    }),
  ]);
  store.putPending(operation("explicit-second-delete", "delete", "second.md"));
  await store.save();
  store = await open();
  assert.deepEqual(store.pending().find((item) => item.id === "repeat-delete"),
    operation("repeat-delete", "delete", "missing.md", { source: "scan", waitForPaths: ["renamed.md"] }));
  assert.deepEqual(store.pending().find((item) => item.id === "explicit-second-delete"),
    operation("explicit-second-delete", "delete", "second.md"));
});

test("切换仓库清除普通同步状态但保留同账户协作，切换账户不泄漏协作正文与待办", async (t) => {
  const open = await diskStore(t);
  let store = await open();
  store.bindVault("vault-a");
  store.bindCollaborationAccount("account-a");
  store.set("notes/conflict.md", baseline);
  store.setCursor(31);
  store.putPending(operation("private", "upsert", "private.md"));
  store.putConflict(conflict);
  store.setCollaboration("shared-vault", 11, collaboration);
  await store.save();

  store = await open();
  assert.equal(store.bindVault("vault-a"), false);
  assert.deepEqual(store.pending(), [operation("private", "upsert", "private.md")]);
  assert.equal(store.bindVault("vault-b"), true);
  await store.save();
  store = await open();
  assert.equal(store.getVaultID(), "vault-b");
  assert.equal(store.getCursor(), 0);
  assert.deepEqual(store.paths(), []);
  assert.deepEqual(store.pending(), []);
  assert.deepEqual(store.conflicts(), []);
  assert.deepEqual(store.getCollaboration("shared-vault", 11), collaboration);
  assert.equal(store.getCollaboration("another-shared-vault", 11), null);
  assert.equal(store.bindCollaborationAccount("account-a"), false);
  assert.deepEqual(store.getCollaboration("shared-vault", 11), collaboration);

  store.set("vault-b.md", baseline);
  const accountIndependentPending = operation("vault-b-upload", "upsert", "vault-b.md");
  store.putPending(accountIndependentPending);
  assert.equal(store.bindCollaborationAccount("account-b"), true);
  await store.save();
  store = await open();
  assert.equal(store.getCollaboration("shared-vault", 11), null);
  assert.deepEqual(store.collaborationEntries(), []);
  assert.deepEqual(store.get("vault-b.md"), baseline);
  assert.deepEqual(store.pending(), [accountIndependentPending]);
  store.bindCollaborationAccount("account-a");
  await store.save();
  store = await open();
  assert.deepEqual(store.collaborationEntries(), []);
  assert.deepEqual(store.pending(), [accountIndependentPending]);
});
