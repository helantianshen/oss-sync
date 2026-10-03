import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import test from "node:test";
import { build } from "esbuild";

async function loadNoticeGuard() {
  const dir = await mkdtemp(join(tmpdir(), "oss-notice-guard-"));
  const outfile = join(dir, "notice-guard.mjs");
  await build({
    entryPoints: ["src/notice-guard.ts"],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
  });
  const module = await import(pathToFileURL(outfile).href);
  return { ...module, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

test("repeated failures within the cooldown show once", async () => {
  const { NoticeGuard, cleanup } = await loadNoticeGuard();
  try {
    const guard = new NoticeGuard();
    const base = 1_000_000;
    assert.equal(guard.shouldShow("sync.run", base), true);
    assert.equal(guard.shouldShow("sync.run", base + 3_000), false);
    assert.equal(guard.shouldShow("sync.run", base + 29_000), false);
  } finally {
    await cleanup();
  }
});

test("failure after the cooldown elapses shows again", async () => {
  const { NoticeGuard, cleanup } = await loadNoticeGuard();
  try {
    const guard = new NoticeGuard();
    const base = 1_000_000;
    assert.equal(guard.shouldShow("sync.run", base), true);
    assert.equal(guard.shouldShow("sync.run", base + 30_000), true);
  } finally {
    await cleanup();
  }
});

test("different keys are suppressed independently", async () => {
  const { NoticeGuard, cleanup } = await loadNoticeGuard();
  try {
    const guard = new NoticeGuard();
    const base = 1_000_000;
    assert.equal(guard.shouldShow("sync.run", base), true);
    assert.equal(guard.shouldShow("collab.list", base), true);
    assert.equal(guard.shouldShow("sync.run", base + 1_000), false);
    assert.equal(guard.shouldShow("collab.list", base + 1_000), false);
  } finally {
    await cleanup();
  }
});

test("reset after success lets the next failure show immediately", async () => {
  const { NoticeGuard, cleanup } = await loadNoticeGuard();
  try {
    const guard = new NoticeGuard();
    const base = 1_000_000;
    assert.equal(guard.shouldShow("sync.run", base), true);
    assert.equal(guard.shouldShow("sync.run", base + 1_000), false);
    guard.reset();
    assert.equal(guard.shouldShow("sync.run", base + 1_500), true);
  } finally {
    await cleanup();
  }
});

test("reset with a key clears only that key", async () => {
  const { NoticeGuard, cleanup } = await loadNoticeGuard();
  try {
    const guard = new NoticeGuard();
    const base = 1_000_000;
    guard.shouldShow("sync.run", base);
    guard.shouldShow("collab.list", base);
    guard.reset("collab.list");
    assert.equal(guard.shouldShow("collab.list", base + 500), true);
    assert.equal(guard.shouldShow("sync.run", base + 500), false);
  } finally {
    await cleanup();
  }
});
