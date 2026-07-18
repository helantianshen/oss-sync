// OSS 后端 HTTP 客户端。
//
// JWT 由插件通过 loadData()/saveData() 持久化。
// 失效或登录失败时抛错，由调用方决定 UI 提示。

import { requestUrl } from "obsidian";
import type { OSSSettings } from "./settings";

export interface AuthResponse {
  token: string;
  expires_in: number;
  user_id: number;
  username: string;
  role: string;
}

export interface AuthStatus {
  readonly needs_first_admin: boolean;
  readonly registration_mode: "first_admin" | "anonymous" | "admin_only";
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

export interface SyncManifestResponse {
  snapshot_revision: number;
  compacted_revision: number;
  next_cursor: number;
  has_more: boolean;
  recovery_snapshot: boolean;
  server_time: number;
  files: SyncFileMeta[];
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

export class OSSApiClient {
  /** 服务端时间与本地时间的偏移，单位为毫秒。 */
  private timeOffset = 0;
  private token: string | null = null;

  constructor(private settings: OSSSettings) {}

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

  async register(): Promise<AuthResponse> {
    const res = await this.doRequest<AuthResponse>("POST", "/api/auth/register", {
      username: this.settings.username,
      password: this.settings.password,
      role: "user",
    });
    this.token = res.token;
    return res;
  }

  async authStatus(): Promise<AuthStatus> {
    return this.doRequest<AuthStatus>("GET", "/api/auth/status");
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

  async updateVault(vaultID: string, input: { name?: string; description?: string }): Promise<VaultOut> {
    return this.doRequest<VaultOut>(
      "PATCH",
      `/api/vaults/${encodeURIComponent(vaultID)}`,
      input
    );
  }

  async archiveVault(vaultID: string): Promise<void> {
    await this.doRequest<void>("DELETE", `/api/vaults/${encodeURIComponent(vaultID)}`);
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
    const query = new URLSearchParams({
      path: input.path,
      base_revision: String(input.baseRevision),
      hash: input.hash,
      mtime: String(input.mtime),
      client_id: this.settings.clientId,
      operation_id: input.operationID,
    });
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
    return this.parseResponse<SyncFileMeta>(res.status, res.json, res.text);
  }

  async downloadV2(
    vaultID: string,
    path: string,
    revision: number
  ): Promise<{ content: ArrayBuffer; meta: SyncFileMeta }> {
    const query = new URLSearchParams({ path, revision: String(revision) });
    const res = await requestUrl({
      url: this.url(`/api/vaults/${encodeURIComponent(vaultID)}/sync/download?${query.toString()}`),
      method: "GET",
      headers: this.authHeaders(),
      throw: false,
    });
    if (res.status >= 400) {
      this.parseResponse<never>(res.status, res.json, res.text);
    }
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

  private async doRequest<T>(method: string, path: string, body?: unknown): Promise<T> {
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
    return this.parseResponse<T>(res.status, res.json, res.text);
  }

  private parseResponse<T>(status: number, json: any, text: string): T {
    if (status >= 400) {
      let body: any = json;
      if (!body || typeof body !== "object") {
        try {
          body = JSON.parse(text);
        } catch {
          body = null;
        }
      }
      throw new OSSApiError(
        body?.error || `HTTP ${status}`,
        status,
        body?.current,
        body?.code,
        body?.compacted_revision,
        body?.head_revision
      );
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

function classifyPath(path: string): "markdown" | "attachment" | "config" {
  const lower = path.toLowerCase();
  if (lower.endsWith(".md")) return "markdown";
  if (lower.startsWith(".obsidian/")) return "config";
  return "attachment";
}
