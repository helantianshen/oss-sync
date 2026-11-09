import { TFile } from "obsidian";
import type { Vault } from "obsidian";
import { normalizePath } from "./baseline.js";
import { configurationRelativePath } from "./blacklist.js";
import type { ExpectedLocalState } from "./ordinary-sync-actions.js";

/** 本地读取快照；hash 为 SHA-256 十六进制 */
export interface LocalReadResult {
  readonly bytes: Uint8Array;
  readonly hash: string;
  readonly mtime: number;
  readonly size: number;
}

/** 新建结果；stale 表示写入前本地已不符合预期，actual 为当前真实状态 */
export type CreateResult =
  | { readonly kind: "created"; readonly snapshot: LocalReadResult }
  | { readonly kind: "stale"; readonly actual: LocalReadResult | null };

/** 覆盖结果；stale 表示写入前本地已被改动 */
export type ReplaceResult =
  | { readonly kind: "replaced"; readonly snapshot: LocalReadResult }
  | { readonly kind: "stale"; readonly actual: LocalReadResult | null };

/** 删除结果；目标本不存在时按已删除处理 */
export type DeleteResult =
  | { readonly kind: "deleted" }
  | { readonly kind: "stale"; readonly actual: LocalReadResult | null };

/** 规范化写入结果；reason 区分目标丢失与内容被改动 */
export type WriteCanonicalResult =
  | { readonly kind: "written"; readonly snapshot: LocalReadResult }
  | { readonly kind: "stale"; readonly reason: "missing" | "mutated"; readonly actual?: LocalReadResult | null };

/** 保留副本结果；collision 表示同秒级时间戳的副本已存在，需换名重试 */
export type PreserveSiblingResult =
  | { readonly kind: "preserved"; readonly siblingPath: string; readonly snapshot: LocalReadResult }
  | { readonly kind: "stale"; readonly reason: "missing" | "mutated"; readonly actual?: LocalReadResult | null }
  | { readonly kind: "collision"; readonly siblingPath: string };

/** 生成带 ISO 时间戳的冲突副本路径，保留原扩展名 */
export function conflictCopyPath(path: string, now: number = Date.now()): string {
  const slash = path.lastIndexOf("/");
  const directory = slash >= 0 ? path.slice(0, slash + 1) : "";
  const filename = slash >= 0 ? path.slice(slash + 1) : path;
  const dot = filename.lastIndexOf(".");
  const base = dot > 0 ? filename.slice(0, dot) : filename;
  const extension = dot > 0 ? filename.slice(dot) : "";
  const timestamp = new Date(now).toISOString().replace(/[:.]/g, "-");
  return `${directory}${base}_conflict_${timestamp}${extension}`;
}

/** 本地文件写入前校验预期状态，避免同步结果覆盖已变化的文件 */
export class OrdinarySyncFileAccess {
  private readonly now: () => number;

  constructor(
    private readonly vault: Vault,
    private readonly suppress: (path: string) => void = () => undefined,
    now: () => number = () => Date.now(),
  ) {
    this.now = now;
  }

  /** 读取文件快照；路径不存在或不是普通文件时返回 null */
  async readExact(path: string): Promise<LocalReadResult | null> {
    const key = normalizePath(path);
    if (this.isConfiguration(key)) {
      const stat = await this.vault.adapter.stat(key);
      if (stat?.type !== "file") return null;
      const bytes = new Uint8Array(await this.vault.adapter.readBinary(key));
      return { bytes, hash: await hashBytes(bytes), mtime: stat.mtime, size: bytes.byteLength };
    }
    const file = this.vault.getAbstractFileByPath(key);
    if (!(file instanceof TFile)) {
      return null;
    }
    const buffer = await this.vault.readBinary(file);
    const bytes = new Uint8Array(buffer);
    const copy = bytes.slice();
    const hash = await hashBytes(copy);
    return {
      bytes: copy,
      hash,
      mtime: file.stat.mtime,
      size: file.stat.size,
    };
  }

