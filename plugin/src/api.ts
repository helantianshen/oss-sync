// OSS 后端 HTTP 客户端
//
// JWT 由插件通过 loadData()/saveData() 持久化
// 失效或登录失败时抛错，由调用方决定 UI 提示

import { classifySyncPath } from "./blacklist.js";
import type { Diagnostics } from "./diagnostics";
import type { OSSSettings } from "./settings";
import { isValidOperationID, isValidServerRevision } from "./operation-id.js";
import { httpRequest } from "./http-transport.js";

/** 登录响应；device_status 为 pending 时令牌可用但需等待授权 */
export interface AuthResponse {
  token: string;
  expires_in: number;
  user_id: number;
  username: string;
  role: string;
  device_status?: "pending" | "approved" | "revoked";
  device_name?: string;
}

/** 服务端注册与管理员入口的开放状态 */
export interface AuthStatus {
  readonly needs_first_admin: boolean;
  readonly registration_enabled: boolean;
  readonly registration_mode: "open" | "closed";
  readonly registration_url: string;
  readonly admin_url: string;
}

interface CheckFileIn {
  path: string;
  mtime: number;
  hash: string;
}

interface CheckFileOut {
  path: string;
  status: "upload_needed" | "download_needed" | "in_sync" | "conflict_detected" | "assume_in_sync";
  server_mtime?: number;
  server_hash?: string;
}

interface CheckResponse {
  server_time: number;
  results: CheckFileOut[];
}

/** 上传结果 */
export interface UploadResult {
  path: string;
  hash: string;
  mtime: number;
  server_time: number;
}

/** 分享条目 */
export interface ShareOut {
  share_id: string;
  vault_id: string;
  target_path: string;
  is_folder: boolean;
  allow_copy: boolean;
  views: number;
  url: string;
  created_at: string;
}

/** 创建分享结果；extra 为同一目录下同时被分享的链接笔记 */
export interface ShareCreateResult {
  share_id: string;
  url: string;
  target_path: string;
  is_folder: boolean;
  extra?: ShareOut[];
}

/** 仓库条目 */
export interface VaultOut {
  id: string;
  name: string;
  description: string;
  is_default: boolean;
  access_role: "owner" | "manager" | "participant";
  storage_quota: number;
  storage_used: number;
  head_revision: number;
  created_at: string;
  updated_at: string;
}

/** 设备在某仓库中的同步进度 */
export interface DeviceVaultOut {
  vault_id: string;
  vault_name: string;
  last_cursor: number;
  head_revision: number;
  pending_changes: number;
  last_sync_at?: string;
}

/** 设备条目；stale 表示超过阈值未活跃 */
export interface DeviceOut {
  client_id: string;
  name: string;
  last_seen_at: string;
  created_at: string;
  revoked_at?: string;
  stale: boolean;
  is_current: boolean;
  vaults: DeviceVaultOut[];
}

/** 服务端文件元信息 */
export interface SyncFileMeta {
  path: string;
  type: "markdown" | "attachment" | "config";
  hash: string;
  size: number;
  mtime: number;
  revision: number;
  deleted: boolean;
  server_time?: number;
}

/** 协作上传参数；baseRevision 与 operationID 由本地基线派生 */
export interface CollaborationUploadInput {
  readonly content: string;
  readonly baseRevision: number;
  readonly operationID: string;
}

/** 清单或变更分页；recovery_snapshot 表示本次为历史压缩后的全量快照 */
export interface SyncManifestResponse {
  snapshot_revision: number;
  compacted_revision: number;
  next_cursor: number;
  has_more: boolean;
  recovery_snapshot: boolean;
  server_time: number;
  files: SyncFileMeta[];
}

/** 同步模式 */
export type SyncMode = "short_poll" | "long_poll";

/** 服务端仓库同步策略；effective_mode 已综合仓库策略与客户端偏好 */
export interface SyncStrategyResponse {
  policy: string;
  effective_mode: SyncMode;
  min_debounce_sec: number;
  long_poll_wait_sec: number;
}

/** 文件历史条目；has_snapshot 表示可读取正文 */
export interface HistoryEntry {
  id: number;
  file_path: string;
  previous_path?: string;
  action: string;
  version: number;
  revision: number;
  username: string;
  device_name: string;
  has_snapshot: boolean;
  created_at: string;
}

/** 历史详情；is_text 为 false 时 content 缺失，diff 为空 */
export interface HistoryDetail extends HistoryEntry {
  content?: string;
  diff: string[];
  is_text: boolean;
  /** 本次 diff 的对比基准，由服务端回显 */
  mode: string;
}

