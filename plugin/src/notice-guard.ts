/**
 * 相同错误的通知在冷却期内只显示一次。
 * 轮询类失败会周期性重试，无限弹窗会淹没界面；状态栏与侧边栏已经持续展示错误状态。
 */

const DEFAULT_COOLDOWN_MS = 30_000;

/** 手动触发（命令、设置页按钮）的错误属于用户主动操作，每次都要反馈 */
export class NoticeGuard {
  private lastShownAt = new Map<string, number>();

  /** 返回 true 表示允许弹出，并在冷却期内抑制同 key 的后续通知 */
  shouldShow(key: string, now: number = Date.now()): boolean {
    const previous = this.lastShownAt.get(key);
    if (previous !== undefined && now - previous < DEFAULT_COOLDOWN_MS) return false;
    this.lastShownAt.set(key, now);
    return true;
  }

  /** 状态恢复（同步成功、重新登录）后清除抑制，让下一次失败能立即提示 */
  reset(key?: string): void {
    if (key === undefined) {
      this.lastShownAt.clear();
      return;
    }
    this.lastShownAt.delete(key);
  }
}
