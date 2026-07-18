// 并发控制队列 — 决策 7.2：限制同时 in-flight 上传/下载上限。
// 默认 MAX_CONCURRENCY=6，可在设置中调，范围 1–10。
// 单请求超时 60s，失败按指数退避重试最多 3 次。
//
// 不引入 p-limit 第三方依赖：手写最小信号量即可，减少 Obsidian 插件包大小。

export interface RetryOptions {
  maxConcurrency: number;
  maxRetries: number; // 默认 3
  baseDelayMs: number; // 默认 500ms，指数退避 base
}

export interface TaskResult<T> {
  ok: boolean;
  result?: T;
  error?: Error;
}

/**
 * 并发限制 + 指数退避重试的任务池。
 * 用法：
 *   const pool = new TaskPool({ maxConcurrency: 6, maxRetries: 3, baseDelayMs: 500 });
 *   const results = await pool.run(items, async (item) => worker(item));
 */
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

  async run<T, R>(items: T[], worker: (item: T, index: number) => Promise<R>): Promise<TaskResult<R>[]> {
    const results: TaskResult<R>[] = new Array(items.length);
    let cursor = 0;
    const waiting: Array<() => void> = [];

    const tryAcquire = (): Promise<void> => {
      if (this.active < this.maxConcurrency) {
        this.active++;
        return Promise.resolve();
      }
      return new Promise<void>((resolve) => waiting.push(resolve));
    };

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
  return new Promise((resolve) => setTimeout(resolve, ms));
}