  /** 仅在目标不存在且符合预期时新建；已存在一律视为 stale */
  async create(path: string, expected: ExpectedLocalState, bytes: Uint8Array): Promise<CreateResult> {
    const key = normalizePath(path);
    await this.ensureParentFolders(key);
    const actual = await this.readExact(key);
    const matches = isExpected(actual, expected);
    if (!matches) {
      return { kind: "stale", actual };
    }
    if (actual !== null) {
      return { kind: "stale", actual };
    }
    const fresh = freshBuffer(bytes);
    this.suppress(key);
    if (this.isConfiguration(key)) await this.vault.adapter.writeBinary(key, fresh);
    else await this.vault.createBinary(key, fresh);
    const snapshot = await this.snapshotFromBytes(bytes, key);
    return { kind: "created", snapshot };
  }

  /** 覆盖已存在文件；目标缺失或写入前已变动时返回 stale */
  async replace(path: string, expected: ExpectedLocalState, bytes: Uint8Array): Promise<ReplaceResult> {
    const key = normalizePath(path);
    await this.ensureParentFolders(key);
    const actual = await this.readExact(key);
    if (!isExpected(actual, expected)) {
      return { kind: "stale", actual };
    }
    if (actual === null) {
      return { kind: "stale", actual };
    }
    const fresh = freshBuffer(bytes);
    this.suppress(key);
    if (this.isConfiguration(key)) {
      await this.vault.adapter.writeBinary(key, fresh);
    } else {
      const file = this.vault.getAbstractFileByPath(key);
      if (!(file instanceof TFile)) return { kind: "stale", actual };
      await this.vault.modifyBinary(file, fresh);
    }
    const snapshot = await this.snapshotFromBytes(bytes, key);
    return { kind: "replaced", snapshot };
  }

  /** 仅在内容符合预期时删除；目标已缺失按已删除处理 */
  async deleteExact(path: string, expected: ExpectedLocalState): Promise<DeleteResult> {
    const key = normalizePath(path);
    const actual = await this.readExact(key);
    if (!isExpected(actual, expected)) {
      return { kind: "stale", actual };
    }
    if (actual === null) {
      if (!this.isConfiguration(key)) await this.removeEmptyParentFolders(key);
      return { kind: "deleted" };
    }
    if (this.isConfiguration(key)) {
      this.suppress(key);
      await this.vault.adapter.remove(key);
      return { kind: "deleted" };
    }
    const file = this.vault.getAbstractFileByPath(key);
    if (!(file instanceof TFile)) {
      return { kind: "stale", actual };
    }
    this.suppress(key);
    await this.vault.delete(file);
    await this.removeEmptyParentFolders(key);
    return { kind: "deleted" };
  }

  /** 以哈希为预期条件覆盖写入；失败时区分目标丢失与内容被改动 */
  async writeCanonicalIfUnchanged(
    path: string,
    expectedHash: string,
    newBytes: Uint8Array,
  ): Promise<WriteCanonicalResult> {
    const expected: ExpectedLocalState = { kind: "hash", hash: expectedHash };
    const result = await this.replace(path, expected, newBytes);
    if (result.kind === "replaced") {
      return { kind: "written", snapshot: result.snapshot };
    }
    const actual = result.actual;
    if (actual === null) {
      return { kind: "stale", reason: "missing", actual };
    }
    return { kind: "stale", reason: "mutated", actual };
  }

  /** 把本地正文另存为带时间戳的副本；副本路径已存在时返回 collision */
  async preserveSibling(
    canonicalPath: string,
    expected: ExpectedLocalState,
    localBytes: Uint8Array,
  ): Promise<PreserveSiblingResult> {
    const key = normalizePath(canonicalPath);
    const actual = await this.readExact(key);
    if (!isExpected(actual, expected)) {
      if (actual === null) {
        return { kind: "stale", reason: "missing", actual };
      }
      return { kind: "stale", reason: "mutated", actual };
    }
    if (actual === null) {
      return { kind: "stale", reason: "missing", actual };
    }
    const siblingPath = conflictCopyPath(key, this.now());
    const config = this.isConfiguration(siblingPath);
    if (config ? await this.vault.adapter.exists(siblingPath) : this.vault.getAbstractFileByPath(siblingPath) !== null) {
      return { kind: "collision", siblingPath };
    }
    await this.ensureParentFolders(siblingPath);
    const fresh = freshBuffer(localBytes);
    this.suppress(siblingPath);
    if (config) await this.vault.adapter.writeBinary(siblingPath, fresh);
    else await this.vault.createBinary(siblingPath, fresh);
    const snapshot = await this.snapshotFromBytes(localBytes, siblingPath);
    return { kind: "preserved", siblingPath, snapshot };
  }

