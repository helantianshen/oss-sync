import type { TranslationKey } from "./i18n";

type Translator = (key: TranslationKey) => string;

const NETWORK_ERROR_KEYS: ReadonlyArray<readonly [string, TranslationKey]> = [
  ["ERR_CONNECTION_REFUSED", "network.connectionRefused"],
  ["ERR_CONNECTION_TIMED_OUT", "network.connectionTimedOut"],
  ["ERR_NAME_NOT_RESOLVED", "network.nameNotResolved"],
  ["ERR_INTERNET_DISCONNECTED", "network.disconnected"],
  ["ERR_NETWORK_CHANGED", "network.disconnected"],
  ["ERR_TOO_MANY_REDIRECTS", "auth.unauthorized"],
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

export function localizeError(error: unknown, t: Translator, fallback: string): string {
  const code = typeof error === "object" && error !== null && "code" in error && typeof error.code === "string" ? error.code : "";
  for (const [candidate, key] of AUTH_CODE_KEYS) {
    if (code === candidate) return t(key);
  }
  const message = error instanceof Error ? error.message : "";
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
