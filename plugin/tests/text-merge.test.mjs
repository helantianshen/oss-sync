import assert from "node:assert/strict";
import test from "node:test";
import { loadSource } from "./helpers/source-loader.mjs";

test("三方合并保留双方独立修改并统一换行，不把相邻替换误判为冲突", async (t) => {
  const { mergeText } = await loadSource(t, "text-merge");
  assert.deepEqual(mergeText(
    "标题\r\n原第一行\r\n原第二行\r\n结尾\r\n",
    "标题\r\n本地第一行\r\n原第二行\r\n结尾\r\n",
    "标题\n原第一行\n远端第二行\n结尾\n",
  ), { kind: "merged", content: "标题\n本地第一行\n远端第二行\n结尾\n" });
  assert.deepEqual(mergeText(
    "开头\n中间\n结尾\n",
    "本地新增\n开头\n中间\n结尾\n",
    "开头\n中间\n结尾\n远端新增\n",
  ), { kind: "merged", content: "本地新增\n开头\n中间\n结尾\n远端新增\n" });
});

test("重叠修改不能自动覆盖，逐块选择顺序仍完整保留双方正文与公共上下文", async (t) => {
  const { mergeText, buildMergeRegions, hasConflictRegion, resolveOrderedMerge } = await loadSource(t, "text-merge");
  const base = "标题\n共同第一版\n公共分隔甲\n公共分隔乙\n共同第二版\n结尾\n";
  const local = "标题\n本地第一版\n公共分隔甲\n公共分隔乙\n本地第二版\n结尾\n";
  const remote = "标题\n远端第一版\n公共分隔甲\n公共分隔乙\n远端第二版\n结尾\n";
  assert.deepEqual(mergeText(base, local, remote), { kind: "conflict" });
  const regions = buildMergeRegions(base, local, remote);
  assert.equal(hasConflictRegion(regions), true);
  assert.deepEqual(regions.filter((region) => region.kind === "conflict"), [
    { kind: "conflict", baseLines: ["共同第一版"], localLines: ["本地第一版"], remoteLines: ["远端第一版"] },
    { kind: "conflict", baseLines: ["共同第二版"], localLines: ["本地第二版"], remoteLines: ["远端第二版"] },
  ]);
  assert.equal(resolveOrderedMerge(regions, (index) => index === 0 ? "local_first" : "remote_first"),
    "标题\n本地第一版\n远端第一版\n公共分隔甲\n公共分隔乙\n远端第二版\n本地第二版\n结尾\n");
  assert.equal(resolveOrderedMerge(regions, (index) => index === 0 ? "remote_first" : "local_first"),
    "标题\n远端第一版\n本地第一版\n公共分隔甲\n公共分隔乙\n本地第二版\n远端第二版\n结尾\n");
});

test("正文解码拒绝二进制类型与损坏 UTF-8，并按字节在 2 MiB 边界保护合并", async (t) => {
  const { decodeMergeableText } = await loadSource(t, "text-merge");
  const encoder = new TextEncoder();
  assert.equal(decodeMergeableText("note.MD", encoder.encode("中文正文\n")), "中文正文\n");
  assert.equal(decodeMergeableText("image.png", encoder.encode("不能按扩展名误当正文")), null);
  assert.equal(decodeMergeableText("archive.zip", new Uint8Array([0x50, 0x4b, 0x03, 0x04])), null);
  assert.equal(decodeMergeableText("note.md", new Uint8Array([0xc3, 0x28])), null);
  assert.equal(decodeMergeableText("note.md", new Uint8Array([0xf0, 0x9f, 0x92])), null);
  const limit = 2 * 1024 * 1024;
  const exactText = "界".repeat(Math.floor(limit / 3)) + "ab";
  const exactBytes = encoder.encode(exactText);
  assert.equal(exactBytes.byteLength, limit);
  assert.equal(decodeMergeableText("large.md", exactBytes), exactText);
  assert.equal(decodeMergeableText("large.md", encoder.encode(exactText + "x")), null);
  // 字符数仍低于上限，UTF-8 字节数超限时也不能进入合并
  assert.equal(decodeMergeableText("large.md", encoder.encode("界".repeat(Math.floor(limit / 3) + 1))), null);
});
