// 服务端更新轮询控制器，逻辑纯粹且有界，不依赖 Obsidian
import type {
  ServerUpdateStatusResponse,
  ServerVersionInfo,
} from "./api";
import { OSSApiError } from "./api";

/** 轮询间隔基准值，毫秒*/
export const SERVER_UPDATE_POLL_INTERVAL_MS = 2000;
/** 轮询尝试次数上限*/
export const SERVER_UPDATE_MAX_ATTEMPTS = 60;
/** 轮询总时长上限，毫秒*/
export const SERVER_UPDATE_MAX_DURATION_MS = 120_000;

/** 服务端更新的终态分类*/
export type ServerUpdateTerminalState = "done" | "failed" | "up_to_date";

const TERMINAL_STATES = new Set<string>(["done", "failed", "up_to_date"]);
const TERMINAL_PHASES = new Set<string>(["done", "failed", "up_to_date"]);

/** state 字段是否已进入终态*/
export function isTerminalServerState(state: string): boolean {
  return TERMINAL_STATES.has(state);
}

/** phase 字段是否已进入终态*/
export function isTerminalServerPhase(phase: string): boolean {
  return TERMINAL_PHASES.has(phase);
}

/** 错误是否表示当前角色已失效，需要重新鉴权*/
export function isStaleRoleError(error: unknown): boolean {
  const status = (error as { status?: unknown })?.status;
  if (typeof status === "number") {
    return status === 401 || status === 403;
  }
  if (error instanceof OSSApiError) {
    return error.status === 401 || error.status === 403;
  }
  return false;
}

/**
 * 错误是否属于可重试的连接类瞬断
 * 仅依据错误消息中的浏览器与 Node 网络错误码判定；401 与 403 属于角色变化，不算瞬断
 */
export function isTransientConnectionError(error: unknown): boolean {
  const status = (error as { status?: unknown })?.status;
  if (typeof status === "number") {
    if (status === 401 || status === 403) return false;
    return false;
  }
  if (error instanceof OSSApiError) {
    // 401 与 403 表示角色变化，不属于瞬断
    if (error.status === 401 || error.status === 403) return false;
    // 其余 OSSApiError 同样在此返回 false，由调用方继续判定
    return false;
  }
  const message = error instanceof Error ? error.message : String(error ?? "");
  return (
    message.includes("ERR_CONNECTION_REFUSED") ||
    message.includes("ERR_CONNECTION_TIMED_OUT") ||
    message.includes("ERR_NAME_NOT_RESOLVED") ||
    message.includes("ERR_INTERNET_DISCONNECTED") ||
    message.includes("ERR_NETWORK_CHANGED") ||
    message.includes("ECONNREFUSED") ||
    message.includes("ETIMEDOUT") ||
    message.includes("Failed to fetch") ||
    message.includes("NetworkError") ||
    message.includes("network") ||
    message.includes("fetch failed")
  );
}

/** 轮询成功：服务端版本已与期望版本一致*/
export interface PollOutcomeSuccess {
  readonly kind: "success";
  readonly version: string;
  readonly status: ServerUpdateStatusResponse;
}

/** 轮询失败：服务端进入 failed 终态且未回滚*/
export interface PollOutcomeFailed {
  readonly kind: "failed";
  readonly state: string;
  readonly error: string;
  readonly status: ServerUpdateStatusResponse;
}

/** 轮询回滚：服务端更新失败且版本回退到旧版本*/
export interface PollOutcomeRolledBack {
  readonly kind: "rolled_back";
  readonly versionBefore: string;
  readonly currentVersion: string;
  readonly status: ServerUpdateStatusResponse;
}

/** 轮询超时：达到尝试次数或总时长上限仍未终态*/
export interface PollOutcomeTimeout {
  readonly kind: "timeout";
  readonly attempts: number;
  readonly lastStatus: ServerUpdateStatusResponse | null;
}

/** 轮询鉴权失败：401 或 403 表示角色已变化*/
export interface PollOutcomeAuthError {
  readonly kind: "auth_error";
  readonly error: unknown;
}

/** 单轮 poll 的返回结果，按 kind 区分*/
export type PollOutcome =
  | PollOutcomeSuccess
  | PollOutcomeFailed
  | PollOutcomeRolledBack
  | PollOutcomeTimeout
  | PollOutcomeAuthError;

/** 轮询参数；期望版本会去掉 v 前缀后比较*/
export interface ServerUpdatePollerOptions {
  readonly expectedVersion: string;
  readonly intervalMs?: number;
  readonly maxAttempts?: number;
  readonly maxDurationMs?: number;
}

/** 轮询依赖的两个服务端接口，便于测试注入*/
export interface ServerUpdatePollerDeps {
  readonly getStatus: () => Promise<ServerUpdateStatusResponse>;
  readonly getVersion: () => Promise<ServerVersionInfo>;
}

/** 有界轮询控制器；实例非线程安全，stop 与 poll 需串行调用*/
export class ServerUpdatePoller {
  private aborted = false;
  private startedAt = 0;
  private attempts = 0;
  private readonly intervalMs: number;
  private readonly maxAttempts: number;
  private readonly maxDurationMs: number;

