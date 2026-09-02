// OSS 后端 HTTP 客户端。
//
// JWT 由插件通过 loadData()/saveData() 持久化。
// 失效或登录失败时抛错，由调用方决定 UI 提示。

import { requestUrl } from "obsidian";
import type { Diagnostics } from "./diagnostics";
import type { OSSSettings } from "./settings";
import { isValidOperationID, isValidServerRevision } from "./operation-id.js";

export interface AuthResponse {
  token: string;
  expires_in: number;
  user_id: number;
  username: string;
  role: string;
  device_status?: "pending" | "approved" | "revoked";
  device_name?: string;
}

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

export interface UploadResult {
  path: string;
  hash: string;
  mtime: number;
  server_time: number;
}

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

export interface ShareCreateResult {
  share_id: string;
  url: string;
  target_path: string;
  is_folder: boolean;
  extra?: ShareOut[];
}

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

export interface DeviceVaultOut {
  vault_id: string;
  vault_name: string;
  last_cursor: number;
  head_revision: number;
  pending_changes: number;
  last_sync_at?: string;
}

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

export interface CollaborationUploadInput {
  readonly content: string;
  readonly baseRevision: number;
  readonly operationID: string;
}

export interface SyncManifestResponse {
  snapshot_revision: number;
  compacted_revision: number;
  next_cursor: number;
  has_more: boolean;
  recovery_snapshot: boolean;
  server_time: number;
  files: SyncFileMeta[];
}

export type SyncMode = "short_poll" | "long_poll";

export interface SyncStrategyResponse {
  policy: string;
  effective_mode: SyncMode;
  min_debounce_sec: number;
  long_poll_wait_sec: number;
}

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

export interface HistoryDetail extends HistoryEntry {
  content?: string;
  diff: string[];
  is_text: boolean;
}

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
  created_at: string;
}

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

export interface ServerVersionInfo {
  readonly version: string;
  readonly env: string;
  readonly commit?: string;
  readonly built_at?: string;
}

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

export interface ServerUpdateCheckResponse {
  readonly check_id: string;
  readonly candidate: ServerUpdateCandidate;
  readonly current_version: string;
  readonly latest_version: string;
  readonly update_available: boolean;
  readonly release_url: string;
  readonly expires_at: number;
}

export interface ServerUpdateOperation {
  readonly id: string;
  readonly state: string;
  readonly version: string;
  readonly started_at_unix: number;
  readonly updated_at_unix: number;
  readonly error?: string;
}

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

export interface ServerUpdateTriggerResponse {
  readonly ok: boolean;
  readonly code: string;
  readonly operation?: ServerUpdateOperation;
  readonly version?: string;
  readonly error?: string;
}

export class OSSApiClient {
  /** 服务端时间与本地时间的偏移，单位为毫秒。 */
  private timeOffset = 0;
  private token: string | null = null;

  constructor(
    private settings: OSSSettings,
    private readonly diagnostics?: Diagnostics,
    private readonly onTokenExpired?: () => Promise<void>
  ) {}

  setToken(token: string | null): void {
    this.token = token;
  }

  hasToken(): boolean {
    return !!this.token;
  }

  getAdjustedMtime(localMtimeMs: number): number {
    return localMtimeMs + this.timeOffset;
  }

  isClockDriftLarge(): boolean {
    return Math.abs(this.timeOffset) > 5 * 60 * 1000;
  }

  getTimeOffset(): number {
    return this.timeOffset;
  }

  async authStatus(): Promise<AuthStatus> {
    return this.doRequest<AuthStatus>("GET", "/api/auth/status");
  }

  /** 校验当前令牌是否有管理员权限（服务端 /api/admin 拒绝非管理员）。 */
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

  async getServerVersion(): Promise<ServerVersionInfo> {
    return this.doRequest<ServerVersionInfo>("GET", "/api/admin/version");
  }

