export type SyncRunTask = (forceFull: boolean) => Promise<void>;

export class SyncRunCoordinator {
  private activeRun: Promise<void> | null = null;
  private queued = false;
  private queuedForceFull = false;

  async run(forceFull: boolean, task: SyncRunTask): Promise<void> {
    if (this.activeRun) {
      this.queued = true;
      this.queuedForceFull ||= forceFull;
      await this.activeRun;
      return;
    }

    this.activeRun = this.drain(forceFull, task).finally(() => {
      this.activeRun = null;
      this.queued = false;
      this.queuedForceFull = false;
    });
    await this.activeRun;
  }

  private async drain(forceFull: boolean, task: SyncRunTask): Promise<void> {
    let nextForceFull = forceFull;
    while (true) {
      await task(nextForceFull);
      if (!this.queued) return;
      nextForceFull = this.queuedForceFull;
      this.queued = false;
      this.queuedForceFull = false;
    }
  }
}
