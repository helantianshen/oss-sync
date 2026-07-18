import { App, Notice, PluginSettingTab, Setting } from "obsidian";
import type { ButtonComponent } from "obsidian";
import type OSSPlugin from "./main";
import type { OSSSettings } from "./settings";

export class OSSSettingTab extends PluginSettingTab {
  constructor(app: App, private plugin: OSSPlugin) {
    super(app, plugin);
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    containerEl.createEl("h3", { text: "OSS Server" });

    new Setting(containerEl)
      .setName("Server URL")
      .setDesc("后端地址，例如 http://localhost:8080")
      .addText((text) =>
        text
          .setPlaceholder("http://localhost:8080")
          .setValue(this.plugin.settings.serverUrl)
          .onChange(async (value) => {
            this.plugin.settings.serverUrl = value.replace(/\/$/, "");
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Username")
      .addText((text) =>
        text
          .setValue(this.plugin.settings.username)
          .onChange(async (value) => {
            this.plugin.settings.username = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Password")
      .addText((text) => {
        text.inputEl.type = "password";
        text
          .setValue(this.plugin.settings.password)
          .onChange(async (value) => {
            this.plugin.settings.password = value;
            await this.plugin.saveSettings();
          });
      });

    let registerButton: ButtonComponent | null = null;
    const authSetting = new Setting(containerEl)
      .setName("Login")
      .setDesc("正在检查服务端注册状态...")
      .addButton((btn) =>
        btn.setButtonText("Login").onClick(async () => {
          const error = this.validateCredentials();
          if (error) {
            new Notice(error);
            return;
          }
          try {
            await this.plugin.login();
            new Notice("OSS 登录成功");
          } catch (e) {
            new Notice("OSS 登录失败: " + this.errorMessage(e));
          }
        })
      )
      .addButton((btn) => {
        registerButton = btn;
        btn.setButtonText("创建首个 admin").setDisabled(true).onClick(async () => {
          const error = this.validateCredentials();
          if (error) {
            new Notice(error);
            return;
          }
          try {
            await this.plugin.register();
            new Notice("OSS 首个 admin 创建成功，已自动登录");
            btn.setDisabled(true);
            authSetting.setDesc("首个 admin 已创建。后续请使用 Login；新用户须由 admin 授权创建。");
          } catch (e) {
            new Notice("OSS 注册失败: " + this.errorMessage(e));
          }
        });
      });

    void this.plugin.api.authStatus().then((status) => {
      if (status.needs_first_admin) {
        authSetting.setDesc("服务端尚未初始化：填写用户名和至少 8 位密码，然后创建首个 admin。");
        registerButton?.setDisabled(false);
      } else {
        authSetting.setDesc("首个 admin 已存在。请使用 Login；匿名注册已关闭。");
        registerButton?.setDisabled(true);
      }
    }).catch((e: unknown) => {
      authSetting.setDesc("无法读取服务端认证状态，请检查 Server URL 和后端服务。");
      new Notice("OSS 状态检查失败: " + this.errorMessage(e));
    });

    containerEl.createEl("h3", { text: "Sync" });

    new Setting(containerEl)
      .setName("Sync interval (seconds)")
      .setDesc("防抖时间，停顿多久后触发自动同步。默认 300 = 5 分钟")
      .addText((text) =>
        text
          .setPlaceholder("300")
          .setValue(String(this.plugin.settings.syncIntervalSec))
          .onChange(async (value) => {
            const n = parseInt(value, 10);
            if (!isNaN(n) && n >= 5) {
              this.plugin.settings.syncIntervalSec = n;
              await this.plugin.saveSettings();
              this.plugin.syncEngine.resetDebounce();
            }
          })
      );

    new Setting(containerEl)
      .setName("Max concurrency")
      .setDesc("决策 7.2：同时上传/下载的并发上限。1–10，默认 6")
      .addText((text) =>
        text
          .setPlaceholder("6")
          .setValue(String(this.plugin.settings.maxConcurrency))
          .onChange(async (value) => {
            const n = parseInt(value, 10);
            if (!isNaN(n) && n >= 1 && n <= 10) {
              this.plugin.settings.maxConcurrency = n;
              await this.plugin.saveSettings();
            }
          })
      );

    new Setting(containerEl)
      .setName("Sync .obsidian poison files")
      .setDesc("决策 6.2：workspace.json / cache / 插件 data.json 等。默认关")
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.syncPoisonObsidianFiles)
          .onChange(async (value) => {
            this.plugin.settings.syncPoisonObsidianFiles = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Incremental check")
      .setDesc("决策 6.5：仅提交本端 mtime 有变动的文件。默认开")
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.incrementalCheck)
          .onChange(async (value) => {
            this.plugin.settings.incrementalCheck = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Force full sync now")
      .setDesc("立即触发一次全量 check（决策 6.5 强制全量校验按钮）")
      .addButton((btn) =>
        btn.setButtonText("Sync now").onClick(async () => {
          await this.plugin.syncEngine.runOnce({ forceFull: true });
        })
      );
  }

  private validateCredentials(): string | null {
    if (this.plugin.settings.username.trim().length < 3) {
      return "用户名至少需要 3 个字符";
    }
    if (this.plugin.settings.password.length < 8) {
      return "密码至少需要 8 个字符";
    }
    return null;
  }

  private errorMessage(error: unknown): string {
    return error instanceof Error ? error.message : "未知错误";
  }
}