/** 回收站条目 */
export interface RecycleBinFile {
  id: number;
  path: string;
  type: "markdown" | "attachment" | "config";
  size: number;
  deleted_at: string;
  expires_at: string;
  remaining_seconds: number;
  can_restore: boolean;
}

/** 协作关系条目 */
export interface CollabEntry {
  id: number;
  file_id: number;
  vault_id: string;
  file_path: string;
  owner_id: number;
  owner_username: string;
  collaborator_id: number;
  collaborator_username: string;
  status: string;
  /** 协作者是否可读该文件的修改记录，仅 owner 或 manager 可改 */
  allow_history_view: boolean;
  created_at: string;
}

/** 服务端 API 错误；current、compactedRevision 与 headRevision 仅在同步接口返回 */
export class OSSApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly current?: SyncFileMeta,
    readonly code?: string,
    readonly compactedRevision?: number,
    readonly headRevision?: number
  ) {
    super(message);
    this.name = "OSSApiError";
  }
}

/** 服务端版本信息 */
export interface ServerVersionInfo {
  readonly version: string;
  readonly env: string;
  readonly commit?: string;
  readonly built_at?: string;
}

/** 服务端插件钩子描述 */
export interface ServerPluginHook {
  readonly name: string;
  readonly id?: string;
  readonly label?: string;
}

/** 服务端插件能力 */
export interface ServerPluginCapability {
  readonly plugin_id: string;
  readonly name: string;
  readonly hooks: readonly ServerPluginHook[];
}

/** 服务端插件能力列表 */
export interface ServerPluginCapabilitiesResponse {
  readonly plugins: readonly ServerPluginCapability[];
}

/** 服务端插件钩子执行结果 */
export interface ServerPluginHookResponse {
  readonly content: string;
}

/** 服务端更新候选 */
export interface ServerUpdateCandidate {
  readonly version: string;
  readonly tag: string;
  readonly goos: string;
  readonly goarch: string;
  readonly asset_name: string;
  readonly asset_url: string;
  readonly release_url: string;
  readonly size: number;
  readonly digest: string;
  readonly release_id: number;
  readonly asset_id: number;
}

/** 服务端更新检查结果；check_id 在触发更新时回传以绑定候选 */
export interface ServerUpdateCheckResponse {
  readonly check_id: string;
  readonly candidate: ServerUpdateCandidate;
  readonly current_version: string;
  readonly latest_version: string;
  readonly update_available: boolean;
  readonly release_url: string;
  readonly expires_at: number;
}

/** 服务端更新操作记录 */
export interface ServerUpdateOperation {
  readonly id: string;
  readonly state: string;
  readonly version: string;
  readonly started_at_unix: number;
  readonly updated_at_unix: number;
  readonly error?: string;
}

/** 服务端更新状态 */
export interface ServerUpdateStatusResponse {
  readonly version: string;
  readonly exec_path: string;
  readonly backup_path: string;
  readonly update_in_progress: boolean;
  readonly state: string;
  readonly last_check?: {
    readonly checked_at: string;
    readonly current_version: string;
    readonly latest_version?: string;
    readonly update_available: boolean;
    readonly release_url?: string;
    readonly note?: string;
  };
  readonly last_update?: {
    readonly at: string;
    readonly ok: boolean;
    readonly code: string;
    readonly phase: string;
    readonly state: string;
    readonly version?: string;
    readonly error?: string;
    readonly backup_path?: string;
  };
  readonly manager_status?: {
    readonly active?: ServerUpdateOperation;
    readonly history: readonly ServerUpdateOperation[];
  };
}

/** 触发服务端更新的响应；ok 为 false 时 error 或 code 说明原因 */
export interface ServerUpdateTriggerResponse {
  readonly ok: boolean;
  readonly code: string;
  readonly operation?: ServerUpdateOperation;
  readonly version?: string;
  readonly error?: string;
}

/** 服务端 HTTP 客户端；令牌由插件持久化后注入，请求失败统一抛 OSSApiError */
export class OSSApiClient {
  /** 服务端时间与本地时间的偏移，单位为毫秒*/
  private timeOffset = 0;
  private token: string | null = null;

  constructor(
    private settings: OSSSettings,
    private readonly diagnostics?: Diagnostics,
    private readonly onTokenExpired?: () => Promise<void>,
    private readonly configDir = ""
  ) {}

  /** 设置或清除当前令牌 */
  setToken(token: string | null): void {
    this.token = token;
  }

  /** 是否持有令牌 */
  hasToken(): boolean {
    return !!this.token;
  }

