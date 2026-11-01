// HTTP 传输层：优先使用 Obsidian 的 requestUrl，遇到 Chromium 网络栈特有的
// ERR_TOO_MANY_REDIRECTS 时降级到 Node 原生 https 模块
//
// 背景：服务端经 Cloudflare 分发并宣告 alt-svc: h3。Electron 的 session 会缓存该
// 协商结果，随后的 GET 可能被反复重定向直至报错；curl 与 Node https 不受影响。
// 该失败只出现在 Obsidian 进程内，外部无法复现，因此以降级方式规避。

import { Platform, requestUrl } from "obsidian";
import { diagnosticRoute, diagnosticErrorDetails, type DiagnosticDetails } from "./diagnostic-report";

type TransportDiagnosticSink = (stage: string, details: DiagnosticDetails) => void;
let transportDiagnosticSink: TransportDiagnosticSink | null = null;
const diagnosticRequestIDs = new WeakMap<HttpRequest, number>();
let diagnosticRequestSequence = 0;

export function setTransportDiagnosticSink(sink: TransportDiagnosticSink | null): void {
  transportDiagnosticSink = sink;
}

function trace(request: HttpRequest, stage: string, details: DiagnosticDetails = {}): void {
  try {
    const url = new URL(request.url);
    transportDiagnosticSink?.(stage, { requestId: diagnosticRequestIDs.get(request),
      route: diagnosticRoute(request.url), path: url.searchParams.get("path") ?? undefined,
      method: request.method, driver: "requestUrl", ...details });
  } catch { /* 日志观察者的失败不改变请求结果。 */ }
}

function responseDetails(response: { status: number; arrayBuffer: ArrayBuffer; headers: Record<string, string> }): DiagnosticDetails {
  const headers = Object.fromEntries(Object.entries(response.headers).map(([key, value]) => [key.toLowerCase(), value]));
  return { status: response.status, bytes: response.arrayBuffer.byteLength,
    contentType: headers["content-type"], contentLength: headers["content-length"],
    hasHash: !!headers["x-oss-hash"], responseRevision: headers["x-oss-revision"] };
}

/** 请求描述；body 为字符串或二进制 */
export interface HttpRequest {
  readonly url: string;
  readonly method: "GET" | "POST" | "PATCH" | "DELETE";
  readonly headers: Record<string, string>;
  readonly body?: string | ArrayBuffer;
}

/** 统一响应；json 在响应体非 JSON 或为空时为 null */
export interface HttpResponse {
  readonly status: number;
  readonly json: unknown;
  readonly text: string;
  readonly arrayBuffer: ArrayBuffer;
  readonly headers: Record<string, string>;
}

/** requestUrl 抛出的重定向循环属于网络栈问题，改用原生 https 重试 */
function isRedirectLoop(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  return error.message.includes("ERR_TOO_MANY_REDIRECTS");
}

/**
 * requestUrl 对非 JSON 响应体会自行解析并抛 SyntaxError
 * 服务端故障时返回 HTML 错误页，此时改用原生 https 重新读取
 * 仅对幂等方法重试：非幂等请求重发可能造成重复提交
 */
function isRetryableJsonFailure(error: unknown, method: string): boolean {
  if (!(error instanceof SyntaxError)) return false;
  return method === "GET";
}

/** 文件响应不一定是 JSON；本地解析失败时保留原始字节和文本。 */
function parseJSONBody(text: string): unknown {
  const body = text.trim();
  if (!body) return null;
  try { return JSON.parse(body); } catch { return null; }
}

async function viaRequestUrl(request: HttpRequest): Promise<HttpResponse> {
  try {
    const res = await requestUrl({
      url: request.url,
      method: request.method as "GET" | "POST",
      headers: request.headers,
      body: request.body,
      throw: false,
    });
    trace(request, "response_received", responseDetails(res));
    const text = res.text;
    const response: HttpResponse = {
      status: res.status,
      json: parseJSONBody(text),
      text,
      arrayBuffer: res.arrayBuffer,
      headers: res.headers,
    };
    reportNonJson(request.url, response.status, response.text);
    return response;
  } catch (error) {
    if (!Platform.isDesktop || !isRetryableJsonFailure(error, request.method)) throw error;
    trace(request, "fallback", { driver: "node", ...diagnosticErrorDetails(error) });
    return viaNodeHttps(request);
  }
}

