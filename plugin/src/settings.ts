// 插件设置及默认值。
export interface OSSSettings {
  /** 服务端 URL（不含尾斜杠），例如 http://localhost:8080 */
  serverUrl: string;
  username: string;
  password: string;
  /** 本地变更触发同步前的防抖时间。 */
  syncIntervalSec: number;
  /** 同时执行的上传和下载任务数。 */
  maxConcurrency: number;
  /** 是否同步 .obsidian 下容易产生设备冲突的本地状态文件。 */
  syncPoisonObsidianFiles: boolean;
  /** 是否优先使用增量同步。 */
  incrementalCheck: boolean;
  /** 博客分享时是否保留目录结构。 */
  keepDirectoryTree: boolean;
  /** 当前本地 Obsidian Vault 绑定的服务端 Vault UUID。 */
  vaultId: string;
  /** 当前绑定的服务端 Vault 名称，仅用于 UI 展示。 */
  vaultName: string;
  /** 当前设备稳定 ID，不随登录或插件重启变化。 */
  clientId: string;
  /** 当前设备在服务端设备列表中的显示名称。 */
  deviceName: string;
  /** 没有本地变更时轮询远端 revision 的间隔。 */
  remotePollIntervalSec: number;
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
  vaultId: "",
  vaultName: "",
  clientId: "",
  deviceName: "",
  remotePollIntervalSec: 30,
};
