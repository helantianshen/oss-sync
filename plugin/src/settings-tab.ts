import { ConfirmModal } from "./confirm-modal";
import { App, ButtonComponent, Notice, PluginSettingTab, requireApiVersion, Setting } from "obsidian";
import type { SettingDefinitionEmpty, SettingDefinitionGroup, SettingDefinitionRender } from "obsidian";
import type OSSPlugin from "./main";
import type { OSSSettings } from "./settings";
import { validateLoginCredentials, validateServerURL } from "./login-state";
import { OSS_LOGO_DATA_URL } from "./brand";
import { addShareGuideButton } from "./share-guide-modal";

type RenderSetting = (setting: Setting) => void | (() => void);
type SettingRow = (Omit<SettingDefinitionRender, "render"> & { render: RenderSetting }) | SettingDefinitionEmpty;
type SettingsSection = SettingDefinitionGroup & { items: SettingRow[] };

/** 插件设置页：按语言、服务端、仓库、设备、同步与更新分组渲染表单，并维护仓库下拉的定时刷新 */
export class OSSSettingTab extends PluginSettingTab {
  private cancelVaultRefresh: (() => void) | null = null;

  /** 构造设置页，app 由基类持有，plugin 提供设置读写与业务操作入口 */
  constructor(app: App, private plugin: OSSPlugin) {
    super(app, plugin);
  }

