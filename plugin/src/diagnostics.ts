import { redactDiagnosticText, sanitizeDiagnosticDetails, type DiagnosticDetails } from "./diagnostic-report";

export type DiagnosticEvent =
  | { readonly kind: "detail"; readonly at: number; readonly area: "transport" | "download" | "sync_action" | "sync_run"; readonly stage: string; readonly details: DiagnosticDetails }
  | {
      readonly kind: "plugin_version_check";
      readonly at: number;
      readonly result: "update_available" | "up_to_date" | "failed";
      readonly currentVersion: string;
      readonly remoteVersion?: string;
    }
  | {
      readonly kind: "sse_state";
      readonly at: number;
      readonly state: "connecting" | "open" | "closed" | "failed";
      readonly reason?: "forced" | "fallback";
    }
  | {
      readonly kind: "sse_event";
      readonly at: number;
      readonly event: "changed" | "revoked" | "invited";
      readonly connectionAgeMs: number;
    }
  | {
      readonly kind: "poll";
      readonly at: number;
      readonly scope: "sync" | "collab";
      readonly changed?: boolean;
      readonly version?: number;
      readonly durationMs: number;
      readonly failed?: true;
    }
  | {
      readonly kind: "api";
      readonly at: number;
      readonly method: "GET" | "POST" | "PATCH" | "DELETE";
      readonly status?: number;
      readonly durationMs: number;
    }
  | {
      readonly kind: "api_failed";
      readonly at: number;
      readonly method: "GET" | "POST" | "PATCH" | "DELETE";
      readonly path: string;
      readonly status?: number;
      readonly message: string;
    }
  | {
      readonly kind: "transfer";
      readonly at: number;
      readonly scope: "upload" | "download" | "collab_upload" | "collab_download";
      readonly durationMs: number;
      readonly bytes?: number;
    }
  | { readonly kind: "collab_activity"; readonly at: number; readonly entries: number; readonly newestCreatedAt?: string }
  | {
      readonly kind: "api_error";
      readonly at: number;
      readonly scope: "collab_upload";
      readonly status: number;
      readonly code?: string;
      readonly reason?: string;
    }
  | {
      readonly kind: "runtime_info";
      readonly at: number;
      readonly pluginVersion: string;
      readonly buildCommit?: string;
      readonly buildTime?: string;
      readonly serverVersion?: string;
      readonly diagnosticsEnabled: boolean;
    }
  | {
      readonly kind: "diagnostics_enabled";
      readonly at: number;
      readonly enabled: boolean;
    }
  | {
      readonly kind: "collab_upload_attempt";
      readonly at: number;
      readonly hasBaseRevision: boolean;
      readonly baseRevisionValid: boolean;
      readonly hasOperationID: boolean;
      readonly operationIDValid: boolean;
      readonly operationIDLength: number;
    }
  | {
      readonly kind: "local_discovery";
      readonly at: number;
      readonly reason: "event_batch" | "collaboration_boundary" | "paths" | "content" | "incomplete" | "delete_deferred" | "event_suppressed" | "event_filtered" | "rename_applied" | "rename_fallback" | "rename_conflict";
      readonly count: number;
      readonly operationID?: string;
      readonly durationMs?: number;
    }
  | {
      readonly kind: "sync_queue";
      readonly at: number;
      readonly phase: "persisted" | "resumed";
      readonly operation: "upsert" | "delete" | "rename";
      readonly pendingCount: number;
    }
  | {
      readonly kind: "sync_run";
      readonly at: number;
      readonly phase: "start" | "remote" | "complete" | "failed";
      readonly forceFull: boolean;
      readonly cursor?: number;
      readonly nextCursor?: number;
      readonly remoteFiles?: number;
      readonly pendingCount?: number;
      readonly actionCount?: number;
      readonly resolvedCount?: number;
    };

/** 仅保留脱敏后的有限事件记录，避免长期持有正文和令牌 */
export class Diagnostics {
  private readonly events: DiagnosticEvent[] = [];
  private readonly failures: DiagnosticEvent[] = [];

  constructor(
    private readonly sink?: (event: DiagnosticEvent) => void,
    private readonly capacity = 2000,
    private readonly isEnabled: () => boolean = () => true,
  ) {}

  /** 记录事件；字段不合法时丢弃，超出容量时淘汰最旧一条 */
  record(event: DiagnosticEvent): void {
    if (!this.isEnabled()) return;
    const safeEvent = sanitizeEvent(event);
    if (!safeEvent) return;
    this.events.push(Object.freeze(safeEvent));
    if (this.events.length > this.capacity) this.events.shift();
    if (safeEvent.kind === "api_failed" || safeEvent.kind === "api_error"
      || (safeEvent.kind === "sync_run" && safeEvent.phase === "failed")
      || (safeEvent.kind === "detail" && /fail|error/.test(safeEvent.stage))) {
      this.failures.push(safeEvent);
      if (this.failures.length > 200) this.failures.shift();
    }
    try { this.sink?.(safeEvent); } catch { /* 诊断输出不得影响同步。 */ }
  }

