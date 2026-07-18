// OSS Plugin — default settings, can be overridden by user.
export interface OSSSettings {
  /** 服务端 URL（不含尾斜杠），例如 http://localhost:8080 */
  serverUrl: string;
  username: string;
  password: string;
  /** 防抖时间（秒）。决策 7.2 同步间隔，默认 300 = 5 分钟 */
  syncIntervalSec: number;
  /** 决策 7.2：并发上限，1–10，默认 6 */
  maxConcurrency: number;
  /** 决策 6.2：是否同步 .obsidian/ 毒药文件（默认关闭） */
  syncPoisonObsidianFiles: boolean;
  /** 决策 6.5：是否使用增量 check 模式（默认 true） */
  incrementalCheck: boolean;
  /** 是否保留目录树（决策 0.5 / Phase 2 服务端按 path 落盘，此项仅影响客户端展示） */
  keepDirectoryTree: boolean;
}

export const DEFAULT_SETTINGS: OSSSettings = {
  serverUrl: "http://localhost:8080",
  username: "",
  password: "",
  syncIntervalSec: 300,
  maxConcurrency: 6,
  syncPoisonObsidianFiles: false,
  incrementalCheck: true,
  keepDirectoryTree: true,
};
