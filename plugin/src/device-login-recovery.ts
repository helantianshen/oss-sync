/** 设备身份替换重试的结果；replacedRevokedIdentity 表示本地身份已被重新生成 */
export type DeviceLoginRecoveryResult<T> = {
  readonly response: T;
  readonly replacedRevokedIdentity: boolean;
};

// 服务端在设备被吊销时以 code 标识错误，其余错误直接向上抛
function isDeviceRevokedError(error: unknown): boolean {
  return typeof error === "object" && error !== null && "code" in error && error.code === "device_revoked";
}

/** 生成本机稳定 clientId；优先 crypto.randomUUID，退化为时间戳加随机串 */
export function createClientID(): string {
  if (typeof window.crypto?.randomUUID === "function") {
    return window.crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

/** 仅在设备确实被吊销时替换本地身份并重试登录一次 */
export async function loginWithRevokedDeviceRecovery<T>(
  login: () => Promise<T>,
  replaceIdentity: () => Promise<void>
): Promise<DeviceLoginRecoveryResult<T>> {
  try {
    return { response: await login(), replacedRevokedIdentity: false };
  } catch (error: unknown) {
    if (!isDeviceRevokedError(error)) throw error;
    await replaceIdentity();
    return { response: await login(), replacedRevokedIdentity: true };
  }
}