  async checkServerUpdate(): Promise<ServerUpdateCheckResponse> {
    return this.doRequest<ServerUpdateCheckResponse>("GET", "/api/admin/update/check");
  }

  async getServerUpdateStatus(): Promise<ServerUpdateStatusResponse> {
    return this.doRequest<ServerUpdateStatusResponse>("GET", "/api/admin/update/status");
  }

  async triggerServerUpdate(opts: { readonly checkId: string; readonly expectedVersion: string }): Promise<ServerUpdateTriggerResponse> {
    return this.doRequest<ServerUpdateTriggerResponse>("POST", "/api/admin/update", {
      check_id: opts.checkId,
      expected_version: opts.expectedVersion,
    });
  }

  async login(): Promise<AuthResponse> {
    const res = await this.doRequest<AuthResponse>("POST", "/api/auth/login", {
      username: this.settings.username,
      password: this.settings.password,
    });
    this.token = res.token;
    return res;
  }

  async listVaults(): Promise<{ vaults: VaultOut[] }> {
    return this.doRequest<{ vaults: VaultOut[] }>("GET", "/api/vaults");
  }

  async createVault(name: string): Promise<VaultOut> {
    return this.doRequest<VaultOut>("POST", "/api/vaults", { name });
  }


  async listDevices(): Promise<{ devices: DeviceOut[]; stale_after_days: number }> {
    return this.doRequest<{ devices: DeviceOut[]; stale_after_days: number }>(
      "GET",
      "/api/devices"
    );
  }

  async renameDevice(clientID: string, name: string): Promise<void> {
    await this.doRequest<void>(
      "PATCH",
      `/api/devices/${encodeURIComponent(clientID)}`,
      { name }
    );
  }

