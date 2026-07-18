import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

async function loadBaselineStore() {
  const dir = await mkdtemp(join(tmpdir(), "oss-baseline-"));
  const outfile = join(dir, "baseline.mjs");
  await build({
    entryPoints: ["src/baseline.ts"],
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
            contents: "export class TFile {}",
            loader: "js",
          }));
        },
      },
    ],
  });
  const module = await import(pathToFileURL(outfile).href);
  return {
    BaselineStore: module.BaselineStore,
    cleanup: () => rm(dir, { recursive: true, force: true }),
  };
}

function operation(id, kind, path, oldPath) {
  return {
    id,
    kind,
    path,
    oldPath,
    createdAt: 1,
  };
}

test("coalesces create followed by delete into one delete", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const store = new BaselineStore({});
    store.putPending(operation("create", "upsert", "Notes/New.md"));
    store.putPending(operation("delete", "delete", "Notes/New.md"));
    assert.deepEqual(store.pending(), [
      operation("delete", "delete", "Notes/New.md"),
    ]);
  } finally {
    await cleanup();
  }
});

test("coalesces a rename chain back to the original path", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const store = new BaselineStore({});
    store.putPending(operation("rename-1", "rename", "Notes/B.md", "Notes/A.md"));
    store.putPending(operation("rename-2", "rename", "Notes/C.md", "Notes/B.md"));
    assert.deepEqual(store.pending(), [
      operation("rename-2", "rename", "Notes/C.md", "Notes/A.md"),
    ]);
  } finally {
    await cleanup();
  }
});

test("turns rename followed by delete into delete of the server path", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const store = new BaselineStore({});
    store.putPending(operation("rename", "rename", "Notes/B.md", "Notes/A.md"));
    store.putPending(operation("delete", "delete", "Notes/B.md"));
    assert.deepEqual(store.pending(), [
      operation("delete", "delete", "Notes/A.md"),
    ]);
  } finally {
    await cleanup();
  }
});

test("keeps rename authoritative when modify follows it", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const store = new BaselineStore({});
    store.putPending(operation("rename", "rename", "Notes/B.md", "Notes/A.md"));
    store.putPending(operation("modify", "upsert", "Notes/B.md"));
    assert.deepEqual(store.pending(), [
      operation("rename", "rename", "Notes/B.md", "Notes/A.md"),
    ]);
  } finally {
    await cleanup();
  }
});

test("loads and overwrites a hidden baseline through the vault adapter", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const files = new Map([
      [
        ".oss-sync-state.json",
        JSON.stringify({
          version: 2,
          vaultId: "vault-a",
          cursor: 7,
          files: {},
          pending: [],
          conflicts: [],
        }),
      ],
    ]);
    const writes = [];
    const store = new BaselineStore({
      adapter: {
        async exists(path) {
          return files.has(path);
        },
        async read(path) {
          return files.get(path);
        },
        async write(path, raw) {
          writes.push({ path, raw });
          files.set(path, raw);
        },
      },
    });

    await store.load();
    assert.equal(store.getCursor(), 7);
    store.setCursor(8);
    await store.save();

    assert.equal(writes.length, 1);
    assert.equal(JSON.parse(files.get(".oss-sync-state.json")).cursor, 8);
  } finally {
    await cleanup();
  }
});

test("serializes baseline writes so an older snapshot cannot win", async () => {
  const { BaselineStore, cleanup } = await loadBaselineStore();
  try {
    const files = new Map();
    let writeCount = 0;
    let activeWrites = 0;
    let maxActiveWrites = 0;
    let releaseFirst;
    let markFirstStarted;
    const firstStarted = new Promise((resolve) => {
      markFirstStarted = resolve;
    });
    const firstRelease = new Promise((resolve) => {
      releaseFirst = resolve;
    });
    const store = new BaselineStore({
      adapter: {
        async exists(path) {
          return files.has(path);
        },
        async read(path) {
          return files.get(path);
        },
        async write(path, raw) {
          writeCount += 1;
          activeWrites += 1;
          maxActiveWrites = Math.max(maxActiveWrites, activeWrites);
          if (writeCount === 1) {
            markFirstStarted();
            await firstRelease;
          }
          files.set(path, raw);
          activeWrites -= 1;
        },
      },
    });

    await store.load();
    store.setCursor(1);
    const firstSave = store.save();
    await firstStarted;
    store.setCursor(2);
    const secondSave = store.save();
    releaseFirst();
    await Promise.all([firstSave, secondSave]);

    assert.equal(maxActiveWrites, 1);
    assert.equal(JSON.parse(files.get(".oss-sync-state.json")).cursor, 2);
  } finally {
    await cleanup();
  }
});
