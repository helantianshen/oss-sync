// 带并发限制和指数退避的任务池

/** 任务池并发与重试参数；并发数被限制在 1-10，重试次数与基数延迟不允许负数 */
export interface RetryOptions {
  maxConcurrency: number;
  maxRetries: number;
  baseDelayMs: number;
}

/** 单任务结果；ok 为 false 时 error 为最后一次失败原因 */
export interface TaskResult<T> {
  ok: boolean;
  result?: T;
  error?: Error;
}

/** 带并发限制与指数退避重试的任务池；结果顺序与输入 items 一致 */
export class TaskPool {
  private active = 0;
  private readonly maxConcurrency: number;
  private readonly maxRetries: number;
  private readonly baseDelayMs: number;

  constructor(opts: RetryOptions) {
    this.maxConcurrency = Math.max(1, Math.min(10, opts.maxConcurrency));
    this.maxRetries = Math.max(0, opts.maxRetries);
    this.baseDelayMs = Math.max(0, opts.baseDelayMs);
  }

  /** 并发执行全部任务并等待完成；单个任务的失败不影响其余任务，重试耗尽后记为失败 */
  async run<T, R>(items: T[], worker: (item: T, index: number) => Promise<R>): Promise<TaskResult<R>[]> {
    const results: TaskResult<R>[] = new Array(items.length);
    const waiting: Array<() => void> = [];

    const tryAcquire = (): Promise<void> => {
      if (this.active < this.maxConcurrency) {
        this.active++;
        return Promise.resolve();
      }
      return new Promise<void>((resolve) => waiting.push(resolve));
    };

    // 释放槽位时直接移交给队首等待者，避免唤醒后再次竞争
    const release = (): void => {
      this.active--;
      const next = waiting.shift();
      if (next) {
        this.active++;
        next();
      }
    };

    const runOne = async (i: number): Promise<void> => {
      await tryAcquire();
      try {
        let lastErr: Error | null = null;
        for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
          try {
            const r = await worker(items[i], i);
            results[i] = { ok: true, result: r };
            return;
          } catch (e) {
            lastErr = e as Error;
            if (attempt < this.maxRetries) {
              // 第 n 次重试前等待 baseDelayMs * 2^(n-1)
              const delay = this.baseDelayMs * Math.pow(2, attempt);
              await sleep(delay);
            }
          }
        }
        results[i] = { ok: false, error: lastErr ?? new Error("unknown") };
      } finally {
        release();
      }
    };

    await Promise.all(items.map((_, i) => runOne(i)));
    return results;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}
