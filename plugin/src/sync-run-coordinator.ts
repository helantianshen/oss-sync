/** 单次同步任务；forceFull 表示本次是否要求全量快照 */
export type SyncRunTask = (forceFull: boolean) => Promise<boolean>;

/** 同一时刻只执行一次同步；并发请求合并为下一轮，完整同步请求优先 */
export class SyncRunCoordinator {
  private activeRun: Promise<boolean> | null = null;
  private queued = false;
  private queuedForceFull = false;

  get isRunning(): boolean {
    return this.activeRun !== null;
  }

  /** 合并在途请求，让所有等待方获得队列实际执行的结果。 */
  run(forceFull: boolean, task: SyncRunTask): Promise<boolean> {
    if (this.activeRun) {
      this.queued = true;
      this.queuedForceFull ||= forceFull;
      return this.activeRun;
    }

    this.activeRun = this.drain(forceFull, task).finally(() => {
      this.activeRun = null;
      this.queued = false;
      this.queuedForceFull = false;
    });
    return this.activeRun;
  }

  // 排空合并队列；只要期间有新请求就再跑一轮，标记位在每轮开始前消费
  private async drain(forceFull: boolean, task: SyncRunTask): Promise<boolean> {
    let nextForceFull = forceFull;
    while (true) {
      const succeeded = await task(nextForceFull);
      if (!this.queued) return succeeded;
      nextForceFull = this.queuedForceFull;
      this.queued = false;
      this.queuedForceFull = false;
    }
  }
}
