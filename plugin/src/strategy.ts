// 同步策略模块：拉取并缓存服务端仓库同步策略
import type { OSSApiClient, SyncMode, SyncStrategyResponse } from "./api";

/** 缓存服务端仓库同步策略，未拉取到时各项取保守默认值 */
export class SyncStrategyManager {
  private strategy: SyncStrategyResponse | null = null;
  private vaultID = "";

  constructor(private readonly api: OSSApiClient) {}

  /** 当前生效的同步模式，未获取策略时默认短轮询*/
  getEffectiveMode(): SyncMode {
    return this.strategy?.effective_mode ?? "short_poll";
  }

  /** 服务端允许的最小本地防抖秒数*/
  getMinDebounceSec(): number {
    return this.strategy?.min_debounce_sec ?? 3;
  }

  /** 长轮询单次等待秒数*/
  getLongPollWaitSec(): number {
    return this.strategy?.long_poll_wait_sec ?? 30;
  }

  supportsCommunityPluginSettings(vaultID: string): boolean {
    return this.vaultID === vaultID && this.strategy?.community_plugin_settings === true;
  }

  /** 拉取最新策略并缓存，失败由调用方决定如何处理*/
  async fetch(vaultID: string, userMode: SyncMode): Promise<SyncStrategyResponse> {
    this.strategy = await this.api.syncStrategy(vaultID, userMode);
    this.vaultID = vaultID;
    return this.strategy;
  }
}
