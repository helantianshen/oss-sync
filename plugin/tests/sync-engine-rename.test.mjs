import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

async function loadSyncEngine() {
  const dir = await mkdtemp(join(tmpdir(), "oss-sync-engine-"));
  const outfile = join(dir, "sync-engine.mjs");
  await build({
    entryPoints: ["src/sync-engine.ts"],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
    plugins: [
      {
        name: "obsidian-stub",
        setup(builder) {
          builder.onResolve({ filter: /^obsidian$/ }, () => ({
            path: "obsidian",
            namespace: "obsidian-stub",
          }));
          builder.onLoad({ filter: /.*/, namespace: "obsidian-stub" }, () => ({
            contents: `
              export class App {}
              export class Vault {}
              export class Notice {}
              export class TFile {
                static [Symbol.hasInstance](value) {
                  return value?.__tfile === true;
                }
              }
              export async function requestUrl() {
                throw new Error("not implemented");
              }
            `,
            loader: "js",
          }));
        },
      },
    ],
  });
  const module = await import(pathToFileURL(outfile).href);
  return {
    SyncEngine: module.SyncEngine,
    cleanup: () => rm(dir, { recursive: true, force: true }),
  };
}

test("successful rename replaces the stale manifest view for the same run", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const content = new TextEncoder().encode("renamed").buffer;
    const file = {
      __tfile: true,
      path: "Notes/B.md",
      content,
      stat: { mtime: 10, size: content.byteLength },
    };
    const vault = {
      getAbstractFileByPath(path) {
        if (path !== "Notes/B.md") return null;
        return file;
      },
      async readBinary(target) {
        return target.content;
      },
    };
    const oldBaseline = {
      serverRevision: 1,
      serverHash: "old-hash",
      serverDeleted: false,
      localHash: "old-hash",
      localMTime: 1,
      localSize: 7,
    };
    const stored = new Map([["Notes/A.md", oldBaseline]]);
    const baseline = {
      getConflict() {
        return null;
      },
      get(path) {
        return stored.get(path) ?? null;
      },
      set(path, value) {
        stored.set(path, value);
      },
      removePending() {},
      async save() {},
    };
    const renameResult = {
      old: {
        path: "Notes/A.md",
        type: "markdown",
        hash: "old-hash",
        size: 7,
        mtime: 10,
        revision: 2,
        deleted: true,
      },
      new: {
        path: "Notes/B.md",
        type: "markdown",
        hash: "old-hash",
        size: 7,
        mtime: 10,
        revision: 3,
        deleted: false,
      },
    };
    const api = {
      async renameV2() {
        return renameResult;
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, api, baseline, plugin);
    const remote = new Map([
      [
        "Notes/A.md",
        {
          path: "Notes/A.md",
          type: "markdown",
          hash: "old-hash",
          size: 7,
          mtime: 1,
          revision: 1,
          deleted: false,
        },
      ],
    ]);

    await engine.processRenames(
      "vault-a",
      [
        {
          id: "rename-a-b",
          kind: "rename",
          oldPath: "Notes/A.md",
          path: "Notes/B.md",
          createdAt: 1,
        },
      ],
      remote
    );

    assert.deepEqual(remote.get("Notes/A.md"), renameResult.old);
    assert.deepEqual(remote.get("Notes/B.md"), renameResult.new);
  } finally {
    await cleanup();
  }
});

test("an unresolved rename blocks normal actions for both paths", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const oldBaseline = {
      serverRevision: 1,
      serverHash: "source-hash",
      serverDeleted: false,
      localHash: "source-hash",
      localMTime: 1,
      localSize: 6,
    };
    const baseline = {
      getConflict() {
        return null;
      },
      get(path) {
        return path === "Notes/A.md" ? oldBaseline : null;
      },
      paths() {
        return ["Notes/A.md"];
      },
      removePending() {},
    };
    const vault = {
      getAbstractFileByPath() {
        return null;
      },
      getFiles() {
        return [];
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, {}, baseline, plugin);
    const pendingRename = {
      id: "rename-a-b",
      kind: "rename",
      oldPath: "Notes/A.md",
      path: "Notes/B.md",
      createdAt: 1,
    };
    const remote = new Map([
      [
        "Notes/A.md",
        {
          path: "Notes/A.md",
          type: "markdown",
          hash: "source-hash",
          size: 6,
          mtime: 1,
          revision: 1,
          deleted: false,
        },
      ],
      [
        "Notes/B.md",
        {
          path: "Notes/B.md",
          type: "markdown",
          hash: "target-hash",
          size: 6,
          mtime: 1,
          revision: 2,
          deleted: false,
        },
      ],
    ]);

    const actions = await engine.planActions(false, remote, [pendingRename]);
    assert.deepEqual(actions, []);
  } finally {
    await cleanup();
  }
});

