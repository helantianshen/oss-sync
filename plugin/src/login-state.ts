/** 登录表单缺失项的标识 */
export type LoginCredentialError = "username_required" | "password_required";

/** 服务端地址不合法原因的标识 */
export type ServerURLValidationError = "protocol_required" | "invalid_url";

/** 按用户名、密码顺序校验，用户名仅忽略首尾空白，密码不允许空白 */
export function validateLoginCredentials(
  username: string,
  password: string
): LoginCredentialError | null {
  if (!username.trim()) return "username_required";
  if (!password) return "password_required";
  return null;
}

/** 要求 http/https 协议且 URL 可解析、hostname 非空，首尾空白被忽略 */
export function validateServerURL(value: string): ServerURLValidationError | null {
  const trimmed = value.trim();
  if (!/^https?:\/\//i.test(trimmed)) return "protocol_required";
  try {
    const parsed = new URL(trimmed);
    if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || !parsed.hostname) {
      return "invalid_url";
    }
  } catch {
    return "invalid_url";
  }
  return null;
}

/** pending 表示设备等待授权，此时不初始化同步会话；未获取到设备状态时按可初始化处理 */
export function shouldInitializeAuthorizedSession(
  deviceStatus: "pending" | "approved" | "revoked" | undefined
): boolean {
  return deviceStatus !== "pending";
}
