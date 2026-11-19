// 分享区：渲染当前仓库的公开文章、浏览量与管理操作
import { Notice, SuggestModal, TFile } from "obsidian";
import type { ShareOut } from "./api";
import type OSSPlugin from "./main";

/**
 * 在容器中渲染分享列表行
 * 每行提供复制链接、打开、切换允许复制与删除操作；onChanged 缺失时回落到侧边栏刷新
 */
export function renderShareRows(
  section: HTMLElement,
  plugin: OSSPlugin,
  options: {
    readonly shares: readonly ShareOut[];
    readonly onChanged?: () => void | Promise<void>;
  }
): void {
  const { shares, onChanged } = options;
  if (shares.length === 0) {
    section.createDiv({ cls: "oss-sidebar-empty", text: plugin.t("sidebar.noShares") });
    return;
  }
  for (const share of shares) {
    const row = section.createDiv({ cls: "oss-sidebar-share" });
    row.createDiv({ cls: "oss-sidebar-share-path", text: share.target_path });
    row.createDiv({
      cls: "oss-sidebar-share-meta",
      text: plugin.t("sidebar.shareMeta", { views: share.views }),
    });
    const actions = row.createDiv({ cls: "oss-sidebar-actions oss-sidebar-share-actions" });
    const url = absoluteShareURL(plugin.settings.serverUrl, share.url);
    actions.createEl("button", {
      cls: "oss-sidebar-share-copy",
      text: plugin.t("sidebar.copyLink"),
    }).addEventListener("click", () => void copyShareLink(plugin, url));
    actions.createEl("a", {
      cls: "oss-sidebar-share-open",
      text: plugin.t("sidebar.openShare"),
      href: url,
      attr: { target: "_blank", rel: "noopener" },
    });
    actions.createEl("button", {
      cls: "oss-sidebar-share-toggle",
      text: plugin.t(share.allow_copy ? "sidebar.blockCopy" : "sidebar.allowCopy"),
    }).addEventListener("click", () => void updateAllowCopy(plugin, share, onChanged));
    if (!share.is_folder && share.vault_id === plugin.settings.vaultId) {
      actions.createEl("button", {
        text: plugin.t("sidebar.rebindShare"),
      }).addEventListener("click", () => new ShareTargetPicker(plugin, share, onChanged).open());
    }
    actions.createEl("button", {
      cls: "oss-sidebar-share-delete mod-warning",
      text: plugin.t("sidebar.deleteShare"),
    }).addEventListener("click", () => void deleteShare(plugin, share.share_id, onChanged));
  }
}

class ShareTargetPicker extends SuggestModal<TFile> {
  constructor(
    private readonly plugin: OSSPlugin,
    private readonly share: ShareOut,
    private readonly onChanged?: () => void | Promise<void>,
  ) {
    super(plugin.app);
    this.setPlaceholder(plugin.t("sidebar.rebindSharePrompt"));
  }

  getSuggestions(query: string): TFile[] {
    const search = query.trim().toLowerCase();
    return this.app.vault.getMarkdownFiles()
      .filter((file) => file.path !== this.share.target_path && file.path.toLowerCase().includes(search))
      .slice(0, 50);
  }

  renderSuggestion(file: TFile, el: HTMLElement): void {
    el.setText(file.path);
  }

  onChooseSuggestion(file: TFile): void {
    void this.rebind(file);
  }

  private async rebind(file: TFile): Promise<void> {
    if (this.share.vault_id !== this.plugin.settings.vaultId) return;
    try {
      await this.plugin.api.updateShareTarget(this.share.share_id, file.path);
      new Notice(this.plugin.t("sidebar.shareRebound"));
      await this.onChanged?.();
    } catch (error) {
      new Notice(this.plugin.t("sidebar.shareActionFailed", { error: this.plugin.localizedError(error) }));
    }
  }
}

function absoluteShareURL(serverURL: string, shareURL: string): string {
  if (/^https?:\/\//i.test(shareURL)) return shareURL;
  return serverURL.replace(/\/$/, "") + (shareURL.startsWith("/") ? shareURL : `/${shareURL}`);
}

async function copyShareLink(plugin: OSSPlugin, url: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(url);
    new Notice(plugin.t("sidebar.shareLinkCopied"));
  } catch (error) {
    const message = plugin.localizedError(error);
    new Notice(plugin.t("sidebar.shareActionFailed", { error: message }));
  }
}

async function updateAllowCopy(
  plugin: OSSPlugin,
  share: ShareOut,
  onChanged?: () => void | Promise<void>
): Promise<void> {
  try {
    await plugin.api.updateShareAllowCopy(share.share_id, !share.allow_copy);
    share.allow_copy = !share.allow_copy;
    if (onChanged) {
      await onChanged();
    } else {
      plugin.sidebarView?.refresh();
    }
  } catch (error) {
    const message = plugin.localizedError(error);
    new Notice(plugin.t("sidebar.shareActionFailed", { error: message }));
  }
}

async function deleteShare(
  plugin: OSSPlugin,
  shareID: string,
  onChanged?: () => void | Promise<void>
): Promise<void> {
  try {
    await plugin.api.deleteShare(shareID);
    if (onChanged) {
      await onChanged();
    } else {
      plugin.sidebarView?.reloadShares();
    }
  } catch (error) {
    const message = plugin.localizedError(error);
    new Notice(plugin.t("sidebar.shareActionFailed", { error: message }));
  }
}
