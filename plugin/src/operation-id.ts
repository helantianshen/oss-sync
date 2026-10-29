// operation-id.ts 统一生成与校验 collaboration operationID
// 规则与服务端 deviceauth.NormalizeClientID 保持一致：
// 长度为 1-64，仅允许 A-Z、a-z、0-9、-、_ 和 .
// 优先使用 crypto.randomUUID()，其输出完全符合该字符集

const OPERATION_ID_RE = /^[A-Za-z0-9._-]{1,64}$/;

/** 判断是否满足服务端 operationID 字符集与长度约束 */
export function isValidOperationID(value: unknown): value is string {
  return typeof value === "string" && OPERATION_ID_RE.test(value);
}

/** 去掉首尾空白并校验，非法时返回 null */
export function normalizeOperationID(value: string): string | null {
  const trimmed = value.trim();
  if (!isValidOperationID(trimmed)) return null;
  return trimmed;
}

/** 生成合法 operationID；crypto.randomUUID 不可用时以时间戳加随机串兜底 */
export function createOperationID(): string {
  // crypto.randomUUID() 在 Obsidian/Electron 环境始终可用
  try {
    const raw = crypto.randomUUID();
    const normalized = normalizeOperationID(raw);
    if (normalized) return normalized;
  } catch {
    // WebCrypto 不可用时继续使用下方的本地标识生成逻辑
  }
  // 备用分支使用时间戳加随机串，同样符合字符集
  const fallback = `${Date.now()}-${Math.random().toString(36).slice(2, 10)}-${Math.random().toString(36).slice(2, 10)}`;
  // 清洗为合法字符并截断至 64 字符
  const cleaned = fallback.replace(/[^A-Za-z0-9._-]/g, "-").slice(0, 64);
  if (isValidOperationID(cleaned)) return cleaned;
  // 极端兜底分支，返回短随机标识
  return "op-" + Math.random().toString(36).slice(2, 10);
}

/** 复用合法值，否则清洗，仍非法时调用 fallback 生成 */
export function ensureOperationID(value: unknown, fallback: () => string = createOperationID): string {
  if (isValidOperationID(value)) return value;
  if (typeof value === "string") {
    const n = normalizeOperationID(value);
    if (n) return n;
  }
  return fallback();
}

/** 服务端 revision 必须是非负安全整数 */
export function isValidServerRevision(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}
