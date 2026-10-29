import { App, Modal, Setting } from "obsidian";
import type OSSPlugin from "./main";
import type { TranslationKey } from "./i18n";

export function renderShareGuideEntry(container: HTMLElement, app: App, plugin: OSSPlugin): void {
  new Setting(container)
    .setName(plugin.t("shareGuide.title"))
    .setDesc(plugin.t("shareGuide.entry"))
    .addButton((button) => button
      .setButtonText(plugin.t("shareGuide.open"))
      .onClick(() => new ShareGuideModal(app, plugin).open()));
}

export class ShareGuideModal extends Modal {
  constructor(app: App, private readonly plugin: OSSPlugin) {
    super(app);
  }

  onOpen(): void {
    const { contentEl } = this;
    contentEl.empty();
    contentEl.addClass("markdown-rendered");
    this.titleEl.setText(this.plugin.t("shareGuide.title"));
    contentEl.createEl("p", { text: this.plugin.t("shareGuide.intro") });

    this.section("shareGuide.automaticTitle", ["shareGuide.filename", "shareGuide.opening", "shareGuide.automatic"]);

    contentEl.createEl("h3", { text: this.plugin.t("shareGuide.manualTitle") });
    contentEl.createEl("p", { text: this.plugin.t("shareGuide.manual") });
    contentEl.createEl("pre").createEl("code", { text: this.plugin.t("shareGuide.example") });
    contentEl.createEl("p", { text: this.plugin.t("shareGuide.manualHint") });

    this.section("shareGuide.bodyTitle", ["shareGuide.headings", "shareGuide.images", "shareGuide.links"]);
    this.section("shareGuide.discoveryTitle", ["shareGuide.publicURL", "shareGuide.blog", "shareGuide.stableURL"]);
    this.section("shareGuide.checkTitle", ["shareGuide.verify", "shareGuide.searchConsole", "shareGuide.expectations"]);

    contentEl.createEl("h3", { text: this.plugin.t("shareGuide.references") });
    const references = contentEl.createEl("ul");
    for (const [key, path] of [
      ["shareGuide.titleReference", "appearance/title-link"],
      ["shareGuide.summaryReference", "appearance/snippet"],
      ["shareGuide.indexReference", "crawling-indexing/ask-google-to-recrawl"],
    ] as const) {
      references.createEl("li").createEl("a", {
        text: this.plugin.t(key),
        href: `https://developers.google.com/search/docs/${path}`,
        attr: { target: "_blank", rel: "noopener noreferrer" },
      });
    }
    new Setting(contentEl).addButton((button) => button
      .setButtonText(this.plugin.t("common.close"))
      .onClick(() => this.close()));
  }

  onClose(): void {
    this.contentEl.empty();
  }

  private section(title: TranslationKey, items: readonly TranslationKey[]): void {
    this.contentEl.createEl("h3", { text: this.plugin.t(title) });
    const list = this.contentEl.createEl("ul");
    for (const key of items) list.createEl("li", { text: this.plugin.t(key) });
  }
}