  /** 返回事件快照副本，调用方无法改动内部缓冲 */
  snapshot(): readonly DiagnosticEvent[] {
    return [...this.events];
  }

  failureSnapshot(): readonly DiagnosticEvent[] {
    return [...this.failures];
  }

  /** 清空全部记录 */
  clear(): void {
    this.events.length = 0;
    this.failures.length = 0;
  }
}

/** 按事件类型逐项校验并脱敏，返回 null 表示该事件应丢弃 */
function sanitizeEvent(event: DiagnosticEvent): DiagnosticEvent | null {
  switch (event.kind) {
    case "detail":
      return { kind: "detail", at: finiteNumber(event.at), area: event.area, stage: event.stage.slice(0, 80), details: sanitizeDiagnosticDetails(event.details) };
    case "local_discovery":
      if (!["event_batch", "collaboration_boundary", "paths", "content", "incomplete", "delete_deferred", "event_suppressed", "event_filtered", "rename_applied", "rename_fallback", "rename_conflict"].includes(event.reason)) return null;
      return { kind: "local_discovery", at: finiteNumber(event.at), reason: event.reason, count: finiteNumber(event.count),
        ...(typeof event.operationID === "string" ? { operationID: event.operationID.slice(0, 128) } : {}),
        ...(typeof event.durationMs === "number" ? { durationMs: finiteNumber(event.durationMs) } : {}) };
    case "plugin_version_check":
      if (!["update_available", "up_to_date", "failed"].includes(event.result) || typeof event.currentVersion !== "string") return null;
      return {
        kind: "plugin_version_check", at: finiteNumber(event.at), result: event.result,
        currentVersion: event.currentVersion.slice(0, 64),
        ...(typeof event.remoteVersion === "string" ? { remoteVersion: event.remoteVersion.slice(0, 64) } : {}),
      };
    case "sse_state":
      if (!isSSEState(event.state)) return null;
      return event.reason === "forced" || event.reason === "fallback"
        ? { kind: "sse_state", at: finiteNumber(event.at), state: event.state, reason: event.reason }
        : { kind: "sse_state", at: finiteNumber(event.at), state: event.state };
    case "sse_event":
      if (!isSSEEvent(event.event)) return null;
      return {
        kind: "sse_event",
        at: finiteNumber(event.at),
        event: event.event,
        connectionAgeMs: finiteNumber(event.connectionAgeMs),
      };
    case "poll":
      if (event.scope !== "sync" && event.scope !== "collab") return null;
      return {
        kind: "poll",
        at: finiteNumber(event.at),
        scope: event.scope,
        ...(typeof event.changed === "boolean" ? { changed: event.changed } : {}),
        ...(typeof event.version === "number" ? { version: finiteNumber(event.version) } : {}),
        durationMs: finiteNumber(event.durationMs),
        ...(event.failed === true ? { failed: true } : {}),
      };
    case "api":
      if (!isHTTPMethod(event.method)) return null;
      return {
        kind: "api",
        at: finiteNumber(event.at),
        method: event.method,
        ...(typeof event.status === "number" ? { status: finiteNumber(event.status) } : {}),
        durationMs: finiteNumber(event.durationMs),
      };
    case "api_failed":
      if (!isHTTPMethod(event.method)) return null;
      return {
        kind: "api_failed",
        at: finiteNumber(event.at),
        method: event.method,
        path: truncate(event.path, 200),
        ...(typeof event.status === "number" ? { status: finiteNumber(event.status) } : {}),
        message: truncate(redactDiagnosticText(/Unexpected token|not valid JSON|JSON\.parse|JSON Parse error/i.test(event.message) ? "JSON decoding failed; response content omitted" : event.message), 300),
      };
    case "transfer":
      if (!isTransferScope(event.scope)) return null;
      return {
        kind: "transfer",
        at: finiteNumber(event.at),
        scope: event.scope,
        durationMs: finiteNumber(event.durationMs),
        ...(typeof event.bytes === "number" ? { bytes: finiteNumber(event.bytes) } : {}),
      };
    case "collab_activity":
      return {
        kind: "collab_activity",
        at: finiteNumber(event.at),
        entries: finiteNumber(event.entries),
        ...(normalizedTimestamp(event.newestCreatedAt) ? { newestCreatedAt: normalizedTimestamp(event.newestCreatedAt) } : {}),
      };
    case "api_error": {
      if (event.scope !== "collab_upload") return null;
      const status = finiteNumber(event.status);
      if (!Number.isFinite(status)) return null;
      return {
        kind: "api_error",
        at: finiteNumber(event.at),
        scope: event.scope,
        status,
        ...(typeof event.code === "string" && event.code ? { code: event.code } : {}),
        ...(typeof event.reason === "string" && event.reason ? { reason: event.reason } : {}),
      };
    }
    case "runtime_info": {
      if (typeof event.pluginVersion !== "string" || !event.pluginVersion) return null;
      return {
        kind: "runtime_info",
        at: finiteNumber(event.at),
        pluginVersion: event.pluginVersion.slice(0, 64),
        ...(typeof event.buildCommit === "string" && event.buildCommit ? { buildCommit: event.buildCommit.slice(0, 64) } : {}),
        ...(typeof event.buildTime === "string" && event.buildTime ? { buildTime: event.buildTime.slice(0, 64) } : {}),
        ...(typeof event.serverVersion === "string" && event.serverVersion ? { serverVersion: event.serverVersion.slice(0, 64) } : {}),
        diagnosticsEnabled: !!event.diagnosticsEnabled,
      };
    }
    case "diagnostics_enabled": {
      return {
        kind: "diagnostics_enabled",
        at: finiteNumber(event.at),
        enabled: !!event.enabled,
      };
    }
    case "collab_upload_attempt": {
      return {
        kind: "collab_upload_attempt",
        at: finiteNumber(event.at),
        hasBaseRevision: !!event.hasBaseRevision,
        baseRevisionValid: !!event.baseRevisionValid,
        hasOperationID: !!event.hasOperationID,
        operationIDValid: !!event.operationIDValid,
        operationIDLength: finiteNumber(event.operationIDLength),
      };
    }
    case "sync_queue": {
      if (event.phase !== "persisted" && event.phase !== "resumed") return null;
      if (event.operation !== "upsert" && event.operation !== "delete" && event.operation !== "rename") return null;
      return {
        kind: "sync_queue",
        at: finiteNumber(event.at),
        phase: event.phase,
        operation: event.operation,
        pendingCount: finiteNumber(event.pendingCount),
      };
    }
    case "sync_run": {
      if (event.phase !== "start" && event.phase !== "remote" && event.phase !== "complete" && event.phase !== "failed") return null;
      return {
        kind: "sync_run",
        at: finiteNumber(event.at),
        phase: event.phase,
        forceFull: !!event.forceFull,
        ...(typeof event.cursor === "number" ? { cursor: finiteNumber(event.cursor) } : {}),
        ...(typeof event.nextCursor === "number" ? { nextCursor: finiteNumber(event.nextCursor) } : {}),
        ...(typeof event.remoteFiles === "number" ? { remoteFiles: finiteNumber(event.remoteFiles) } : {}),
        ...(typeof event.pendingCount === "number" ? { pendingCount: finiteNumber(event.pendingCount) } : {}),
        ...(typeof event.actionCount === "number" ? { actionCount: finiteNumber(event.actionCount) } : {}),
        ...(typeof event.resolvedCount === "number" ? { resolvedCount: finiteNumber(event.resolvedCount) } : {}),
      };
    }
  }
}

