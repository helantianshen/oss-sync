// 基线管理 — 决策 1。
//
// 在 vault 根维护本地基线文件 .oss-sync-state.json：
//   { "<vault相对路径>": { "serverMTime": <int>, "hash": <string> } }
//
// 仅在某文件成功 upload 或 download 完成后，用服务端返回的最新 mtime/hash
// 覆盖该路径条目。失败、冲突未解决期间不得更新基线。
//
// 首次同步：本地无基线条目时，视为 download_needed（拉服务端 mtime 建基线）；
// 若服务端也无记录，视为 upload_needed（上传后建立基线）。
//
// 已知限制：基线是设备本地文件，同一用户在多台设备编辑时，第二台设备的基线
// 没有第一台写入的服务端 mtime，多设备冲突检测会退化（详见决策 1 末尾注释）。
// 我们不在客户端对此做兜底——服务端 LWW 直接覆盖是已知妥协。

import type { Vault } from "obsidian";
import { TFile } from "obsidian";

export const BASELINE_FILENAME = ".oss-sync-state.json";

export interface BaselineEntry {
  serverMTime: number;
  hash: string;
}

export type BaselineMap = Record<string, BaselineEntry>;

export class BaselineStore {
  private data: BaselineMap = {};
  private loaded = false;

  constructor(private vault: Vault) {}

  /** 加载基线文件，文件不存在时视为空 map。 */
  async load(): Promise<void> {
    let tf = this.vault.getAbstractFileByPath(BASELINE_FILENAME);
    if (!tf || !(tf instanceof TFile)) {
      this.data = {};
      this.loaded = true;
      return;
    }
    const raw = await this.vault.read(tf as TFile);
    try {
      const parsed = JSON.parse(raw);
      if (parsed && typeof parsed === "object") {
        this.data = parsed as BaselineMap;
      } else {
        this.data = {};
      }
    } catch {
      this.data = {};
    }
    this.loaded = true;
  }

  /** 持久化基线文件。会创建隐藏文件（Obsidian 默认隐藏 .开头）。 */
  async save(): Promise<void> {
    const raw = JSON.stringify(this.data, null, 2);
    let tf = this.vault.getAbstractFileByPath(BASELINE_FILENAME);
    if (!tf || !(tf instanceof TFile)) {
      try {
        await this.vault.create(BASELINE_FILENAME, raw);
      } catch (error: unknown) {
        tf = this.vault.getAbstractFileByPath(BASELINE_FILENAME);
        if (!(tf instanceof TFile)) throw error;
        await this.vault.modify(tf, raw);
      }
      return;
    }
    await this.vault.modify(tf as TFile, raw);
  }

  /** 取某路径的基线条目，未存在返回 null。 */
  get(path: string): BaselineEntry | null {
    return this.data[normalizePath(path)] ?? null;
  }

  /** 覆盖某路径的基线（仅 upload/download 成功后调用）。 */
  set(path: string, entry: BaselineEntry): void {
    this.data[normalizePath(path)] = entry;
  }

  /** 清除某路径的基线条目（删除文件时同步清除）。 */
  delete(path: string): void {
    delete this.data[normalizePath(path)];
  }

  /** 全量覆盖。Phase 3 暂不导出，Phase 5 用得到。 */
  _all(): BaselineMap {
    return this.data;
  }
}

// Obsidian 的 TAbstractFile.path 已经是 vault 内相对路径，且用 / 分隔。
// 这里仅做安全归一化，确保与黑名单、check 请求路径一致。
export function normalizePath(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}