test("a pending delete without local baseline forces a full server snapshot", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const baseline = {
      get() {
        return null;
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault: {} }, {}, baseline, plugin);

    assert.equal(
      engine.needsFullSnapshotForDeletes([
        {
          id: "delete-without-baseline",
          kind: "delete",
          path: "Notes/Deleted.md",
          createdAt: 1,
        },
      ]),
      true
    );
  } finally {
    await cleanup();
  }
});

test("an explicit delete without local baseline deletes the live server file", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const removedPending = [];
    const baseline = {
      get() {
        return null;
      },
      getConflict() {
        return null;
      },
      paths() {
        return [];
      },
      removePending(id) {
        removedPending.push(id);
      },
    };
    const vault = {
      getAbstractFileByPath() {
        return null;
      },
      getFiles() {
        return [];
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, {}, baseline, plugin);
    const pending = {
      id: "delete-server-file",
      kind: "delete",
      path: "Notes/Deleted.md",
      createdAt: 1,
    };
    const remote = new Map([
      [
        pending.path,
        {
          path: pending.path,
          type: "markdown",
          hash: "server-hash",
          size: 7,
          mtime: 10,
          revision: 9,
          deleted: false,
        },
      ],
    ]);

    const actions = await engine.planActions(true, remote, [pending]);

    assert.deepEqual(actions, [
      {
        kind: "delete_remote",
        path: pending.path,
        baseRevision: 9,
        operationID: pending.id,
        operation: pending,
      },
    ]);
    assert.deepEqual(removedPending, []);
  } finally {
    await cleanup();
  }
});

test("a normal incremental delete uses the acknowledged baseline revision", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const baselineEntry = {
      serverRevision: 12,
      serverHash: "server-hash",
      serverDeleted: false,
      localHash: "server-hash",
      localMTime: 10,
      localSize: 7,
    };
    const baseline = {
      get(path) {
        return path === "Notes/Deleted.md" ? baselineEntry : null;
      },
      getConflict() {
        return null;
      },
      paths() {
        return ["Notes/Deleted.md"];
      },
    };
    const vault = {
      getAbstractFileByPath() {
        return null;
      },
      getFiles() {
        return [];
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, {}, baseline, plugin);
    const pending = {
      id: "delete-from-baseline",
      kind: "delete",
      path: "Notes/Deleted.md",
      createdAt: 1,
    };

    const actions = await engine.planActions(false, new Map(), [pending]);

    assert.deepEqual(actions, [
      {
        kind: "delete_remote",
        path: pending.path,
        baseRevision: 12,
        operationID: pending.id,
        operation: pending,
      },
    ]);
  } finally {
    await cleanup();
  }
});

