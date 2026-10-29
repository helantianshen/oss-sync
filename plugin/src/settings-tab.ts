import { ConfirmModal } from "./confirm-modal";
import { App, Notice, PluginSettingTab, Setting } from "obsidian";
import type { ButtonComponent } from "obsidian";
import type OSSPlugin from "./main";
import type { OSSSettings } from "./settings";
import { validateLoginCredentials, validateServerURL } from "./login-state";
import { OSS_LOGO_DATA_URL } from "./brand";
import { renderShareGuideEntry } from "./share-guide-modal";

/** 插件设置页：按语言、服务端、仓库、设备、同步与更新分组渲染表单，并维护仓库下拉的定时刷新 */
export class OSSSettingTab extends PluginSettingTab {
  /** 仓库下拉的轮询句柄，离开设置页或重新渲染时必须清理，否则计时器叠加 */
  private vaultRefreshTimer: number | null = null;

  /** 构造设置页，app 由基类持有，plugin 提供设置读写与业务操作入口 */
  constructor(app: App, private plugin: OSSPlugin) {
    super(app, plugin);
  }

  /** 重建整个设置页；未设置设备名时只渲染初始化分区并提前返回 */
  display(): void {
    this.stopVaultRefresh();
    const { containerEl } = this;
    containerEl.empty();

    const brand = containerEl.createDiv({ cls: "oss-settings-brand" });
    brand.createEl("img", { attr: { src: OSS_LOGO_DATA_URL, alt: "", width: "72", height: "28" } });
    new Setting(brand).setName("OSS Sync").setHeading();

    // 语言与服务端分区：界面语言、服务端地址与登录态，登录前后渲染不同表单

    new Setting(containerEl).setName(this.plugin.t("settings.language.title")).setHeading();

    new Setting(containerEl)
      .setName(this.plugin.t("settings.language.name"))
      .setDesc(this.plugin.t("settings.language.desc"))
      .addDropdown((dropdown) =>
        dropdown
          .addOption("auto", this.plugin.t("settings.language.auto"))
          .addOption("zh", this.plugin.t("settings.language.zh"))
          .addOption("en", this.plugin.t("settings.language.en"))
          .setValue(this.plugin.settings.language)
          .onChange(async (value) => {
            this.plugin.settings.language = parseLanguage(value);
            await this.plugin.saveSettings();
            this.plugin.refreshLocalizedUI();
            this.display();
          })
      );

    if (!this.plugin.settings.deviceName.trim()) {
      this.renderInitialDeviceName(containerEl);
      return;
    }

    new Setting(containerEl).setName(this.plugin.t("settings.server.title")).setHeading();

    new Setting(containerEl)
      .setName(this.plugin.t("settings.server.url"))
      .setDesc(this.plugin.t("settings.server.urlDesc"))
      .addText((text) =>
        text
          .setPlaceholder("http://localhost:8080")
          .setValue(this.plugin.settings.serverUrl)
          .onChange(async (value) => {
            this.plugin.settings.serverUrl = value.trim().replace(/\/$/, "");
            await this.plugin.saveSettings();
          })
      );

    if (this.plugin.isLoggedIn()) {
      new Setting(containerEl)
        .setName(this.plugin.t("settings.server.signedIn"))
        .setDesc(this.plugin.t("settings.server.signedInAs", { username: this.plugin.settings.username }))
        .addButton((button) =>
          button.setButtonText(this.plugin.t("settings.server.logoutButton")).setWarning().onClick(async () => {
            await this.plugin.logout();
            new Notice(this.plugin.t("notice.logoutSuccess"));
            this.display();
         })
        );
      new Setting(containerEl)
        .setName(this.plugin.t("settings.devices.thisDevice"))
        .setDesc(this.plugin.settings.deviceName);
    } else {
      this.renderLoggedOutDeviceName(containerEl);

      new Setting(containerEl)
        .setName(this.plugin.t("settings.server.username"))
        .addText((text) =>
          text
            .setValue(this.plugin.settings.username)
            .onChange(async (value) => {
              this.plugin.settings.username = value;
              await this.plugin.saveSettings();
            })
        );

      new Setting(containerEl)
        .setName(this.plugin.t("settings.server.password"))
        .addText((text) => {
          text.inputEl.type = "password";
          text
            .setValue(this.plugin.settings.password)
            .onChange((value) => {
              this.plugin.settings.password = value;
            });
        });

      const authSetting = new Setting(containerEl)
        .setName(this.plugin.t("settings.server.login"))
        .setDesc(this.plugin.t("settings.server.checking"))
        .addButton((btn) =>
          btn.setButtonText(this.plugin.t("settings.server.loginButton")).onClick(async () => {
            const serverUrlError = validateServerURL(this.plugin.settings.serverUrl);
            if (serverUrlError) {
              new Notice(this.plugin.t(serverUrlError === "protocol_required" ? "settings.server.protocolRequired" : "settings.server.invalidUrl"));
              return;
            }
            const error = this.validateCredentials();
            if (error) {
              new Notice(error);
              return;
            }
            try {
              const login = await this.plugin.login();
              let message = this.plugin.settings.vaultId
                ? this.plugin.t("notice.loginSuccess")
                : this.plugin.t("notice.loginSuccessNoVault");
              if (login.response.device_status === "pending") {
                message = this.plugin.t("notice.loginPending");
              }
              if (login.replacedRevokedIdentity) {
                message = this.plugin.t("notice.loginReregistered");
              }
              new Notice(message);
              this.display();
            } catch (e) {
              new Notice(this.plugin.t("notice.loginFailed", { error: this.errorMessage(e) }));
            }
          })
        )
        .addButton((btn) =>
          btn.setButtonText(this.plugin.t("settings.server.registrationButton")).onClick(() => {
            const serverUrlError = validateServerURL(this.plugin.settings.serverUrl);
            if (serverUrlError) {
              new Notice(this.plugin.t(serverUrlError === "protocol_required" ? "settings.server.protocolRequired" : "settings.server.invalidUrl"));
              return;
            }
            window.open(this.plugin.webURL("/register"), "_blank", "noopener,noreferrer");
          })
        );

      // 服务端地址可用时预取注册状态，据需要更新登录区的说明文案
      if (!validateServerURL(this.plugin.settings.serverUrl)) {
        void this.plugin.api.authStatus().then((status) => {
          if (status.needs_first_admin) {
            authSetting.setDesc(this.plugin.t("settings.server.needsAdmin"));
          } else if (status.registration_enabled) {
            authSetting.setDesc(this.plugin.t("settings.server.registrationOpen"));
          } else {
            authSetting.setDesc(this.plugin.t("settings.server.registrationClosed"));
          }
        }).catch((e: unknown) => {
          authSetting.setDesc(this.plugin.t("settings.server.statusUnavailable"));
          new Notice(this.plugin.t("notice.authStatusFailed", { error: this.errorMessage(e) }));
        });
      }
    }

    // 仓库分区：绑定已有仓库、下拉选项按 3 秒轮询刷新、以及新建仓库

    new Setting(containerEl).setName(this.plugin.t("settings.vault.title")).setHeading();

    const boundVaultSetting = new Setting(containerEl)
      .setName(this.plugin.t("settings.vault.bound"))
      .setDesc(this.plugin.t("settings.vault.boundDesc"))
      .addDropdown((dropdown) => {
        dropdown.addOption("", this.plugin.t("settings.vault.select"));
        dropdown.setValue(this.plugin.settings.vaultId);
        dropdown.onChange(async (vaultID) => {
          const vault = this.plugin.availableVaults.find((item) => item.id === vaultID);
          if (!vault) return;
          try {
            const synced = await this.plugin.bindVault(vault);
            new Notice(this.plugin.t(synced ? "notice.vaultBound" : "notice.vaultBoundPartial", { name: vault.name }));
          } catch (error: unknown) {
            new Notice(this.plugin.t("notice.vaultBindFailed", { error: this.errorMessage(error) }));
          }
        });
        const applyVaultOptions = (vaults: Awaited<ReturnType<OSSPlugin["refreshVaults"]>>): void => {
          while (dropdown.selectEl.options.length > 0) dropdown.selectEl.remove(0);
          dropdown.addOption("", vaults.length === 0
            ? this.plugin.t("settings.vault.none")
            : this.plugin.t("settings.vault.select"));
          boundVaultSetting.setDesc(
            vaults.length === 0
              ? this.plugin.t("settings.vault.noneDesc")
              : this.plugin.t("settings.vault.boundDesc")
          );
          for (const vault of vaults) {
            dropdown.addOption(
              vault.id,
              vault.is_default
                ? `${vault.name} (${this.plugin.t("settings.vault.defaultSuffix")})`
                : vault.name
            );
          }
          const selected = vaults.some((vault) => vault.id === this.plugin.settings.vaultId)
            ? this.plugin.settings.vaultId
            : "";
          dropdown.setValue(selected);
        };
        // 轮询刷新仓库选项，失败时静默保留现有选项，等下一轮重试
        const refreshVaultOptions = async (): Promise<void> => {
          if (!this.plugin.isLoggedIn()) return;
          try {
            const vaults = await this.plugin.refreshVaults();
            applyVaultOptions(vaults);
            // 登录后未绑定（例如绑定请求曾失败）时自动落到默认仓库
            if (!this.plugin.settings.vaultId && vaults.length > 0) {
              await this.plugin.ensureVaultBinding();
              applyVaultOptions(await this.plugin.refreshVaults());
            }
          } catch {
            // 保留现有选项，下一次轮询继续尝试
          }
        };
        // 3 秒轮询覆盖登录成功后仓库列表到位的时间差，重渲染前由 stopVaultRefresh 清理
        void refreshVaultOptions();
        this.vaultRefreshTimer = window.setInterval(() => {
          void refreshVaultOptions();
        }, 3000);
      });
    let newVaultName = "";
    new Setting(containerEl)
      .setName(this.plugin.t("settings.vault.create"))
      .setDesc(this.plugin.t("settings.vault.createDesc"))
      .addText((text) =>
        text.setPlaceholder(this.plugin.t("settings.vault.namePlaceholder")).onChange((value) => {
          newVaultName = value.trim();
        })
      )
      .addButton((button) =>
        button.setButtonText(this.plugin.t("settings.vault.createButton")).onClick(async () => {
          if (!newVaultName) {
            new Notice(this.plugin.t("notice.vaultNameRequired"));
            return;
          }
          button.setDisabled(true);
          try {
            const vault = await this.plugin.api.createVault(newVaultName);
            await this.plugin.refreshVaults();
            const synced = await this.plugin.bindVault(vault);
            new Notice(this.plugin.t(synced ? "notice.vaultCreated" : "notice.vaultCreatedPartial", { name: vault.name }));
            this.display();
          } catch (error: unknown) {
            new Notice(this.plugin.t("notice.vaultCreateFailed", { error: this.errorMessage(error) }));
          } finally {
            button.setDisabled(false);
          }
        })
      );

    // 设备分区：列出当前账号下各设备的同步游标，仅作只读展示

    new Setting(containerEl).setName(this.plugin.t("settings.devices.title")).setHeading();

    const devicesEl = containerEl.createDiv({ cls: "oss-device-list" });
    devicesEl.setText(this.plugin.t("settings.devices.loading"));
    void this.plugin.api.listDevices().then((result) => {
      devicesEl.empty();
      for (const device of result.devices) {
        const cursorSummary = device.vaults.length > 0
          ? device.vaults
              .map((vault) => `${vault.vault_name}: ${vault.last_cursor}/${vault.head_revision}`)
              .join(" · ")
          : this.plugin.t("common.noVaultSync");
        const state = this.plugin.t(device.stale ? "common.expired" : "common.valid");
        const lastSeen = device.last_seen_at
          ? new Date(device.last_seen_at).toLocaleString()
          : this.plugin.t("common.unknown");
        new Setting(devicesEl)
          .setName(`${device.name || this.plugin.t("settings.devices.unnamed")}${device.is_current ? ` (${this.plugin.t("settings.devices.currentSuffix")})` : ""}`)
          .setDesc(this.plugin.t("settings.devices.summary", { state, lastSeen, cursor: cursorSummary }));
      }
      if (result.devices.length === 0) {
        devicesEl.setText(this.plugin.t("settings.devices.none"));
      }
    }).catch((error: unknown) => {
      devicesEl.setText(this.plugin.t("settings.devices.loadFailed"));
      new Notice(this.plugin.t("notice.devicesLoadFailed", { error: this.errorMessage(error) }));
    });

    // 同步分区：间隔、轮询模式、并发、黑名单与诊断开关，均即时保存
    // 涉及运行时机参数的项在保存后立即作用于同步引擎，不等待重启

    new Setting(containerEl).setName(this.plugin.t("settings.sync.title")).setHeading();

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.interval"))
      .setDesc(this.plugin.t("settings.sync.intervalDesc"))
      .addText((text) =>
        text
          .setPlaceholder("3")
          .setValue(String(this.plugin.settings.syncIntervalSec))
          .onChange(async (value) => {
            const n = parseInt(value, 10);
            if (!isNaN(n) && n >= 1) {
              this.plugin.settings.syncIntervalSec = n;
              await this.plugin.saveSettings();
              this.plugin.syncEngine.resetDebounce();
            }
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.mode"))
      .setDesc(this.plugin.t("settings.sync.modeDesc"))
      .addDropdown((dropdown) =>
        dropdown
          .addOption("short_poll", this.plugin.t("common.shortPoll"))
          .addOption("long_poll", this.plugin.t("common.longPoll"))
          .setValue(this.plugin.settings.vaultSyncMode)
          .onChange(async (value) => {
            this.plugin.settings.vaultSyncMode = value as "short_poll" | "long_poll";
            await this.plugin.saveSettings();
            await this.plugin.syncEngine.refreshPollingStrategy();
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.maxConcurrency"))
      .setDesc(this.plugin.t("settings.sync.maxConcurrencyDesc"))
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
      .setName(this.plugin.t("settings.sync.incremental"))
      .setDesc(this.plugin.t("settings.sync.incrementalDesc"))
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.incrementalCheck)
          .onChange(async (value) => {
            this.plugin.settings.incrementalCheck = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.remoteInterval"))
      .setDesc(this.plugin.t("settings.sync.remoteIntervalDesc"))
      .addText((text) =>
        text
          .setPlaceholder("3")
          .setValue(String(this.plugin.settings.remotePollIntervalSec))
          .onChange(async (value) => {
            const n = parseInt(value, 10);
            if (!isNaN(n) && n >= 3) {
              this.plugin.settings.remotePollIntervalSec = n;
              await this.plugin.saveSettings();
              this.plugin.syncEngine.resetPolling();
            }
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.forceSSE"))
      .setDesc(this.plugin.t("settings.sync.forceSSEDesc"))
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.forceSSE)
          .onChange(async (value) => {
            // SSE 连接方式需重建协作管理器才能生效，仅在运行中才重启
            this.plugin.settings.forceSSE = value;
            await this.plugin.saveSettings();
            if (this.plugin.collabManager.isRunning()) {
              this.plugin.collabManager.stop();
              this.plugin.collabManager.start();
            }
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.diagnostics"))
      .setDesc(this.plugin.t("settings.sync.diagnosticsDesc"))
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.diagnosticsEnabled)
          .onChange(async (value) => {
            this.plugin.settings.diagnosticsEnabled = value;
            await this.plugin.saveSettings();
            this.plugin.emitDiagnosticsEnabled(value);
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.conflictEditWarning"))
      .setDesc(this.plugin.t("settings.sync.conflictEditWarningDesc"))
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.conflictEditWarning)
          .onChange(async (value) => {
            this.plugin.settings.conflictEditWarning = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName(this.plugin.t("settings.sync.forceFull"))
      .setDesc(this.plugin.t("settings.sync.forceFullDesc"))
      .addButton((btn) =>
        btn.setButtonText(this.plugin.t("settings.sync.syncNow")).onClick(async () => {
          const synced = await this.plugin.syncEngine.runOnce({ forceFull: true });
          if (!synced) new Notice(this.plugin.t("notice.syncIncomplete"));
        })
      );

    renderShareGuideEntry(containerEl, this.app, this.plugin);

    this.renderUpdateSection(containerEl);
    if (this.plugin.isAdmin()) {
      this.renderServerUpdateSection(containerEl);
    }
  }

  /** 设置页隐藏时停止仓库轮询，避免后台持续请求 */
  hide(): void {
    this.stopVaultRefresh();
    super.hide();
  }

  /** 清理仓库下拉轮询句柄 */
  private stopVaultRefresh(): void {
    if (this.vaultRefreshTimer !== null) {
      window.clearInterval(this.vaultRefreshTimer);
      this.vaultRefreshTimer = null;
    }
  }

  /** 未登录状态下渲染设备名输入，保存后写入设置并刷新页面 */
  private renderLoggedOutDeviceName(containerEl: HTMLElement): void {
    let deviceName = this.plugin.settings.deviceName;
    new Setting(containerEl)
      .setName(this.plugin.t("settings.devices.thisDevice"))
      .setDesc(this.plugin.t("settings.devices.thisDeviceDesc"))
      .addText((text) => text
        .setValue(deviceName)
        .onChange((value) => {
          deviceName = value.trim();
        }))
      .addButton((button) => button
        .setButtonText(this.plugin.t("common.save"))
        .onClick(async () => {
          if (!deviceName) {
            new Notice(this.plugin.t("notice.deviceNameRequired"));
            return;
          }
          try {
            await this.plugin.setDeviceName(deviceName);
            new Notice(this.plugin.t("notice.deviceNameSaved"));
            this.display();
          } catch (error: unknown) {
            new Notice(this.errorMessage(error));
          }
        }));
  }

  /** 首次使用时的设备名初始化分区，未填写设备名时其余设置项不渲染 */
  private renderInitialDeviceName(containerEl: HTMLElement): void {
    new Setting(containerEl).setName(this.plugin.t("settings.devices.setupTitle")).setHeading();
    let deviceName = this.plugin.settings.deviceName;
    new Setting(containerEl)
      .setName(this.plugin.t("settings.devices.setupName"))
      .setDesc(this.plugin.t("settings.devices.setupDesc"))
      .addText((text) => text.setValue(deviceName).onChange((value) => {
        deviceName = value.trim();
      }))
      .addButton((button) => button.setButtonText(this.plugin.t("common.save")).onClick(async () => {
        if (!deviceName) {
          new Notice(this.plugin.t("notice.deviceNameRequired"));
          return;
        }
        try {
          await this.plugin.setDeviceName(deviceName);
          new Notice(this.plugin.t("notice.deviceNameSaved"));
          this.display();
        } catch (error: unknown) {
          new Notice(this.errorMessage(error));
        }
      }));
  }

  private renderUpdateSection(containerEl: HTMLElement): void {
    new Setting(containerEl).setName(this.plugin.t("settings.update.title")).setHeading();
    new Setting(containerEl)
      .setName(this.plugin.t("settings.update.currentVersion", { version: this.plugin.manifest.version }))
      .setDesc(this.plugin.t("settings.update.marketplace"));
  }

  /** 服务端更新分区：状态检查与触发更新串行执行，更新期间禁用检查与触发按钮 */
  private renderServerUpdateSection(containerEl: HTMLElement): void {
    new Setting(containerEl).setName(this.plugin.t("settings.serverUpdate.title")).setHeading();
    containerEl.createEl("p", { text: this.plugin.t("settings.serverUpdate.desc"), cls: "setting-item-description" });


    let currentVersion = this.plugin.t("common.unknown");
    let latestVersion: string | null = null;
    let checkId: string | null = null;
    let candidateVersion: string | null = null;
    let isChecking = false;
    let isTriggering = false;
    let isPolling = false;

    const versionSetting = new Setting(containerEl)
      .setName(this.plugin.t("settings.serverUpdate.statusLabel"))
      .setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));

    const statusSetting = new Setting(containerEl)
      .setName(this.plugin.t("settings.serverUpdate.statusLabel"))
      .setDesc(this.plugin.t("settings.serverUpdate.statusIdle"));

    let checkButton: ButtonComponent | null = null;
    let triggerButton: ButtonComponent | null = null;

    const handleAuthError = (error: unknown): void => {
      statusSetting.setDesc(this.plugin.t("settings.serverUpdate.authError"));
      new Notice(this.plugin.t("notice.serverUpdateAuthFailed", { error: this.errorMessage(error) }));
    };

    const isAuthError = (error: unknown): boolean => {
      const anyErr = error as { status?: number };
      return anyErr && (anyErr.status === 401 || anyErr.status === 403);
    };

    const refreshTrigger = (): void => {
      if (!triggerButton) return;
      if (candidateVersion && checkId) {
        triggerButton.buttonEl.show();
        triggerButton.setButtonText(`更新版本到 ${candidateVersion}`);
        triggerButton.setDisabled(isTriggering || isPolling || isChecking);
      } else {
        triggerButton.buttonEl.hide();
      }
    };


    const doTrigger = async (): Promise<void> => {
      if (!checkId || !candidateVersion) return;
      if (!await ConfirmModal.ask(this.app, `确认将服务端更新到 ${candidateVersion}？`, `更新到 ${candidateVersion}`, this.plugin.t("common.cancel"))) return;
      if (isTriggering || isPolling) return;
      isTriggering = true;
      triggerButton?.setDisabled(true);
      checkButton?.setDisabled(true);
      statusSetting.setDesc(this.plugin.t("settings.serverUpdate.triggering"));
      try {
        const trigger = await this.plugin.triggerServerUpdate({ checkId, expectedVersion: candidateVersion });
        if (!trigger.ok) throw new Error(trigger.error ?? trigger.code);
        isTriggering = false;
        isPolling = true;
        const poller = this.plugin.createServerUpdatePoller({ expectedVersion: candidateVersion! });
        const outcome = await poller.poll();
        isPolling = false;
        if (outcome.kind === "success") {
          currentVersion = outcome.version;
          versionSetting.setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.success", { version: outcome.version }));
          new Notice(this.plugin.t("notice.serverUpdateSuccess", { version: outcome.version }));
        } else if (outcome.kind === "rolled_back") {
          currentVersion = outcome.currentVersion;
          versionSetting.setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.rolledBack", { version: outcome.currentVersion }));
          new Notice(this.plugin.t("notice.serverUpdateRolledBack", { version: outcome.currentVersion }));
        } else if (outcome.kind === "failed") {
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.failed", { state: outcome.state, error: outcome.error }));
          new Notice(this.plugin.t("notice.serverUpdateFailed", { error: outcome.error }));
        } else if (outcome.kind === "timeout") {
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.timeout", { count: String(outcome.attempts) }));
          new Notice(this.plugin.t("notice.serverUpdateFailed", { error: this.plugin.t("settings.serverUpdate.timeout", { count: String(outcome.attempts) }) }));
        } else if (outcome.kind === "auth_error") {
          handleAuthError(outcome.error);
        }
        checkButton?.setDisabled(false);
        refreshTrigger();
      } catch (error: unknown) {
        isTriggering = false;
        isPolling = false;
        if (isAuthError(error)) handleAuthError(error);
        else {
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.triggerFailed", { error: this.errorMessage(error) }));
          new Notice(this.plugin.t("notice.serverUpdateFailed", { error: this.errorMessage(error) }));
        }
        checkButton?.setDisabled(false);
        refreshTrigger();
      }
    };

    void this.plugin.getServerVersion().then((info) => {
      currentVersion = info.version ?? currentVersion;
      versionSetting.setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));
    }).catch((error: unknown) => {
      if (isAuthError(error)) {
        versionSetting.setDesc(this.plugin.t("settings.serverUpdate.staleRole"));
        handleAuthError(error);
      }
    });

    new Setting(containerEl)
      .setName(this.plugin.t("settings.serverUpdate.check"))
      .setDesc(this.plugin.t("settings.serverUpdate.statusIdle"))
      .addButton((button) => {
        checkButton = button;
        button.setButtonText(this.plugin.t("settings.serverUpdate.check")).onClick(async () => {
          if (isChecking || isTriggering || isPolling) return;
          isChecking = true;
          button.setDisabled(true);
          triggerButton?.setDisabled(true);
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.statusChecking"));
          try {
            const check = await this.plugin.checkServerUpdate();
            currentVersion = check.current_version;
            latestVersion = check.latest_version;
            checkId = check.check_id;
            candidateVersion = check.candidate?.version ?? latestVersion;
            versionSetting.setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));
            if (check.update_available && candidateVersion && checkId) {
              statusSetting.setDesc(this.plugin.t("settings.serverUpdate.available", { from: currentVersion, to: candidateVersion }));
              refreshTrigger();
            } else {
              statusSetting.setDesc(this.plugin.t("settings.serverUpdate.noUpdate", { version: currentVersion }));
              candidateVersion = null;
              checkId = null;
              refreshTrigger();
            }
          } catch (error: unknown) {
            if (isAuthError(error)) handleAuthError(error);
            else statusSetting.setDesc(this.plugin.t("settings.serverUpdate.checkFailed", { error: this.errorMessage(error) }));
          } finally {
            isChecking = false;
            button.setDisabled(false);
            if (!isTriggering && !isPolling) refreshTrigger();
          }
        });
      })
      .addButton((button) => {
        triggerButton = button;
        button.setButtonText("更新版本").setCta();
        button.buttonEl.hide();
        button.onClick(doTrigger);
      });

    void this.plugin.getServerUpdateStatus().then((s) => {
      const state = s.state ?? s.last_update?.state ?? "idle";
      currentVersion = s.version ?? currentVersion;
      versionSetting.setDesc(this.plugin.t("settings.serverUpdate.current", { version: currentVersion }));
      if (s.last_update?.state) {
        statusSetting.setDesc(`${state}: ${s.last_update.phase ?? s.last_update.state}`);
      }
    }).catch((error: unknown) => {
      if (isAuthError(error)) handleAuthError(error);
    });
  }

  /** 校验登录表单，返回可直接提示的错误文案，通过时返回 null */
  private validateCredentials(): string | null {
    const error = validateLoginCredentials(
      this.plugin.settings.username,
      this.plugin.settings.password
    );
    switch (error) {
      case "username_required":
        return this.plugin.t("settings.server.usernameRequired");
      case "password_required":
        return this.plugin.t("settings.server.passwordRequired");
      default:
        return null;
    }
  }

  /** 把异常转为本语言可读文案，设置页提示统一入口 */
  private errorMessage(error: unknown): string {
    return this.plugin.localizedError(error);
  }
}

/** 把下拉值解析为受支持的语言设置，未知值回落为 auto */
function parseLanguage(value: string): OSSSettings["language"] {
  switch (value) {
    case "zh":
      return "zh";
    case "en":
      return "en";
    default:
      return "auto";
  }
}
