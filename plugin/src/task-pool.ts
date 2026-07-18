// 带并发限制和指数退避的任务池。

export interface RetryOptions {
  maxConcurrency: number;
  maxRetries: number;
  baseDelayMs: number;
}

export interface TaskResult<T> {
  ok: boolean;
  result?: T;
  error?: Error;
}

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