/** 非有限数归一为 0，保证记录中不出现 NaN 或 Infinity */
function finiteNumber(value: number): number {
  return Number.isFinite(value) ? value : 0;
}

/** 限制脱敏字段长度，避免诊断记录持有超长正文或 URL */
function truncate(value: string, maxLength: number): string {
  const text = typeof value === "string" ? value : "";
  return text.length <= maxLength ? text : text.slice(0, maxLength);
}

/** 归一时间戳为 ISO 字符串；无法解析时丢弃该可选字段 */
function normalizedTimestamp(value: string | undefined): string | undefined {
  if (!value) return undefined;
  const timestamp = Date.parse(value);
  return Number.isNaN(timestamp) ? undefined : new Date(timestamp).toISOString();
}

function isSSEState(value: string): value is "connecting" | "open" | "closed" | "failed" {
  return value === "connecting" || value === "open" || value === "closed" || value === "failed";
}

function isSSEEvent(value: string): value is "changed" | "revoked" | "invited" {
  return value === "changed" || value === "revoked" || value === "invited";
}

function isHTTPMethod(value: string): value is "GET" | "POST" | "PATCH" | "DELETE" {
  return value === "GET" || value === "POST" || value === "PATCH" || value === "DELETE";
}

function isTransferScope(value: string): value is "upload" | "download" | "collab_upload" | "collab_download" {
  return value === "upload" || value === "download" || value === "collab_upload" || value === "collab_download";
}
