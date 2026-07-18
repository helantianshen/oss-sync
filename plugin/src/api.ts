// HTTP 客户端 — 调用 OSS 后端的 /api/auth/* 与 /api/sync/*。
//
// JWT 持久化：存到 plugin loadData()（Obsidian 官方推荐的非敏感存储）。
// 失效或登录失败时抛错，由调用方决定 UI 提示。
//
// 决策 7.1：check 响应含 server_time，客户端缓存 time_offset。
// 上传时 mtime = 本地真实 mtime + time_offset，强行以服务器时间为基准。

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
  readonly registration_mode: "first_admin" | "admin_only";
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

export class OSSApiClient {
  /** 决策 7.1：服务端时间 - 本地时间 的偏移（毫秒）。每次 check 刷新。 */
  private timeOffset = 0;
  private token: string | null = null;

  constructor(private settings: OSSSettings) {}

  setToken(token: string | null): void {
    this.token = token;
  }

  hasToken(): boolean {
    return !!this.token;
  }

  /** 决策 7.1：上传/下载时用于校准的本地 mtime。 */
  getAdjustedMtime(localMtimeMs: number): number {
    return localMtimeMs + this.timeOffset;
  }

  /** 决策 7.1：|offset| 超过 5 分钟时返回 true，UI 应警告。 */
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

  /**
   * 调用 /api/sync/check。
   * 决策 7.1：响应中的 server_time 用于刷新 timeOffset。
   * 决策 6.5：mode 字段 incremental/full 由 settings 控制。
   */
  async check(files: CheckFileIn[], mode: "full" | "incremental"): Promise<CheckResponse> {
    const localBefore = Date.now();
    const res = await this.doRequest<CheckResponse>("POST", "/api/sync/check", {
      mode,
      files,
    });
    const localAfter = Date.now();
    // 决策 7.1：用「网络往返中点」近似 server_time 对应的本地时刻。
    // 这是工程化的近似，足够纠正分钟级漂移。
    const localMid = Math.floor((localBefore + localAfter) / 2);
    this.timeOffset = res.server_time - localMid;
    return res;
  }

  /** 流式上传 — requestUrl 仅支持 string/ArrayBuffer，因此直接发送原始字节。 */
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

  /** 流式下载 — 决策 7.3：直接拿 ArrayBuffer。Phase 5 大文件场景需流式重写。 */
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
      target_path: opts.targetPath,
      is_folder: opts.isFolder,
      allow_copy: opts.allowCopy,
      recursive_backlinks: opts.recursiveBacklinks,
    });
  }

  async listShares(): Promise<{ shares: ShareOut[] }> {
    return this.doRequest<{ shares: ShareOut[] }>("GET", "/api/shares");
  }

  async deleteShare(shareID: string): Promise<void> {
    await this.doRequest<void>("DELETE", `/api/shares/${encodeURIComponent(shareID)}`);
  }

  private url(path: string): string {
    return this.settings.serverUrl.replace(/\/$/, "") + path;
  }

  private authHeaders(): Record<string, string> {
    return this.token ? { Authorization: "Bearer " + this.token } : {};
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
    });
    if (res.status >= 400) {
      let msg = `HTTP ${res.status}`;
      try {
        const j = typeof res.json === "object" ? res.json : JSON.parse(res.text);
        if (j && j.error) msg = j.error;
      } catch {
        /* ignore */
      }
      throw new Error(msg);
    }
    return res.json as T;
  }
}