test("source-side rename conflict preserves remote source and queues local target", async () => {
  globalThis.window = {
    setTimeout() {
      return 0;
    },
  };
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const localContent = new TextEncoder().encode("local target").buffer;
    const remoteContent = new TextEncoder().encode("remote source").buffer;
    const files = new Map([
      [
        "Notes/B.md",
        {
          __tfile: true,
          path: "Notes/B.md",
          content: localContent,
          stat: { mtime: 10, size: localContent.byteLength },
        },
      ],
    ]);
    const vault = {
      getAbstractFileByPath(path) {
        return files.get(path) ?? null;
      },
      getFiles() {
        return [...files.values()].filter((file) => file.__tfile);
      },
      async readBinary(file) {
        return file.content;
      },
      async createFolder(path) {
        files.set(path, { path });
      },
      async createBinary(path, content, options) {
        const file = {
          __tfile: true,
          path,
          content,
          stat: { mtime: options?.mtime ?? 0, size: content.byteLength },
        };
        files.set(path, file);
        return file;
      },
      async modifyBinary(file, content, options) {
        file.content = content;
        file.stat = { mtime: options?.mtime ?? 0, size: content.byteLength };
      },
    };
    const oldBaseline = {
      serverRevision: 1,
      serverHash: "old-hash",
      serverDeleted: false,
      localHash: "old-hash",
      localMTime: 1,
      localSize: 10,
    };
    const stored = new Map([["Notes/A.md", oldBaseline]]);
    let pending = [
      {
        id: "rename-a-b",
        kind: "rename",
        oldPath: "Notes/A.md",
        path: "Notes/B.md",
        createdAt: 1,
      },
    ];
    const baseline = {
      getConflict() {
        return null;
      },
      get(path) {
        return stored.get(path) ?? null;
      },
      set(path, value) {
        stored.set(path, value);
      },
      removePending(id) {
        pending = pending.filter((operation) => operation.id !== id);
      },
      removePendingForPath(path) {
        pending = pending.filter(
          (operation) => operation.path !== path && operation.oldPath !== path
        );
      },
      putPending(operation) {
        pending = pending.filter((item) => item.path !== operation.path);
        pending.push(operation);
      },
      async save() {},
    };
    const currentSource = {
      path: "Notes/A.md",
      type: "markdown",
      hash: "remote-hash",
      size: remoteContent.byteLength,
      mtime: 20,
      revision: 2,
      deleted: false,
    };
    const api = {
      async renameV2() {
        throw new Error("rename should not be attempted");
      },
      async downloadV2(_vaultID, path, revision) {
        assert.equal(path, "Notes/A.md");
        assert.equal(revision, 2);
        return { content: remoteContent, meta: currentSource };
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, api, baseline, plugin);
    const remote = new Map([["Notes/A.md", currentSource]]);

    await engine.processRenames("vault-a", pending, remote);

    assert.equal(
      new TextDecoder().decode(new Uint8Array(files.get("Notes/A.md").content)),
      "remote source"
    );
    assert.equal(files.get("Notes/B.md").content, localContent);
    assert.equal(stored.get("Notes/A.md").serverRevision, 2);
    assert.equal(pending.length, 1);
    assert.equal(pending[0].kind, "upsert");
    assert.equal(pending[0].path, "Notes/B.md");
    assert.equal(pending[0].oldPath, undefined);
  } finally {
    await cleanup();
  }
});

test("recovery snapshot deletes unchanged files whose tombstones were compacted", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const file = {
      __tfile: true,
      path: "Notes/Deleted.md",
      stat: { mtime: 10, size: 7 },
    };
    const baselineEntry = {
      serverRevision: 4,
      serverHash: "same-hash",
      serverDeleted: false,
      localHash: "same-hash",
      localMTime: 10,
      localSize: 7,
    };
    const baseline = {
      get(path) {
        return path === file.path ? baselineEntry : null;
      },
      getConflict() {
        return null;
      },
      paths() {
        return [file.path];
      },
      pending() {
        return [];
      },
    };
    const vault = {
      getAbstractFileByPath(path) {
        return path === file.path ? file : null;
      },
      getFiles() {
        return [file];
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, {}, baseline, plugin);
    const actions = await engine.planActions(true, new Map(), [], true);
    assert.deepEqual(actions, [{ kind: "delete_local_absent", path: file.path }]);
  } finally {
    await cleanup();
  }
});

test("recovery snapshot preserves locally changed compacted files as conflicts", async () => {
  const { SyncEngine, cleanup } = await loadSyncEngine();
  try {
    const file = {
      __tfile: true,
      path: "Notes/Changed.md",
      stat: { mtime: 20, size: 7 },
    };
    const baselineEntry = {
      serverRevision: 4,
      serverHash: "server-hash",
      serverDeleted: false,
      localHash: "local-hash",
      localMTime: 20,
      localSize: 7,
    };
    const baseline = {
      get(path) {
        return path === file.path ? baselineEntry : null;
      },
      getConflict() {
        return null;
      },
      paths() {
        return [file.path];
      },
    };
    const vault = {
      getAbstractFileByPath(path) {
        return path === file.path ? file : null;
      },
      getFiles() {
        return [file];
      },
    };
    const plugin = {
      settings: {
        syncPoisonObsidianFiles: false,
        syncIntervalSec: 300,
        remotePollIntervalSec: 30,
      },
    };
    const engine = new SyncEngine({ vault }, {}, baseline, plugin);
    const actions = await engine.planActions(true, new Map(), [], true);
    assert.equal(actions.length, 1);
    assert.equal(actions[0].kind, "conflict");
    assert.equal(actions[0].remote.deleted, true);
    assert.equal(actions[0].remote.revision, 0);
  } finally {
    await cleanup();
  }
});