async function viaNodeHttps(request: HttpRequest): Promise<HttpResponse> {
  const target = new URL(request.url);
  if (Platform.isDesktop) {
    const transport = target.protocol === "http:" ? await import("http") : await import("https");
    const payload =
      request.body === undefined
        ? undefined
        : typeof request.body === "string"
          ? Buffer.from(request.body, "utf-8")
          : Buffer.from(request.body);

    const raw = await new Promise<{ status: number; headers: Record<string, string | string[] | undefined>; body: Buffer }>(
      (resolve, reject) => {
        const req = transport.request(
          {
            protocol: target.protocol,
            hostname: target.hostname,
            port: target.port || (target.protocol === "https:" ? 443 : 80),
            path: `${target.pathname}${target.search}`,
            method: request.method,
            headers: {
              ...request.headers,
              ...(payload ? { "Content-Length": String(payload.byteLength) } : {}),
            },
          },
          (res) => {
            const chunks: Buffer[] = [];
            res.on("data", (chunk: Buffer) => chunks.push(chunk));
            res.on("end", () => {
              resolve({
                status: res.statusCode ?? 0,
                headers: res.headers,
                body: Buffer.concat(chunks),
              });
            });
          }
        );
        req.on("error", reject);
        if (payload) req.write(payload);
        req.end();
      }
    );

    const text = raw.body.toString("utf-8");
    const json = parseJSONBody(text);
    const arrayBuffer = raw.body.buffer.slice(
      raw.body.byteOffset,
      raw.body.byteOffset + raw.body.byteLength
    ) as ArrayBuffer;
    const headers: Record<string, string> = {};
    for (const [key, value] of Object.entries(raw.headers)) {
      if (typeof value === "string") headers[key] = value;
    }
    const response: HttpResponse = { status: raw.status, json, text, arrayBuffer, headers };
    trace(request, "response_received", { ...responseDetails(response), driver: "node" });
    reportNonJson(request.url, response.status, text);
    return response;
  }
  throw new Error("Node transport is only available on desktop");
}

/** 发送请求；Chromium 网络栈重定向循环时自动降级到原生 https */
export async function httpRequest(request: HttpRequest): Promise<HttpResponse> {
  const startedAt = Date.now();
  diagnosticRequestIDs.set(request, ++diagnosticRequestSequence);
  trace(request, "request_started");
  try {
    return await viaRequestUrl(request);
  } catch (error) {
    trace(request, "request_failed", { durationMs: Date.now() - startedAt, ...diagnosticErrorDetails(error) });
    if (!Platform.isDesktop || !isRedirectLoop(error)) throw error;
    trace(request, "fallback", { driver: "node" });
    return viaNodeHttps(request);
  }
}

/** 诊断钩子：记录非 JSON 响应的原文前若干字符，供定位服务端异常页 */
export type NonJsonSink = (info: {
  readonly url: string;
  readonly status: number;
  readonly body: string;
}) => void;

let nonJsonSink: NonJsonSink | null = null;

/** 注册非 JSON 响应观察者；传 null 注销 */
export function setNonJsonSink(sink: NonJsonSink | null): void {
  nonJsonSink = sink;
}

/** 把非 JSON 响应体前若干字符交给观察者，便于定位服务端异常页 */
function reportNonJson(url: string, status: number, text: string): void {
  if (!nonJsonSink) return;
  const trimmed = text.trim();
  if (trimmed.startsWith("{") || trimmed.startsWith("[")) return;
  nonJsonSink({ url, status, body: trimmed.slice(0, 200) });
}