  /** Obsidian 1.13+ 在注册时索引这些定义，控件和请求只在渲染时创建。 */
  getSettingDefinitions(): SettingsSection[] {
    let items: SettingRow[] = [];
    const sections: SettingsSection[] = [{ type: "group", items }];
    const heading = (name: string): void => {
      items = [];
      sections.push({ type: "group", heading: name, items });
    };
    const row = (
      name: string,
      desc?: string,
      render?: RenderSetting,
    ): void => {
      items.push(render ? { name, desc, render } : { name, desc });
    };

    items.push({
      name: "OSS Sync",
      searchable: false,
      render: (setting) => {
        const brand = this.createSettingsBlock(setting).createDiv({ cls: "oss-settings-brand" });
        brand.createEl("img", { attr: { src: OSS_LOGO_DATA_URL, alt: "", width: "72", height: "28" } });
        new Setting(brand).setName("OSS Sync").setHeading();
      },
    });

    // 语言与服务端分区：界面语言、服务端地址与登录态，登录前后渲染不同表单

    heading(this.plugin.t("settings.language.title"));

    row(this.plugin.t("settings.language.name"), this.plugin.t("settings.language.desc"), (setting) => {
      setting
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
              this.refreshSettings();
            })
        );
    });

    if (!this.plugin.settings.deviceName.trim()) {
      heading(this.plugin.t("settings.devices.setupTitle"));
      row(this.plugin.t("settings.devices.setupName"), this.plugin.t("settings.devices.setupDesc"),
        (setting) => this.renderDeviceName(setting));
      return sections;
    }

    heading(this.plugin.t("settings.server.title"));

    row(this.plugin.t("settings.server.url"), this.plugin.t("settings.server.urlDesc"), (setting) => {
      setting
        .addText((text) =>
          text
            .setPlaceholder("http://localhost:8080")
            .setValue(this.plugin.settings.serverUrl)
            .onChange(async (value) => {
              this.plugin.settings.serverUrl = value.trim().replace(/\/$/, "");
              await this.plugin.saveSettings();
            })
        );
    });

    if (this.plugin.isLoggedIn()) {
      row(this.plugin.t("settings.server.signedIn"), this.plugin.t("settings.server.signedInAs", { username: this.plugin.settings.username }), (setting) => {
        setting
          .addButton((button) =>
            button.setButtonText(this.plugin.t("settings.server.logoutButton")).setWarning().onClick(async () => {
              await this.plugin.logout();
              new Notice(this.plugin.t("notice.logoutSuccess"));
              this.refreshSettings();
            })
          );
      });
      row(this.plugin.t("settings.devices.thisDevice"), this.plugin.settings.deviceName);
    } else {
      row(this.plugin.t("settings.devices.thisDevice"), this.plugin.t("settings.devices.thisDeviceDesc"),
        (setting) => this.renderDeviceName(setting));

      row(this.plugin.t("settings.server.username"), undefined, (setting) => {
        setting
          .addText((text) =>
            text
              .setValue(this.plugin.settings.username)
              .onChange(async (value) => {
                this.plugin.settings.username = value;
                await this.plugin.saveSettings();
              })
          );
      });

      row(this.plugin.t("settings.server.password"), undefined, (setting) => {
        setting
          .addText((text) => {
            text.inputEl.type = "password";
            text
              .setValue(this.plugin.settings.password)
              .onChange((value) => {
                this.plugin.settings.password = value;
              });
          });
      });

      row(this.plugin.t("settings.server.login"), this.plugin.t("settings.server.checking"), (authSetting) => {
        authSetting
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
                this.refreshSettings();
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
      });
    }

    // 仓库分区：绑定已有仓库、下拉选项按 3 秒轮询刷新、以及新建仓库

    heading(this.plugin.t("settings.vault.title"));

    row(this.plugin.t("settings.vault.bound"), this.plugin.t("settings.vault.boundDesc"), (setting) => {
      let active = true;
      this.stopVaultRefresh();
      setting.addDropdown((dropdown) => {
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
          setting.setDesc(
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
            if (!active) return;
            applyVaultOptions(vaults);
            // 登录后未绑定（例如绑定请求曾失败）时自动落到默认仓库
            if (!this.plugin.settings.vaultId && vaults.length > 0) {
              await this.plugin.ensureVaultBinding();
              const refreshed = await this.plugin.refreshVaults();
              if (active) applyVaultOptions(refreshed);
            }
          } catch {
            // 保留现有选项，下一次轮询继续尝试
          }
        };
        // 3 秒轮询覆盖登录成功后仓库列表到位的时间差。
        void refreshVaultOptions();
        const timer = window.setInterval(() => {
          void refreshVaultOptions();
        }, 3000);
        this.cancelVaultRefresh = () => {
          active = false;
          window.clearInterval(timer);
        };
      });
      const cancel = this.cancelVaultRefresh;
      return () => {
        if (this.cancelVaultRefresh === cancel) this.stopVaultRefresh();
      };
    });

    row(this.plugin.t("settings.vault.create"), this.plugin.t("settings.vault.createDesc"), (setting) => {
      let newVaultName = "";
      setting
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
              this.refreshSettings();
            } catch (error: unknown) {
              new Notice(this.plugin.t("notice.vaultCreateFailed", { error: this.errorMessage(error) }));
            } finally {
              button.setDisabled(false);
            }
          })
        );
    });

    // 设备分区：列出当前账号下各设备的同步游标，仅作只读展示

    heading(this.plugin.t("settings.devices.title"));

    row(this.plugin.t("settings.devices.title"), undefined, (setting) => {
      this.renderDeviceList(setting);
    });

    // 同步分区：间隔、轮询模式、并发、黑名单与诊断开关，均即时保存
    // 涉及运行时机参数的项在保存后立即作用于同步引擎，不等待重启

    heading(this.plugin.t("settings.sync.title"));

    row(this.plugin.t("settings.sync.configuration"), this.plugin.t("settings.sync.configurationDesc"), (setting) => {
      setting.addToggle((toggle) => toggle.setValue(this.plugin.settings.syncConfiguration).onChange(async (enabled) => {
        this.plugin.settings.syncConfiguration = enabled;
        await this.applySyncPathSettings();
      }));
    });

    row(this.plugin.t("settings.sync.autoResolveConfiguration"), this.plugin.t("settings.sync.autoResolveConfigurationDesc"), (setting) => {
      setting.addToggle((toggle) => toggle.setValue(this.plugin.settings.autoResolveConfiguration).onChange(async (enabled) => {
        this.plugin.settings.autoResolveConfiguration = enabled;
        await this.applySyncPathSettings();
      }));
    });

    row(this.plugin.t("settings.sync.communityPlugins"), this.plugin.t("settings.sync.communityPluginsDesc"), (setting) => {
      setting.addToggle((toggle) => toggle.setValue(this.plugin.settings.syncCommunityPlugins).onChange(async (enabled) => {
        this.plugin.settings.syncCommunityPlugins = enabled;
        await this.applySyncPathSettings();
      }));
    });

    for (const { field, name, desc } of [
      { field: "syncExclusions", name: "settings.sync.exclusions", desc: "settings.sync.exclusionsDesc" },
      { field: "syncWhitelist", name: "settings.sync.whitelist", desc: "settings.sync.whitelistDesc" },
    ] as const) {
      row(this.plugin.t(name), this.plugin.t(desc), (setting) => {
        let value = this.plugin.settings[field];
        setting.addTextArea((text) => text.setValue(value).onChange((draft) => { value = draft; }));
        setting.addButton((button) => button.setButtonText(this.plugin.t("common.save")).onClick(async () => {
          this.plugin.settings[field] = value.trim();
          await this.applySyncPathSettings();
        }));
      });
    }

    row(this.plugin.t("settings.sync.interval"), this.plugin.t("settings.sync.intervalDesc"), (setting) => {
      setting
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
    });

    row(this.plugin.t("settings.sync.mode"), this.plugin.t("settings.sync.modeDesc"), (setting) => {
      setting
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
    });

    row(this.plugin.t("settings.sync.maxConcurrency"), this.plugin.t("settings.sync.maxConcurrencyDesc"), (setting) => {
      setting
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
    });

    row(this.plugin.t("settings.sync.incremental"), this.plugin.t("settings.sync.incrementalDesc"), (setting) => {
      setting
        .addToggle((toggle) =>
          toggle
            .setValue(this.plugin.settings.incrementalCheck)
            .onChange(async (value) => {
              this.plugin.settings.incrementalCheck = value;
              await this.plugin.saveSettings();
            })
        );
    });

    row(this.plugin.t("settings.sync.remoteInterval"), this.plugin.t("settings.sync.remoteIntervalDesc"), (setting) => {
      setting
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
    });

    row(this.plugin.t("settings.sync.forceSSE"), this.plugin.t("settings.sync.forceSSEDesc"), (setting) => {
      setting
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
    });

    row(this.plugin.t("settings.sync.diagnostics"), this.plugin.t("settings.sync.diagnosticsDesc"), (diagnosticsSetting) => {
      diagnosticsSetting.setClass("oss-diagnostics-setting")
        .addToggle((toggle) =>
          toggle
            .setValue(this.plugin.settings.diagnosticsEnabled)
            .onChange(async (value) => {
              this.plugin.settings.diagnosticsEnabled = value;
              await this.plugin.saveSettings();
              this.plugin.emitDiagnosticsEnabled(value);
            })
        );

      const diagnosticActions = diagnosticsSetting.settingEl.createDiv({ cls: "oss-diagnostics-actions" });
      const exportButton = new ButtonComponent(diagnosticActions)
        .setButtonText(this.plugin.t("settings.diagnostics.exportButton"))
        .onClick(async () => {
          exportButton.setDisabled(true);
          try {
            await this.plugin.exportDiagnosticLogs();
          } catch (error) {
            new Notice(this.plugin.t("settings.diagnostics.failed", { error: this.plugin.localizedError(error) }), 10000);
          } finally {
            exportButton.setDisabled(false);
          }
        });
      new ButtonComponent(diagnosticActions)
        .setButtonText(this.plugin.t("settings.diagnostics.clear"))
        .onClick(() => this.plugin.clearDiagnosticLogs());
      return () => diagnosticActions.remove();
    });

    row(this.plugin.t("settings.sync.conflictEditWarning"), this.plugin.t("settings.sync.conflictEditWarningDesc"), (setting) => {
      setting
        .addToggle((toggle) =>
          toggle
            .setValue(this.plugin.settings.conflictEditWarning)
            .onChange(async (value) => {
              this.plugin.settings.conflictEditWarning = value;
              await this.plugin.saveSettings();
            })
        );
    });

    row(this.plugin.t("settings.sync.forceFull"), this.plugin.t("settings.sync.forceFullDesc"), (setting) => {
      setting
        .addButton((btn) =>
          btn.setButtonText(this.plugin.t("settings.sync.syncNow")).onClick(async () => {
            const synced = await this.plugin.syncEngine.runOnce({ forceFull: true });
            if (!synced) new Notice(this.plugin.t("notice.syncIncomplete"));
          })
        );
    });

    row(this.plugin.t("shareGuide.title"), this.plugin.t("shareGuide.entry"), (setting) => {
      addShareGuideButton(setting, this.app, this.plugin);
    });

    heading(this.plugin.t("settings.update.title"));
    row(this.plugin.t("settings.update.currentVersion", { version: this.plugin.manifest.version }),
      this.plugin.t("settings.update.marketplace"));
    if (this.plugin.isAdmin()) {
      heading(this.plugin.t("settings.serverUpdate.title"));
      row(this.plugin.t("settings.serverUpdate.check"), this.plugin.t("settings.serverUpdate.desc"), (setting) => {
        this.renderServerUpdateSection(this.createSettingsBlock(setting));
      });
    }
    return sections;
  }

  /** 较旧宿主用同一份定义渲染，避免两套设置逻辑产生差异。 */
  display(): void {
    this.renderLegacySettings();
  }

  private renderLegacySettings(): void {
    this.stopVaultRefresh();
    this.containerEl.empty();
    for (const section of this.getSettingDefinitions()) {
      if (section.heading) new Setting(this.containerEl).setName(section.heading).setHeading();
      for (const definition of section.items) {
        const setting = new Setting(this.containerEl).setName(definition.name);
        if (definition.desc) setting.setDesc(definition.desc);
        definition.render?.(setting);
      }
    }
  }

  private refreshSettings(): void {
    if (requireApiVersion("1.13.0")) this.update();
    else this.renderLegacySettings();
  }

  private async applySyncPathSettings(): Promise<void> {
    await this.plugin.saveSettings();
    this.plugin.syncEngine.resetPolling();
    if (this.plugin.isLoggedIn() && this.plugin.settings.vaultId) {
      void this.plugin.syncEngine.runOnce({ forceFull: true });
    }
  }

  private createSettingsBlock(setting: Setting): HTMLElement {
    setting.setClass("oss-settings-block");
    return setting.controlEl.createDiv();
  }

  private renderDeviceList(setting: Setting): void {
    const devicesEl = this.createSettingsBlock(setting).createDiv({ cls: "oss-device-list" });
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
  }

  /** 设置页隐藏时停止仓库轮询，避免后台持续请求 */
  hide(): void {
    this.stopVaultRefresh();
    super.hide();
  }

  /** 清理仓库下拉轮询句柄 */
  private stopVaultRefresh(): void {
    this.cancelVaultRefresh?.();
    this.cancelVaultRefresh = null;
  }

  /** 首次初始化和未登录时复用设备名输入。 */
  private renderDeviceName(setting: Setting): void {
    let deviceName = this.plugin.settings.deviceName;
    setting
      .addText((text) => text.setValue(deviceName).onChange((value) => {
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
            this.refreshSettings();
          } catch (error: unknown) {
            new Notice(this.errorMessage(error));
          }
        }));
  }

  /** 服务端更新分区：状态检查与触发更新串行执行，更新期间禁用检查与触发按钮 */
  private renderServerUpdateSection(containerEl: HTMLElement): void {
    let currentVersion = this.plugin.t("common.unknown");
    let latestVersion: string | null = null;
    let checkId: string | null = null;
    let candidateVersion: string | null = null;
    let isChecking = false;
    let isTriggering = false;
    let isPolling = false;

    const versionSetting = new Setting(containerEl)
      .setName(this.plugin.t("settings.serverUpdate.currentLabel"))
      .setDesc(currentVersion);

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
          versionSetting.setDesc(currentVersion);
          statusSetting.setDesc(this.plugin.t("settings.serverUpdate.success", { version: outcome.version }));
          new Notice(this.plugin.t("notice.serverUpdateSuccess", { version: outcome.version }));
        } else if (outcome.kind === "rolled_back") {
          currentVersion = outcome.currentVersion;
          versionSetting.setDesc(currentVersion);
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
      versionSetting.setDesc(currentVersion);
    }).catch((error: unknown) => {
      if (isAuthError(error)) {
        versionSetting.setDesc(this.plugin.t("settings.serverUpdate.staleRole"));
        handleAuthError(error);
      }
    });

    new Setting(containerEl)
      .setName(this.plugin.t("settings.serverUpdate.check"))
      .setDesc(this.plugin.t("settings.serverUpdate.desc"))
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
            versionSetting.setDesc(currentVersion);
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
      versionSetting.setDesc(currentVersion);
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