  constructor(
    private readonly deps: ServerUpdatePollerDeps,
    private readonly opts: ServerUpdatePollerOptions,
    private readonly sleep: (ms: number) => Promise<void> = (ms) => new Promise<void>((resolve) => window.setTimeout(resolve, ms)),
  ) {
    this.intervalMs = opts.intervalMs ?? SERVER_UPDATE_POLL_INTERVAL_MS;
    this.maxAttempts = opts.maxAttempts ?? SERVER_UPDATE_MAX_ATTEMPTS;
    this.maxDurationMs = opts.maxDurationMs ?? SERVER_UPDATE_MAX_DURATION_MS;
  }

  getAttempts(): number {
    return this.attempts;
  }

  isAborted(): boolean {
    return this.aborted;
  }

  stop(): void {
    this.aborted = true;
  }

  dispose(): void {
    this.stop();
  }

  async poll(signal?: AbortSignal): Promise<PollOutcome> {
    this.aborted = false;
    this.attempts = 0;
    this.startedAt = Date.now();
    let lastStatus: ServerUpdateStatusResponse | null = null;

    while (!this.aborted) {
      if (signal?.aborted) {
        this.aborted = true;
        break;
      }
      if (this.attempts >= this.maxAttempts) {
        return { kind: "timeout", attempts: this.attempts, lastStatus };
      }
      if (Date.now() - this.startedAt >= this.maxDurationMs) {
        return { kind: "timeout", attempts: this.attempts, lastStatus };
      }
      this.attempts += 1;

      try {
        const status = await this.deps.getStatus();
        lastStatus = status;

        const outcome = this.evaluateTerminal(status);
        if (outcome !== null) return outcome;

        // 未进入终态时休眠后继续下一轮，轮询次数与总时长有上限
      } catch (error: unknown) {
        if (isStaleRoleError(error)) {
          return { kind: "auth_error", error };
        }
        // 非认证错误在重启期间允许重试，受总时长与尝试次数约束
      }

      if (this.aborted || signal?.aborted) break;
      await this.sleep(this.intervalMs);
    }

    return { kind: "timeout", attempts: this.attempts, lastStatus };
  }

  private evaluateTerminal(status: ServerUpdateStatusResponse): PollOutcome | null {
    const expected = normalizeVersion(this.opts.expectedVersion);
    const versionMatches = normalizeVersion(status.version) === expected;
    const lastUpdate = status.last_update;
    const state = status.state;
    const phase = lastUpdate?.phase ?? state;

    // 终态判定依据 state 或 phase 是否为 done、failed 或 up_to_date
    const isTerminal =
      isTerminalServerState(state) ||
      isTerminalServerState(phase) ||
      (lastUpdate != null &&
        (isTerminalServerPhase(lastUpdate.state) ||
          isTerminalServerPhase(lastUpdate.phase) ||
          isTerminalServerState(lastUpdate.state)));

    if (!isTerminal) {
      // 版本一致且 state 为 done 时直接视为成功
      if (versionMatches && state === "done") return { kind: "success", version: status.version, status };
      return null;
    }

    // 终态下按版本号与 ok 标记区分成功、回滚与失败
    if (versionMatches && (lastUpdate?.ok === true || state === "done" || phase === "done")) {
      return { kind: "success", version: status.version, status };
    }

    if (!versionMatches && (state === "failed" || lastUpdate?.state === "failed" || phase === "failed")) {
      const error = lastUpdate?.error ?? `server update failed (state=${state})`;
      // 回滚的启发式判定：已失败且版本号未达预期版本
      if (lastUpdate?.code === "failed" || state === "failed") {
        if (normalizeVersion(status.version) !== expected) {
          return { kind: "rolled_back", versionBefore: this.opts.expectedVersion, currentVersion: status.version, status };
        }
      }
      return { kind: "failed", state: state, error, status };
    }

    if (state === "failed" || lastUpdate?.state === "failed" || phase === "failed") {
      const error = lastUpdate?.error ?? `server update failed (state=${state})`;
      if (!versionMatches) {
        return { kind: "rolled_back", versionBefore: this.opts.expectedVersion, currentVersion: status.version, status };
      }
      return { kind: "failed", state, error, status };
    }

    // state 为 up_to_date 时视为未发生更新，按失败返回
    if (state === "up_to_date") {
      return { kind: "failed", state, error: lastUpdate?.error ?? "already up to date", status };
    }

    if (isTerminal && versionMatches) {
      return { kind: "success", version: status.version, status };
    }

    return { kind: "failed", state, error: lastUpdate?.error ?? `terminal state=${state} phase=${phase}`, status };
  }
}

function normalizeVersion(value: string): string {
  return value.trim().replace(/^[vV]/, "");
}

/** 构造有界轮询器；sleep 可注入以便测试中跳过等待*/
export function createBoundedPoller(
  deps: ServerUpdatePollerDeps,
  opts: ServerUpdatePollerOptions,
  sleep?: (ms: number) => Promise<void>,
): ServerUpdatePoller {
  return new ServerUpdatePoller(deps, opts, sleep);
}
