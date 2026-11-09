import type { DiagnosticEvent } from "./diagnostics";

export const DIAGNOSTIC_FOLDER = "OSS Sync Diagnostics";

export type DiagnosticDetails = Readonly<Record<string, string | number | boolean | undefined>>;

/** 只保留路由，不保留请求参数、凭据和具体 Vault ID。 */
export function diagnosticRoute(url: string): string {
  try {
    return new URL(url).pathname.replace(/\/vaults\/[^/]+/g, "/vaults/:vault").slice(0, 240);
  } catch {
    return "[invalid URL]";
  }
}

export function redactDiagnosticText(value: string, secrets: readonly string[] = []): string {
  let safe = value;
  for (const secret of secrets) {
    if (!secret) continue;
    for (const form of new Set([secret, encodeURIComponent(secret)])) safe = safe.split(form).join("[REDACTED]");
  }
  safe = safe.replace(/https?:\/\/[^\s<>"']+/gi, (raw) => {
    try {
      const url = new URL(raw);
      return `${url.protocol}//${url.host}${url.pathname}${url.search ? "?[REDACTED]" : ""}`;
    } catch { return "[REDACTED URL]"; }
  });
  safe = safe.replace(/\bBearer\s+[^\s,;"']+/gi, "Bearer [REDACTED]");
  safe = safe.replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b/g, "[REDACTED JWT]");
  return safe.replace(/\b((?:access[_-]?|refresh[_-]?)?token|password|passwd|secret|authorization|cookie)\b["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;&}]+)/gi, "$1=[REDACTED]");
}

/** JSON 解析异常可能引用笔记正文，故只保留类别和调用栈位置。 */
export function diagnosticErrorDetails(error: unknown): DiagnosticDetails {
  const value = error && typeof error === "object" ? error as Record<string, unknown> : {};
  const name = typeof value.name === "string" ? value.name : "Error";
  const message = name === "SyntaxError"
    ? "JSON decoding failed; response content omitted"
    : typeof value.message === "string" ? value.message : typeof error === "string" ? error : "Unknown failure";
  return {
    errorName: name.slice(0, 80),
    errorMessage: redactDiagnosticText(message).slice(0, 600),
    ...(typeof value.stack === "string" ? { errorStack: redactDiagnosticText(value.stack.split("\n").filter((line) => /^\s*at\s.+:\d+|@.+:\d+/.test(line)).slice(0, 8).join("\n")).slice(0, 1600) } : {}),
    ...(typeof value.status === "number" ? { status: value.status } : {}),
    ...(typeof value.code === "string" ? { code: redactDiagnosticText(value.code).slice(0, 120) } : {}),
  };
}

export function sanitizeDiagnosticDetails(details: DiagnosticDetails): DiagnosticDetails {
  const allowed = new Set(["requestId", "path", "route", "method", "driver", "status", "bytes", "expectedBytes", "revision", "durationMs", "attempt", "delayMs", "contentType", "contentLength", "hasHash", "responseRevision", "errorName", "errorMessage", "errorStack", "code", "action", "localState", "writeResult"]);
  const safe: Record<string, string | number | boolean> = {};
  for (const [key, value] of Object.entries(details)) {
    if (!allowed.has(key)) continue;
    if (typeof value === "string") safe[key] = redactDiagnosticText(value).slice(0, key === "errorStack" ? 1600 : 600);
    else if (typeof value === "boolean") safe[key] = value;
    else if (typeof value === "number" && Number.isFinite(value)) safe[key] = value;
  }
  return Object.freeze(safe);
}

interface ReportInput {
  pluginVersion: string;
  obsidianVersion: string;
  platform: string;
  operatingSystem: string;
  serverURL: string;
  loggedIn: boolean;
  vaultBound: boolean;
  pendingCount: number;
  settings: Readonly<Record<string, unknown>>;
  events: readonly DiagnosticEvent[];
  recentFailures: readonly DiagnosticEvent[];
}

/** 显式列出可导出的环境与设置，不序列化插件数据、凭据或笔记正文。 */
export function createDiagnosticReport(input: ReportInput, secrets: readonly string[] = []): string {
  const settings: Record<string, unknown> = {};
  for (const key of ["syncIntervalSec", "maxConcurrency", "incrementalCheck", "remotePollIntervalSec", "forceSSE", "diagnosticsEnabled", "vaultSyncMode", "language", "syncConfiguration", "autoResolveConfiguration", "syncCommunityPlugins"]) {
    const value = input.settings[key];
    if (["string", "number", "boolean"].includes(typeof value)) settings[key] = value;
  }
  const counts: Record<string, number> = {};
  for (const event of input.events) counts[event.kind] = (counts[event.kind] ?? 0) + 1;
  const report = {
    schemaVersion: 1, pluginID: "oss-sync",
    exportedAt: new Date().toISOString(),
    privacy: "Local export only. Includes relative file paths, server address and device runtime. No request/response bodies, credentials or authorization headers. Logs reset when the plugin restarts.",
    runtime: {
      pluginVersion: input.pluginVersion, obsidianVersion: input.obsidianVersion,
      platform: input.platform, operatingSystem: input.operatingSystem,
      serverURL: redactDiagnosticText(input.serverURL), loggedIn: input.loggedIn,
      vaultBound: input.vaultBound, pendingCount: input.pendingCount,
    },
    settings, summary: { eventCount: input.events.length, retainedFailureCount: input.recentFailures.length, counts },
    recentFailures: input.recentFailures, events: input.events,
  };
  return JSON.stringify(report, (_key, value: unknown) => typeof value === "string" ? redactDiagnosticText(value, secrets) : value, 2);
}

interface ExportVault<T> {
  getAbstractFileByPath(path: string): unknown;
  createFolder(path: string): Promise<unknown>;
  create(path: string, data: string): Promise<T>;
}

export async function saveDiagnosticReport<T>(vault: ExportVault<T>, json: string, now = new Date()): Promise<T> {
  if (!vault.getAbstractFileByPath(DIAGNOSTIC_FOLDER)) await vault.createFolder(DIAGNOSTIC_FOLDER);
  const stem = `${DIAGNOSTIC_FOLDER}/oss-sync-${now.toISOString().replace(/[:.]/g, "-")}`;
  let path = `${stem}.md`;
  for (let index = 1; vault.getAbstractFileByPath(path); index++) path = `${stem}-${index}.md`;
  return vault.create(path, `# OSS Sync diagnostic log\n\n\`\`\`json\n${json}\n\`\`\`\n`);
}
