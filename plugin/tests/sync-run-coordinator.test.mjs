import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

async function loadCoordinator() {
  const dir = await mkdtemp(join(tmpdir(), "oss-coordinator-"));
  const outfile = join(dir, "coordinator.mjs");
  await build({
    entryPoints: ["src/sync-run-coordinator.ts"],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
  });
  const module = await import(pathToFileURL(outfile).href);
  return { Coordinator: module.SyncRunCoordinator, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

test("serializes overlapping runs and preserves a queued full sync", async () => {
  const { Coordinator, cleanup } = await loadCoordinator();
  try {
    const coordinator = new Coordinator();
    const modes = [];
    let active = 0;
    let maxActive = 0;
    let releaseFirst;
    const firstBlocked = new Promise((resolve) => {
      releaseFirst = resolve;
    });
    const task = async (forceFull) => {
      modes.push(forceFull);
      active += 1;
      maxActive = Math.max(maxActive, active);
      if (modes.length === 1) await firstBlocked;
      active -= 1;
    };

    const first = coordinator.run(false, task);
    const second = coordinator.run(true, task);
    await Promise.resolve();
    assert.equal(maxActive, 1);
    releaseFirst();
    await Promise.all([first, second]);
    assert.deepEqual(modes, [false, true]);
    assert.equal(maxActive, 1);
  } finally {
    await cleanup();
  }
});

test("clears queued state after a failed active run", async () => {
  const { Coordinator, cleanup } = await loadCoordinator();
  try {
    const coordinator = new Coordinator();
    const modes = [];
    let rejectFirst;
    const firstBlocked = new Promise((_, reject) => {
      rejectFirst = reject;
    });
    const failingTask = async (forceFull) => {
      modes.push(forceFull);
      await firstBlocked;
    };

    const first = coordinator.run(false, failingTask);
    const queued = coordinator.run(true, failingTask);
    rejectFirst(new Error("sync failed"));
    await Promise.allSettled([first, queued]);
    await coordinator.run(false, async (forceFull) => {
      modes.push(forceFull);
    });
    assert.deepEqual(modes, [false, false]);
  } finally {
    await cleanup();
  }
});