  async revokeDevice(clientID: string): Promise<void> {
    await this.doRequest<void>("DELETE", `/api/devices/${encodeURIComponent(clientID)}`);
  }

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
      const res = await requestUrl({
        url: this.url(`/api/vaults/${encodeURIComponent(vaultID)}/sync/upload?${query.toString()}`),
        method: "POST",
        headers: {
          "Content-Type": "application/octet-stream",
          ...this.authHeaders(),
        },
        body: input.content,
        throw: false,
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
      const res = await requestUrl({
        url: this.url(`/api/vaults/${encodeURIComponent(vaultID)}/sync/download?${query.toString()}`),
        method: "GET",
        headers: this.authHeaders(),
        throw: false,
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
          type: classifyPath(path),
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

  /** 获取仓库同步策略，effective_mode 由服务端根据仓库策略与客户端偏好计算。 */
  async syncStrategy(vaultID: string, mode: SyncMode): Promise<SyncStrategyResponse> {
    return this.doRequest<SyncStrategyResponse>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/strategy` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}` +
        `&mode=${encodeURIComponent(mode)}`
    );
  }

  /** 查询指定路径的修改历史。 */
  async history(vaultID: string, path: string): Promise<{ history: HistoryEntry[] }> {
    return this.doRequest<{ history: HistoryEntry[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/history` +
        `?path=${encodeURIComponent(path)}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

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

  async historyRestore(vaultID: string, historyID: number): Promise<{ path: string }> {
    return this.doRequest<{ path: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/sync/history/${encodeURIComponent(String(historyID))}/restore` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  async recycleList(vaultID: string): Promise<{ files: RecycleBinFile[] }> {
    return this.doRequest<{ files: RecycleBinFile[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  async recycleRestore(vaultID: string, fileID: number): Promise<void> {
    await this.doRequest<void>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin/${encodeURIComponent(String(fileID))}/restore` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  async recycleDelete(vaultID: string, fileID: number): Promise<void> {
    await this.doRequest<void>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/recycle-bin/${encodeURIComponent(String(fileID))}/delete` +
        `?client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 列出当前用户在仓库中的协作关系。 */
  async collabList(vaultID: string): Promise<{ collaborations: CollabEntry[] }> {
    return this.doRequest<{ collaborations: CollabEntry[] }>(
      "GET",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations`
    );
  }

  /** 列出当前用户跨仓库收到的协作关系。 */
  async collabInbox(): Promise<{ collaborations: CollabEntry[] }> {
    return this.doRequest<{ collaborations: CollabEntry[] }>("GET", "/api/collaborations");
  }

  /** 邀请用户协作指定 Markdown 文件。 */
  async collabInvite(
    vaultID: string,
    filePath: string,
    username: string
  ): Promise<CollabEntry> {
    return this.doRequest<CollabEntry>("POST", `/api/vaults/${encodeURIComponent(vaultID)}/collaborations`, {
      file_path: filePath,
      username,
    });
  }

  /** 接受或拒绝协作邀请。 */
  async collabRespond(vaultID: string, collabID: number, accept: boolean): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/respond`,
      { accept }
    );
  }

  /** 撤回邀请或解除协作（owner/manager）。 */
  async collabRevoke(vaultID: string, collabID: number): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/revoke`
    );
  }

  async collabLeave(vaultID: string, collabID: number): Promise<{ status: string }> {
    return this.doRequest<{ status: string }>(
      "POST",
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/${encodeURIComponent(String(collabID))}/leave`
    );
  }

  /** 以协作者身份上传协作文件正文。 */
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

  /** 长轮询协作事件：changed 为 true 表示有新事件，version 用于下次 after 参数。 */
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

  /** 长轮询当前账户在所有仓库中的协作事件。 */
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

  /** EventSource 查询凭据只允许 HTTPS 或本机回环 HTTP。 */
  collabEventStreamURL(vaultID: string): string | null {
    return this.buildCollabEventStreamURL(
      `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/stream`
    );
  }

  /** 构造当前账户的跨仓库协作事件流地址。 */
  collabAccountEventStreamURL(): string | null {
    return this.buildCollabEventStreamURL("/api/collaborations/stream");
  }

  private buildCollabEventStreamURL(path: string): string | null {
    if (!this.token) {
      return null;
    }
    let serverURL: URL;
    try {
      serverURL = new URL(this.settings.serverUrl);
    } catch {
      return null;
    }
    const secure = serverURL.protocol === "https:";
    const loopbackHTTP = serverURL.protocol === "http:" && isLoopbackHostname(serverURL.hostname);
    if (!secure && !loopbackHTTP) return null;
    return this.url(
      `${path}?token=${encodeURIComponent(this.token)}` +
        `&client_id=${encodeURIComponent(this.settings.clientId)}`
    );
  }

  /** 下载协作文件正文；协作者不需要仓库成员或设备仓库授权。 */
  async downloadCollabContent(
    vaultID: string,
    fileID: number
  ): Promise<{ content: ArrayBuffer; meta: SyncFileMeta } | null> {
    const startedAt = Date.now();
    let status: number | undefined;
    let bytes: number | undefined;
    try {
      const res = await requestUrl({
        url: this.url(
          `/api/vaults/${encodeURIComponent(vaultID)}/collaborations/files/${encodeURIComponent(String(fileID))}/content`
        ),
        method: "GET",
        headers: this.authHeaders(),
        throw: false,
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

  /** 调用旧版同步检查接口，并更新本地时钟偏移。 */
  async check(files: CheckFileIn[], mode: "full" | "incremental"): Promise<CheckResponse> {
    const localBefore = Date.now();
    const res = await this.doRequest<CheckResponse>("POST", "/api/sync/check", {
      mode,
      files,
    });
    const localAfter = Date.now();
    // 用请求往返中点估算服务端时间对应的本地时刻。
    const localMid = Math.floor((localBefore + localAfter) / 2);
    this.timeOffset = res.server_time - localMid;
    return res;
  }

  /** requestUrl 使用 ArrayBuffer 发送原始文件内容。 */
  async upload(path: string, adjustedMtime: number, content: ArrayBuffer): Promise<UploadResult> {
    const res = await requestUrl({
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

  async download(path: string): Promise<{ content: ArrayBuffer; mtime: number; hash: string }> {
    const res = await requestUrl({
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

  async delete(path: string): Promise<void> {
    await this.doRequest<void>("POST", "/api/sync/delete", { path });
  }

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

  async listShares(): Promise<{ shares: ShareOut[] }> {
    const query = this.settings.vaultId
      ? `?vault_id=${encodeURIComponent(this.settings.vaultId)}`
      : "";
    return this.doRequest<{ shares: ShareOut[] }>("GET", `/api/shares${query}`);
  }

  async updateShareAllowCopy(shareID: string, allowCopy: boolean): Promise<ShareOut> {
    return this.doRequest<ShareOut>("PATCH", `/api/shares/${encodeURIComponent(shareID)}`, {
      allow_copy: allowCopy,
    });
  }

  async deleteShare(shareID: string): Promise<void> {
    await this.doRequest<void>("DELETE", `/api/shares/${encodeURIComponent(shareID)}`);
  }

  private url(path: string): string {
    return this.settings.serverUrl.replace(/\/$/, "") + path;
  }

  private authHeaders(): Record<string, string> {
    const headers: Record<string, string> = {
      "X-OSS-Client-ID": this.settings.clientId,
      "X-OSS-Device-Name": encodeURIComponent(this.settings.deviceName),
    };
    if (this.token) headers.Authorization = "Bearer " + this.token;
    return headers;
  }

  private async doRequest<T>(
    method: "GET" | "POST" | "PATCH" | "DELETE",
    path: string,
    body?: unknown
  ): Promise<T> {
    const startedAt = Date.now();
    let status: number | undefined;
    try {
      const res = await requestUrl({
        url: this.url(path),
        method: method as any,
        headers: {
          "Content-Type": "application/json",
          ...this.authHeaders(),
        },
        body: body ? JSON.stringify(body) : undefined,
        throw: false,
      });
      status = res.status;
      return this.parseResponse<T>(res.status, res.json, res.text);
    } catch (error) {
      if (error instanceof OSSApiError) status = error.status;
      throw error;
    } finally {
      this.diagnostics?.record({ kind: "api", at: Date.now(), method, status, durationMs: Date.now() - startedAt });
    }
  }

  private recordDirectAPI(
    method: "GET" | "POST",
    status: number | undefined,
    startedAt: number
  ): void {
    this.diagnostics?.record({ kind: "api", at: Date.now(), method, status, durationMs: Date.now() - startedAt });
  }

  private async parseResponse<T>(status: number, json: any, text: string): Promise<T> {
    if (status >= 400) {
      let body: any = json;
      if (!body || typeof body !== "object") {
        try {
          body = JSON.parse(text);
        } catch {
          body = null;
        }
      }
      const error = new OSSApiError(
        body?.error || `HTTP ${status}`,
        status,
        body?.current,
        body?.code,
        body?.compacted_revision,
        body?.head_revision
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

  private updateTimeOffset(serverTime: number, before: number, after: number): void {
    if (!serverTime) return;
    this.timeOffset = serverTime - Math.floor((before + after) / 2);
  }
}

function header(headers: Record<string, string>, name: string): string {
  const target = name.toLowerCase();
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === target) return value;
  }
  return "";
}

export function isLoopbackHostname(hostname: string): boolean {
  const normalized = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  if (normalized === "localhost" || normalized === "::1" || normalized === "0:0:0:0:0:0:0:1") {
    return true;
  }
  return /^127(?:\.\d{1,3}){3}$/.test(normalized);
}

function classifyPath(path: string): "markdown" | "attachment" | "config" {
  const lower = path.toLowerCase();
  if (lower.endsWith(".md")) return "markdown";
  if (lower.startsWith(".obsidian/")) return "config";
  return "attachment";
}