  /** 把本地 mtime 换算为服务端时间轴上的值 */
  getAdjustedMtime(localMtimeMs: number): number {
    return localMtimeMs + this.timeOffset;
  }

  /** 时钟偏移超过 5 分钟时提示用户，避免 mtime 比较失真 */
  isClockDriftLarge(): boolean {
    return Math.abs(this.timeOffset) > 5 * 60 * 1000;
  }

  /** 当前时钟偏移，单位为毫秒 */
  getTimeOffset(): number {
    return this.timeOffset;
  }

  /** 查询服务端注册状态 */
  async authStatus(): Promise<AuthStatus> {
    return this.doRequest<AuthStatus>("GET", "/api/auth/status");
  }

  /** 校验当前令牌是否有管理员权限（服务端 /api/admin 拒绝非管理员）*/
  async checkAdminAccess(): Promise<boolean> {
    try {
      await this.doRequest("GET", "/api/admin/users");
      return true;
    } catch (error) {
      if (error instanceof OSSApiError && (error.status === 401 || error.status === 403)) {
        return false;
      }
      throw error;
    }
  }

  /** 获取服务端版本信息 */
  async getServerVersion(): Promise<ServerVersionInfo> {
    return this.doRequest<ServerVersionInfo>("GET", "/api/admin/version");
  }

  /** 列出服务端已注册插件的编辑器命令钩子 */
  async getServerPluginCapabilities(): Promise<ServerPluginCapabilitiesResponse> {
    return this.doRequest<ServerPluginCapabilitiesResponse>("GET", "/api/plugin-capabilities");
  }

  /** 执行服务端插件钩子 */
  async runServerPluginHook(hook: string, content: string, metadata: Record<string, string> = {}): Promise<ServerPluginHookResponse> {
    return this.doRequest<ServerPluginHookResponse>("POST", `/api/plugin-hooks/${encodeURIComponent(hook)}`, {
      vault_id: this.settings.vaultId,
      content,
      metadata,
    });
  }

  /** 检查服务端新版本 */
  async checkServerUpdate(): Promise<ServerUpdateCheckResponse> {
    return this.doRequest<ServerUpdateCheckResponse>("GET", "/api/admin/update/check");
  }

  /** 查询服务端更新进度 */
  async getServerUpdateStatus(): Promise<ServerUpdateStatusResponse> {
    return this.doRequest<ServerUpdateStatusResponse>("GET", "/api/admin/update/status");
  }

  /** 触发服务端更新到指定版本 */
  async triggerServerUpdate(opts: { readonly checkId: string; readonly expectedVersion: string }): Promise<ServerUpdateTriggerResponse> {
    return this.doRequest<ServerUpdateTriggerResponse>("POST", "/api/admin/update", {
      check_id: opts.checkId,
      expected_version: opts.expectedVersion,
    });
  }

  /** 登录并缓存返回的令牌 */
  async login(): Promise<AuthResponse> {
    const res = await this.doRequest<AuthResponse>("POST", "/api/auth/login", {
      username: this.settings.username,
      password: this.settings.password,
    });
    this.token = res.token;
    return res;
  }

  /** 列出当前用户可见的仓库 */
  async listVaults(): Promise<{ vaults: VaultOut[] }> {
    return this.doRequest<{ vaults: VaultOut[] }>("GET", "/api/vaults");
  }

  /** 创建仓库 */
  async createVault(name: string): Promise<VaultOut> {
    return this.doRequest<VaultOut>("POST", "/api/vaults", { name });
  }

  /** 列出当前用户的设备 */
  async listDevices(): Promise<{ devices: DeviceOut[]; stale_after_days: number }> {
    return this.doRequest<{ devices: DeviceOut[]; stale_after_days: number }>(
      "GET",
      "/api/devices"
    );
  }

  /** 拉取全量清单；after 与 wait 由调用方控制分页与长轮询等待 */
  async manifest(vaultID: string, after = 0, waitSeconds = 0): Promise<SyncManifestResponse> {
    const before = Date.now();
    const result = await this.doRequest<SyncManifestResponse>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/manifest` +
        `?after=${encodeURIComponent(String(after))}` +
        `&limit=500&wait=${encodeURIComponent(String(waitSeconds))}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
    this.updateTimeOffset(result.server_time, before, Date.now());
    return result;
  }

  /** 拉取自 cursor 之后的变更；waitSeconds 用于长轮询 */
  async changes(vaultID: string, after: number, waitSeconds = 0): Promise<SyncManifestResponse> {
    const before = Date.now();
    const result = await this.doRequest<SyncManifestResponse>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/changes` +
        `?after=${encodeURIComponent(String(after))}` +
        `&limit=500&wait=${encodeURIComponent(String(waitSeconds))}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
    this.updateTimeOffset(result.server_time, before, Date.now());
    return result;
  }