  /** 以哈希为预期条件保留副本 */
  async preserveSiblingIfUnchanged(
    canonicalPath: string,
    expectedHash: string,
    localBytes: Uint8Array,
  ): Promise<PreserveSiblingResult> {
    const expected: ExpectedLocalState = { kind: "hash", hash: expectedHash };
    return this.preserveSibling(canonicalPath, expected, localBytes);
  }

  private async snapshotFromBytes(bytes: Uint8Array, path: string): Promise<LocalReadResult> {
    const snapshotBytes = bytes.slice();
    const snapshotHash = await hashBytes(snapshotBytes.slice());
    if (this.isConfiguration(path)) {
      const stat = await this.vault.adapter.stat(path);
      return { bytes: snapshotBytes, hash: snapshotHash, mtime: stat?.mtime ?? 0, size: snapshotBytes.byteLength };
    }
    const created = this.vault.getAbstractFileByPath(path);
    let mtime = 0;
    let size = snapshotBytes.length;
    if (created instanceof TFile) {
      mtime = created.stat.mtime;
      size = created.stat.size;
    }
    return { bytes: snapshotBytes, hash: snapshotHash, mtime, size };
  }

  /** 仅清理当前路径的空父目录，非递归删除可保护检查后新增的本地内容 */
  private async removeEmptyParentFolders(path: string): Promise<void> {
    const parts = normalizePath(path).split("/");
    if (parts.some((part) => !part || part === "." || part === ".." || part.includes(":"))) return;
    parts.pop();
    // 空路径对应 Vault 根目录，不参与列举或删除
    while (parts.length > 0) {
      const parent = parts.join("/");
      try {
        const entries = await this.vault.adapter.list(parent);
        if (entries.files.length > 0 || entries.folders.length > 0) return;
        this.suppress(parent);
        await this.vault.adapter.rmdir(parent, false);
      } catch {
        // 目录非空、已消失或不可访问时停止清理，文件删除结果仍然有效
        return;
      }
      parts.pop();
    }
  }

  private async ensureParentFolders(path: string): Promise<void> {
    const parts = normalizePath(path).split("/");
    parts.pop();
    let current = "";
    const config = this.isConfiguration(path);
    for (const part of parts) {
      current = current ? `${current}/${part}` : part;
      if (config) {
        if (!(await this.vault.adapter.exists(current))) {
          try {
            await this.vault.adapter.mkdir(current);
          } catch (error) {
            if ((await this.vault.adapter.stat(current))?.type !== "folder") throw error;
          }
        }
        continue;
      }
      if (this.vault.getAbstractFileByPath(current) === null) {
        try {
          await this.vault.createFolder(current);
        } catch (error) {
          if (this.vault.getAbstractFileByPath(current) === null) {
            throw error;
          }
        }
      }
    }
  }

  private isConfiguration(path: string): boolean {
    return configurationRelativePath(path, this.vault.configDir) !== null;
  }
}

function isExpected(actual: LocalReadResult | null, expected: ExpectedLocalState): boolean {
  if (expected.kind === "absent") {
    return actual === null;
  }
  return actual !== null && actual.hash === expected.hash;
}

function freshBuffer(bytes: Uint8Array): ArrayBuffer {
  const copy = new Uint8Array(bytes.length);
  copy.set(bytes);
  return copy.buffer;
}

async function hashBytes(bytes: Uint8Array): Promise<string> {
  const copy = bytes.slice();
  const digest = await crypto.subtle.digest("SHA-256", copy);
  return Array.from(new Uint8Array(digest))
    .map((value) => value.toString(16).padStart(2, "0"))
    .join("");
}
