import type { TranslationKey } from "./i18n";

type Translator = (key: TranslationKey) => string;

const NETWORK_ERROR_KEYS: ReadonlyArray<readonly [string, TranslationKey]> = [
  ["ERR_CONNECTION_REFUSED", "network.connectionRefused"],
  ["ERR_CONNECTION_TIMED_OUT", "network.connectionTimedOut"],
  ["ERR_NAME_NOT_RESOLVED", "network.nameNotResolved"],
  ["ERR_INTERNET_DISCONNECTED", "network.disconnected"],
  ["ERR_NETWORK_CHANGED", "network.disconnected"],
  // 重定向循环由代理或网关产生，与登录状态无关
  ["ERR_TOO_MANY_REDIRECTS", "network.tooManyRedirects"],
  ["ERR_ABORTED", "network.disconnected"],
  ["ERR_CONNECTION_RESET", "network.connectionRefused"],
  ["ERR_NETWORK_ACCESS_DENIED", "network.connectionRefused"],
];

const AUTH_ERROR_KEYS: ReadonlyArray<readonly [string, TranslationKey]> = [
  ["jwt token expired", "auth.tokenExpired"],
  ["invalid credentials", "auth.invalidCredentials"],
];

const AUTH_CODE_KEYS: ReadonlyArray<readonly [string, TranslationKey]> = [
  ["token_expired", "auth.tokenExpired"],
  ["device_pending", "auth.devicePending"],
  ["device_revoked", "auth.deviceRevoked"],
  ["device_not_authorized", "auth.deviceUnauthorized"],
  ["device_unknown", "auth.deviceUnknown"],
  ["device_identity_required", "auth.deviceIdentityRequired"],
  ["device_identity_mismatch", "auth.deviceIdentityMismatch"],
  ["share_target_missing", "sidebar.shareTargetMissing"],
  ["missing_base_revision", "collab.errorMissingBaseRevision"],
  ["invalid_base_revision", "collab.errorInvalidBaseRevision"],
  ["missing_operation_id", "collab.errorMissingOperationID"],
  ["invalid_operation_id", "collab.errorInvalidOperationID"],
  ["project_storage_quota_exceeded", "storage.projectQuotaExceeded"],
];

/** 401/403 的服务端消息不能让用户误判为密码错误，先判断 token 语义再退回通用未授权 */
const AUTH_STATUS_KEYS: ReadonlyArray<readonly [number, TranslationKey]> = [
  [401, "auth.unauthorized"],
  [403, "auth.forbidden"],
];

const TOKEN_EXPIRY_FRAGMENTS: readonly string[] = ["jwt token expired", "token expired", "invalid signature"];

/** requestUrl 解析非 JSON 响应体时抛 SyntaxError，转成可读的服务端故障描述 */
const JSON_PARSE_FRAGMENTS: readonly string[] = [
  "unexpected token",
  "is not valid json",
  "json.parse",
  "unexpected end of",
];

function isJsonParseError(error: unknown): boolean {
  if (!(error instanceof SyntaxError)) return false;
  const message = error.message.toLowerCase();
  return JSON_PARSE_FRAGMENTS.some((fragment) => message.includes(fragment));
}

/** 把错误转成用户可读的本地化文本；无法识别时退回原始消息或 fallback */
export function localizeError(error: unknown, t: Translator, fallback: string): string {
  const code = typeof error === "object" && error !== null && "code" in error && typeof error.code === "string" ? error.code : "";
  if (code === "sync_request_timeout") return t("network.connectionTimedOut");
  for (const [candidate, key] of AUTH_CODE_KEYS) {
    if (code === candidate) return t(key);
  }
  const message = error instanceof Error ? error.message : "";
  // 响应体解析失败先于网络与状态码判断，服务端异常页不应泄漏语法错误细节
  if (isJsonParseError(error)) return t("network.invalidResponseBody");
  for (const [code, key] of NETWORK_ERROR_KEYS) {
    if (message.includes(code)) return t(key);
  }
  // HTTP 状态码优先于消息文本：服务端 401 的措辞可能同时包含凭据字样，会让未登录被误报成密码错误
  const status = typeof error === "object" && error !== null && "status" in error && typeof error.status === "number" ? error.status : 0;
  for (const [candidate, key] of AUTH_STATUS_KEYS) {
    if (status === candidate) {
      const normalized = message.toLowerCase();
      if (TOKEN_EXPIRY_FRAGMENTS.some((fragment) => normalized.includes(fragment))) return t("auth.tokenExpired");
      return t(key);
    }
  }
  const normalized = message.toLowerCase();
  if (TOKEN_EXPIRY_FRAGMENTS.some((fragment) => normalized.includes(fragment))) return t("auth.tokenExpired");
  for (const [fragment, key] of AUTH_ERROR_KEYS) {
    if (normalized.includes(fragment)) return t(key);
  }
  return message || fallback;
}