  /** 确认已处理到指定 cursor，服务端据此清理该设备的待推送队列 */
  async acknowledge(vaultID: string, cursor: number): Promise<void> {
    await this.doRequest<void>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/ack`,
      {
        client_id: this.settings.clientId,
        cursor,
      }
    );
  }

  /** 上传文件正文；body 为二进制，参数走 query，diagnostics 在 finally 中记录耗时与字节数 */
  async uploadV2(
    vaultID: string,
    input: {
      path: string;
      baseRevision: number;
      hash: string;
      mtime: number;
      operationID: string;
      content: ArrayBuffer;
    }
  ): Promise<SyncFileMeta> {
    const startedAt = Date.now();
    let status: number | undefined;
    const query = new URLSearchParams({
      path: input.path,
      base_revision: String(input.baseRevision),
      hash: input.hash,
      mtime: String(input.mtime),
      client_id: this.settings.clientId,
      operation_id: input.operationID,
    });
    try {
      const res = await httpRequest({
        url: this.url(`/api/vaults/${encodeURIComponent(vaultID)}/sync/upload?${query.toString()}`),
        method: "POST",
        headers: {
  "Content-Type": "application/octet-stream",
  ...this.authHeaders(),
          },
        body: input.content,
        });
      status = res.status;
      return this.parseResponse<SyncFileMeta>(res.status, res.json, res.text);
    } finally {
      this.recordDirectAPI("POST", status, startedAt);
      this.diagnostics?.record({
        kind: "transfer",
        at: Date.now(),
        scope: "upload",
        durationMs: Date.now() - startedAt,
        bytes: input.content.byteLength,
      });
    }
  }

  /** 下载指定 revision 的正文；文件元信息从响应头解析 */
  async downloadV2(
    vaultID: string,
    path: string,
    revision: number
  ): Promise<{ content: ArrayBuffer; meta: SyncFileMeta }> {
    const startedAt = Date.now();
    let status: number | undefined;
    let bytes: number | undefined;
    const query = new URLSearchParams({ path, revision: String(revision) });
    try {
      const res = await httpRequest({
        url: this.url(`/api/vaults/${encodeURIComponent(vaultID)}/sync/download?${query.toString()}`),
        method: "GET",
        headers: this.authHeaders(),
      });
      status = res.status;
      if (res.status >= 400) {
        await this.parseResponse<never>(res.status, res.json, res.text);
      }
      bytes = res.arrayBuffer.byteLength;
      return {
        content: res.arrayBuffer,
        meta: {
          path,
          type: classifySyncPath(path, this.configDir),
          hash: header(res.headers, "x-oss-hash"),
          size: res.arrayBuffer.byteLength,
          mtime: parseInt(header(res.headers, "x-oss-mtime") || "0", 10),
          revision: parseInt(header(res.headers, "x-oss-revision") || "0", 10),
          deleted: false,
        },
      };
    } finally {
      this.recordDirectAPI("GET", status, startedAt);
      this.diagnostics?.record({
        kind: "transfer",
        at: Date.now(),
        scope: "download",
        durationMs: Date.now() - startedAt,
        ...(bytes === undefined ? {} : { bytes }),
      });
    }
  }

  /** 删除服务端文件 */
  async deleteV2(
    vaultID: string,
    input: {
      path: string;
      baseRevision: number;
      operationID: string;
      mtime: number;
    }
  ): Promise<SyncFileMeta> {
    return this.doRequest<SyncFileMeta>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/delete`,
      {
        path: input.path,
        base_revision: input.baseRevision,
        client_id: this.settings.clientId,
        operation_id: input.operationID,
        client_mtime: input.mtime,
      }
    );
  }

  /** 重命名服务端文件；目标位置需提供 targetRevision 以检测并发修改 */
  async renameV2(
    vaultID: string,
    input: {
      oldPath: string;
      newPath: string;
      baseRevision: number;
      targetRevision: number;
      operationID: string;
      mtime: number;
    }
  ): Promise<{ old: SyncFileMeta; new: SyncFileMeta }> {
    return this.doRequest<{ old: SyncFileMeta; new: SyncFileMeta }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/rename`,
      {
        old_path: input.oldPath,
        new_path: input.newPath,
        base_revision: input.baseRevision,
        target_revision: input.targetRevision,
        client_id: this.settings.clientId,
        operation_id: input.operationID,
        client_mtime: input.mtime,
      }
    );
  }

  /** 获取仓库同步策略，effective_mode 由服务端根据仓库策略与客户端偏好计算*/
  async syncStrategy(vaultID: string, mode: SyncMode): Promise<SyncStrategyResponse> {
    return this.doRequest<SyncStrategyResponse>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/strategy` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}` +
        `&mode=${encodeURIComponent(mode)}`
    );
  }

  /** 查询指定路径的修改历史*/
  async history(vaultID: string, path: string): Promise<{ history: HistoryEntry[] }> {
    return this.doRequest<{ history: HistoryEntry[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/history` +
        `?path=${encodeURIComponent(path)}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 查询历史版本详情；mode 为 last 时与该版本上一版比较，current 时与当前版本比较 */
  async historyDetail(
    vaultID: string,
    historyID: number,
    mode: "last" | "current"
  ): Promise<HistoryDetail> {
    return this.doRequest<HistoryDetail>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/history/${encodeURIComponent(String(historyID))}` +
        `?mode=${mode}&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 恢复历史版本为当前版本 */
  async historyRestore(vaultID: string, historyID: number): Promise<{ path: string }> {
    return this.doRequest<{ path: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/history/${encodeURIComponent(String(historyID))}/restore` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 列出仓库回收站 */
  async recycleList(vaultID: string): Promise<{ files: RecycleBinFile[] }> {
    return this.doRequest<{ files: RecycleBinFile[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 从回收站恢复文件 */
  async recycleRestore(vaultID: string, fileID: number): Promise<void> {
    await this.doRequest<void>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin/${encodeURIComponent(String(fileID))}/restore` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 彻底删除回收站文件 */
  async recycleDelete(vaultID: string, fileID: number): Promise<void> {
    await this.doRequest<void>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin/${encodeURIComponent(String(fileID))}/delete` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 列出当前用户在仓库中的协作关系*/
  async collabList(vaultID: string): Promise<{ collaborations: CollabEntry[] }> {
    return this.doRequest<{ collaborations: CollabEntry[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations`
    );
  }

  /** 列出当前用户跨仓库收到的协作关系*/
  async collabInbox(): Promise<{ collaborations: CollabEntry[] }> {
    return this.doRequest<{ collaborations: CollabEntry[] }>("GET", "/api/collaborations");
  }

  /** 邀请用户协作指定 Markdown 文件*/
  async collabInvite(
    vaultID: string,
    filePath: string,
    username: string,
    allowHistoryView = false
  ): Promise<CollabEntry> {
    return this.doRequest<CollabEntry>("POST", `/api/vaults/${encodeURIComponent(vaultID)}/collaborations`, {
      file_path: filePath,
      username,
      allow_history_view: allowHistoryView,
    });
  }

  /** 接受或拒绝协作邀请*/
  async collabRespond(vaultID: string, collabID: number, accept: boolean): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/respond`,
      { accept }
    );
  }

  /** 撤回邀请或解除协作（owner/manager）*/
  async collabRevoke(vaultID: string, collabID: number): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/revoke`
    );
  }

  /** 切换协作者对该文件修改记录的读取权限（owner/manager）*/
  async collabSetHistoryView(vaultID: string, collabID: number, allow: boolean): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/history-view`,
      { allow_history_view: allow }
    );
  }

  /** 协作者主动退出协作 */
  async collabLeave(vaultID: string, collabID: number): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/leave`
    );
  }

  /** 以协作者身份上传协作文件正文；baseRevision 与 operationID 先本地校验再发出，避免无意义请求 */
  async collabUpload(
    vaultID: string,
    fileID: number,
    input: CollaborationUploadInput
  ): Promise<SyncFileMeta> {
    const startedAt = Date.now();
    const hasBaseRevision = input.baseRevision !== undefined && input.baseRevision !== null;
    const hasOperationID = typeof input.operationID === "string" && input.operationID.length > 0;
    const baseRevisionValid = isValidServerRevision(input.baseRevision);
    const operationIDValid = isValidOperationID(input.operationID);
    this.diagnostics?.record({
      kind: "collab_upload_attempt",
      at: Date.now(),
      hasBaseRevision,
      baseRevisionValid,
      hasOperationID,
      operationIDValid,
      operationIDLength: typeof input.operationID === "string" ? input.operationID.length : 0,
    });
    if (!hasBaseRevision || !baseRevisionValid) {
      const code = !hasBaseRevision ? "missing_base_revision" : "invalid_base_revision";
      const err = new OSSApiError("base_revision is required", 400, undefined, code);
      this.diagnostics?.record({
        kind: "api_error",
        at: Date.now(),
        scope: "collab_upload",
        status: 400,
        code,
        reason: "invalid_collaboration_upload_state",
      });
      throw err;
    }
    if (!hasOperationID || !operationIDValid) {
      const raw = typeof input.operationID === "string" ? input.operationID.trim() : "";
      const code = raw === "" ? "missing_operation_id" : "invalid_operation_id";
      const err = new OSSApiError("operation_id is required", 400, undefined, code);
      this.diagnostics?.record({
        kind: "api_error",
        at: Date.now(),
        scope: "collab_upload",
        status: 400,
        code,
        reason: "invalid_collaboration_upload_state",
      });
      throw err;
    }
    try {
      return await this.doRequest<SyncFileMeta>(
        "POST",
        `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/files/${encodeURIComponent(String(fileID))}/upload`,
        {
          content: input.content,
          base_revision: input.baseRevision,
          operation_id: input.operationID,
        }
      );
    } catch (error) {
      if (error instanceof OSSApiError && error.status === 400) {
        this.diagnostics?.record({
          kind: "api_error",
          at: Date.now(),
          scope: "collab_upload",
          status: error.status,
          ...(error.code ? { code: error.code } : {}),
          reason: "invalid_collaboration_upload_state",
        });
      }
      throw error;
    } finally {
      this.diagnostics?.record({
        kind: "transfer",
        at: Date.now(),
        scope: "collab_upload",
        durationMs: Date.now() - startedAt,
        bytes: new TextEncoder().encode(input.content).byteLength,
      });
    }
  }

  /** 长轮询当前仓库的协作事件；changed 为 true 表示有新事件，version 用于下次 after 参数 */
  async collabPoll(
    vaultID: string,
    after: number,
    waitSeconds: number
  ): Promise<{ changed: boolean; version: number; vault_id: string }> {
    return this.doRequest<{ changed: boolean; version: number; vault_id: string }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/poll` +
        `?after=${encodeURIComponent(String(after))}` +
        `&wait=${encodeURIComponent(String(waitSeconds))}`
    );
  }

  /** 长轮询当前账户在所有仓库中的协作事件*/
  async collabAccountPoll(
    after: number,
    waitSeconds: number
  ): Promise<{ changed: boolean; version: number }> {
    return this.doRequest<{ changed: boolean; version: number }>(
      "GET",
      `/api/collaborations/poll?after=${encodeURIComponent(String(after))}` +
        `&wait=${encodeURIComponent(String(waitSeconds))}`
    );
  }

  /** EventSource 查询凭据只允许 HTTPS 或本机回环 HTTP*/
  collabEventStreamURL(vaultID: string): string | null {
    return this.buildCollabEventStreamURL(
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/stream`
    );
  }

  /** 构造当前账户的跨仓库协作事件流地址*/
  collabAccountEventStreamURL(): string | null {
    return this.buildCollabEventStreamURL("/api/collaborations/stream");
  }

  /**
   * 构造事件流地址
   * EventSource 无法携带 Authorization 头，令牌只能走查询参数；
   * 非 HTTPS 时仍放行，由使用方自行评估明文传输风险
   */
  private buildCollabEventStreamURL(path: string): string | null {
    if (!this.token) {
      return null;
    }
    try {
      new URL(this.settings.serverUrl);
    } catch {
      return null;
    }
    return this.url(
      `${path}?token=${encodeURIComponent(this.token)}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 下载协作文件正文；403/404/410 表示无权或已失效，返回 null 而不抛错 */
  async downloadCollabContent(
    vaultID: string,
    fileID: number
  ): Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null> {
    const startedAt = Date.now();
    let status: number | undefined;
    let bytes: number | undefined;
    try {
      const res = await httpRequest({
        url: this.url(
          `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/files/${encodeURIComponent(String(fileID))}/content`
        ),
        method: "GET",
        headers: this.authHeaders(),
      });
      status = res.status;
      if (res.status >= 400) {
        await this.parseResponse<never>(res.status, res.json, res.text);
      }
      bytes = res.arrayBuffer.byteLength;
      return {
        content: res.arrayBuffer,
        meta: {
          path: "",
          type: "markdown",
          hash: header(res.headers, "x-oss-hash"),
          size: res.arrayBuffer.byteLength,
          mtime: parseInt(header(res.headers, "x-oss-mtime") || "0", 10),
          revision: parseInt(header(res.headers, "x-oss-revision") || "0", 10),
          deleted: false,
        },
      };
    } catch (error) {
      if (
        error instanceof OSSApiError &&
        (error.status === 403 || error.status === 404 || error.status === 410)
      ) {
        return null;
      }
      throw error;
    } finally {
      this.recordDirectAPI("GET", status, startedAt);
      this.diagnostics?.record({
        kind: "transfer",
        at: Date.now(),
        scope: "collab_download",
        durationMs: Date.now() - startedAt,
        ...(bytes === undefined ? {} : { bytes }),
      });
    }
  }

  /** 调用旧版同步检查接口，并更新本地时钟偏移 */
  async check(files: CheckFileIn[], mode: "full" | "incremental"): Promise<CheckResponse> {
    const localBefore = Date.now();
    const res = await this.doRequest<CheckResponse>("POST", "/api/sync/check", {
      mode,
      files,
    });
    const localAfter = Date.now();
    // 用请求往返中点估算服务端时间对应的本地时刻
    const localMid = Math.floor((localBefore + localAfter) / 2);
    this.timeOffset = res.server_time - localMid;
    return res;
  }

  /** 旧版上传接口，使用 ArrayBuffer 发送原始文件内容 */
  async upload(path: string, adjustedMtime: number, content: ArrayBuffer): Promise<UploadResult> {
    const res = await httpRequest({
      url: this.url(
        `/api/sync/upload?path=${encodeURIComponent(path)}&mtime=${encodeURIComponent(String(adjustedMtime))}`
      ),
      method: "POST",
      headers: {
        "Content-Type": "application/octet-stream",
        ...this.authHeaders(),
      },
      body: content,
    });
    return res.json as UploadResult;
  }

  /** 旧版下载接口；mtime 与 hash 从响应头读取 */
  async download(path: string): Promise<{ content: ArrayBuffer; mtime: number; hash: string }> {
    const res = await httpRequest({
      url: this.url("/api/sync/download?path=" + encodeURIComponent(path)),
      method: "GET",
      headers: this.authHeaders(),
    });
    return {
      content: res.arrayBuffer,
      mtime: parseInt(res.headers["X-Oss-MTime"] || res.headers["x-oss-mtime"] || "0", 10),
      hash: res.headers["X-Oss-Hash"] || res.headers["x-oss-hash"] || "",
    };
  }

  /** 旧版删除接口 */
  async delete(path: string): Promise<void> {
    await this.doRequest<void>("POST", "/api/sync/delete", { path });
  }

  /** 创建分享；文件夹分享可携带同时生成的链接笔记 */
  async createShare(opts: {
    targetPath: string;
    isFolder: boolean;
    allowCopy: boolean;
    recursiveBacklinks: boolean;
  }): Promise<ShareCreateResult> {
    return this.doRequest<ShareCreateResult>("POST", "/api/shares", {
      vault_id: this.settings.vaultId,
      target_path: opts.targetPath,
      is_folder: opts.isFolder,
      allow_copy: opts.allowCopy,
      recursive_backlinks: opts.recursiveBacklinks,
    });
  }

  /** 列出当前仓库的分享 */
  async listShares(): Promise<{ shares: ShareOut[] }> {
    const query = this.settings.vaultId
      ? `?vault_id=${encodeURIComponent(this.settings.vaultId)}`
      : "";
    return this.doRequest<{ shares: ShareOut[] }>("GET", `/api/shares${query}`);
  }

  /** 切换分享是否允许复制 */
  async updateShareAllowCopy(shareID: string, allowCopy: boolean): Promise<ShareOut> {
    return this.doRequest<ShareOut>("PATCH", `/api/shares/${encodeURIComponent(shareID)}`, {
      allow_copy: allowCopy,
    });
  }

  /** 删除分享 */
  async deleteShare(shareID: string): Promise<void> {
    await this.doRequest<void>("DELETE", `/api/shares/${encodeURIComponent(shareID)}`);
  }

  /** 拼接完整请求地址，去掉 serverUrl 尾部斜杠避免双斜杠 */
  private url(path: string): string {
    return this.settings.serverUrl.replace(/\/$/, "") + path;
  }

  /** 构造请求头；设备名需编码以兼容非 ASCII 字符 */
  private authHeaders(): Record<string, string> {
    const headers: Record<string, string> = {
      "X-OSS-Client-ID": this.settings.clientId,
      "X-OSS-Device-Name": encodeURIComponent(this.settings.deviceName),
    };
    if (this.token) headers.Authorization = "Bearer " + this.token;
    return headers;
  }

  /** 统一 JSON 请求入口；失败时记录诊断后原样抛出 */
  private async doRequest<T>(
    method: "GET" | "POST" | "PATCH" | "DELETE",
    path: string,
    body?: unknown
  ): Promise<T> {
    const startedAt = Date.now();
    let status: number | undefined;
    try {
      const res = await httpRequest({
        url: this.url(path),
        method,
        headers: {
          "Content-Type": "application/json",
          ...this.authHeaders(),
        },
        body: body ? JSON.stringify(body) : undefined,
      });
      status = res.status;
      return this.parseResponse<T>(res.status, res.json, res.text);
    } catch (error) {
      if (error instanceof OSSApiError) status = error.status;
      this.diagnostics?.record({
        kind: "api_failed",
        at: Date.now(),
        method,
        path,
        ...(typeof status === "number" ? { status } : {}),
        message: error instanceof Error ? `${error.name}: ${error.message}` : String(error),
      });
      throw error;
    } finally {
      this.diagnostics?.record({ kind: "api", at: Date.now(), method, status, durationMs: Date.now() - startedAt });
    }
  }

  /** 记录绕过 doRequest 的二进制请求耗时 */
  private recordDirectAPI(
    method: "GET" | "POST",
    status: number | undefined,
    startedAt: number
  ): void {
    this.diagnostics?.record({ kind: "api", at: Date.now(), method, status, durationMs: Date.now() - startedAt });
  }

  /** 解析响应；4xx/5xx 时构造 OSSApiError 并透出服务端字段 */
  private async parseResponse<T>(status: number, json: unknown, text: string): Promise<T> {
    if (status >= 400) {
      // 服务端故障时可能返回 HTML 错误页，不能把 JSON 解析异常直接抛给用户
      let body: unknown = json;
      if (!isErrorBody(body)) {
        try {
          body = JSON.parse(text);
        } catch {
          body = null;
        }
      }
      const message = errorBodyMessage(body) ?? this.describeStatusFailure(status, text);
      const error = new OSSApiError(
        message,
        status,
        errorBodyField(body, "current"),
        errorBodyString(body, "code"),
        errorBodyNumber(body, "compacted_revision"),
        errorBodyNumber(body, "head_revision")
      );
      if (
        status === 401 &&
        this.token &&
        (error.code === "token_expired" || error.message.toLowerCase().includes("jwt token expired"))
      ) {
        this.token = null;
        await this.onTokenExpired?.();
      }
      throw error;
    }
    return json as T;
  }

  /** 把非 JSON 的失败响应描述成可读文本；HTML 错误页按状态码归类，其余透出服务端原因 */
  private describeStatusFailure(status: number, text: string): string {
    const trimmed = text.trim();
    const looksLikeHTML = trimmed.startsWith("<");
    // 非 HTML 的正文通常是服务端写的可读原因，优先透出
    if (trimmed.length > 0 && !looksLikeHTML) return trimmed.slice(0, 200);
    if (status >= 500) return `服务器内部错误（HTTP ${status}）`;
    if (status === 404) return `接口不存在（HTTP 404）`;
    if (status === 401 || status === 403) return `未授权或登录已失效（HTTP ${status}）`;
    return `HTTP ${status}`;
  }

  /** 用请求往返中点估算服务端时间对应的本地时刻，据此更新时钟偏移 */
  private updateTimeOffset(serverTime: number, before: number, after: number): void {
    if (!serverTime) return;
    this.timeOffset = serverTime - Math.floor((before + after) / 2);
  }
}

/** 大小写不敏感地读取响应头 */
function header(headers: Record<string, string>, name: string): string {
  const target = name.toLowerCase();
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === target) return value;
  }
  return "";
}

// 错误响应体的字段访问全部经过类型守卫，外部响应不可信
/** 错误响应体必须是普通对象，数组不可作为错误体 */
function isErrorBody(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** 读取错误响应体的 error 字段，空白文本视为缺失 */
function errorBodyMessage(body: unknown): string | undefined {
  if (!isErrorBody(body)) return undefined;
  const message = body.error;
  return typeof message === "string" && message.trim() !== "" ? message : undefined;
}

/** 读取错误响应体的字符串字段 */
function errorBodyString(body: unknown, field: string): string | undefined {
  if (!isErrorBody(body)) return undefined;
  const value = body[field];
  return typeof value === "string" ? value : undefined;
}

/** 读取错误响应体的数字字段 */
function errorBodyNumber(body: unknown, field: string): number | undefined {
  if (!isErrorBody(body)) return undefined;
  const value = body[field];
  return typeof value === "number" ? value : undefined;
}

/** 读取错误响应体中的 current 文件元信息；字段缺失时由调用方兜底 */
function errorBodyField(body: unknown, field: string): SyncFileMeta | undefined {
  if (!isErrorBody(body)) return undefined;
  return body[field] as SyncFileMeta | undefined;
}
