# OSS Sync 项目事实与源码导航

以源码和实际验证为准。此文件记录稳定事实与长期协作约束，现有视觉系统见 [DESIGN.md](DESIGN.md)。需要交接的任务进度写入本地 `tasks/`，该目录不纳入 Git。

## 协作与修改约束

- 后端入口为 [main.go](../cmd/server/main.go)，路由集中在 [server.go](../internal/server/server.go)；Obsidian 插件源码位于 [plugin/src](../plugin/src/)，测试位于 [plugin/tests](../plugin/tests/)。
- 修改同步写路径时必须同时检查 Vault 隔离、设备授权、单调 revision、`operation_id` 幂等、`synclock` 并发约束，以及数据库、正文、历史快照和失败恢复的一致性。
- 修改模型时同步检查 [database.go](../internal/database/database.go) 的迁移与回填，并兼容 SQLite 和 PostgreSQL。
- 插件源码只修改 [plugin/src](../plugin/src/)；`plugin/main.js` 是忽略的构建产物，不提交。
- 提交涉及 Obsidian 插件实际更新的修改时，必须按 [插件版本与更新发布规则](rules/plugin-versioning.md) 同步提升并核对插件版本元数据；Release 资源核验后更新根目录清单公告，确保社区市场能发现并下载对应版本。
- [Dockerfile](../Dockerfile) 只构建后端镜像；[docker-compose.yml](../docker-compose.yml) 使用 SQLite 命名卷。容器升级通过替换镜像完成。
- 不提交 `data/`、数据库、日志、构建产物或密钥。新增配置字段时同步检查开发/生产 YAML、环境变量覆盖和校验逻辑。
- 优先复用现有 Handler、策略、路径校验和锁；修改保持在需求范围内，避免无关重构和新增依赖。
- `.agent/PROJECT.md` 和 `.agent/DESIGN.md` 纳入版本控制；多阶段任务状态写入被忽略的 `.agent/tasks/`。简单一次性修改和纯只读分析不强制创建任务记录。

## 结构与入口

| 范围 | 源码入口与职责 |
| --- | --- |
| 后端 | [go.mod](../go.mod) 声明 Go 1.25；使用 Gin、GORM。[main.go](../cmd/server/main.go) 负责启动、迁移、存储对账、更新恢复和优雅关闭。 |
| 路由 | [server.go](../internal/server/server.go) 组装各模块 Handler；具体路径还分布在各模块的 `Register` 方法中。 |
| 数据库 | [database.go](../internal/database/database.go) 支持 SQLite、PostgreSQL；`AutoMigrate` 后执行旧 Vault、revision、设备状态和 Vault 设置回填。模型位于 [models.go](../internal/models/models.go)。 |
| 网页后台 | [webui](../internal/webui/) 包含 Handler、Go 模板及静态资源；当前 CSP 限制脚本和样式来自自身站点。 |
| 插件 | [main.ts](../plugin/src/main.ts) 为入口，[api.ts](../plugin/src/api.ts) 封装请求，[sync-engine.ts](../plugin/src/sync-engine.ts) 编排同步；协作同步另有 `collaboration-*` 模块。测试位于 [plugin/tests](../plugin/tests/)。 |
| 配置 | [config.go](../internal/config/config.go) 负责 YAML 加载、环境变量覆盖和校验；维护配置时同时核对 [开发配置](../configs/config.dev.yaml) 与 [生产配置](../configs/config.prod.yaml)。 |

## 总体架构与运行链路

项目由单个 Go 后端、Obsidian TypeScript 插件和服务端扩展 SDK 组成。后端同时提供 JSON API、服务端渲染控制台、公开博客、静态资源和服务插件宿主；数据库保存关系与同步元数据，正文、历史快照、回收站、备份和插件包保存到 `storage.data_dir`。

后端启动链路为：

1. [main.go](../cmd/server/main.go) 先处理更新 helper 模式和 `--version`，再加载配置与更新状态。
2. [database.Init](../internal/database/database.go) 建立 SQLite/PostgreSQL 连接；SQLite 默认使用 `_txlock=immediate`，避免设备活动等先读后写事务升级争锁，保留 DSN 中显式指定的锁模式。`AutoMigrate` 注册模型并执行兼容回填。
3. 启动时执行一次 [reconcile](../internal/reconcile/reconcile.go)，随后加载已启用的服务插件。
4. [server.Router](../internal/server/server.go) 装配认证、管理、网页、Vault、设备、同步、博客、分享、更新和插件路由。
5. [cron.Scheduler](../internal/cron/scheduler.go) 注册墓碑、临时文件、孤立文件、历史清理、存储对账及插件任务。
6. 退出时依次停止 scheduler、关闭 HTTP 服务、关闭数据库；服务插件管理器也在进程退出时关闭。

主要 HTTP 边界：

| 路由范围 | 模块 | 职责 |
| --- | --- | --- |
| `/api/auth` | [auth](../internal/auth/) | 注册、登录、设备登录、令牌和密码管理 |
| `/api/admin` | [admin](../internal/admin/)、[update](../internal/update/) | 用户、主题、版本和管理员操作 |
| `/api/vaults` | [vaults](../internal/vaults/) | Vault CRUD、成员及角色 |
| `/api/devices` | [devices](../internal/devices/) | 设备状态、批准、吊销和 Vault 授权 |
| `/api/sync` | [syncapi/sync.go](../internal/syncapi/sync.go) | 兼容旧客户端的默认 Vault 同步 |
| `/api/vaults/:vault_id/sync` | [syncapi](../internal/syncapi/) | V2 manifest/changes/ack、上传下载、删除重命名、历史与策略 |
| `/api/vaults/:vault_id/recycle-bin` | [syncapi/history.go](../internal/syncapi/history.go) | 回收站列表、恢复和永久删除 |
| `/api/vaults/:vault_id/collaborations` | [syncapi](../internal/syncapi/) | 协作邀请、正文、SSE 和长轮询 |
| `/api/shares` | [shares](../internal/shares/) | 分享创建、查询、更新和删除 |
| `/`, `/b/:vault_id`, `/p/:share_id` | [blog](../internal/blog/) | 公共博客目录、Vault 博客和单篇分享 |
| `/plugins`, `/api/plugins` | [serverplugin](../internal/serverplugin/) | 公共/认证插件路由和客户端能力 |
| `/ui` 及控制台页面 | [webui](../internal/webui/) | Cookie 会话、CSRF、用户控制台和管理后台 |

## 后端模块边界

| 模块 | 主要职责与依赖方向 |
| --- | --- |
| [models](../internal/models/) | GORM 持久化模型；业务模块共享的最低层数据结构。 |
| [jwt](../internal/jwt/) | 最小 HS256 实现；claims 含用户、角色、token version 和可选设备 `did`。 |
| [auth](../internal/auth/) | 账户、Bearer/Basic 鉴权、网页/设备令牌、注册开关和限流。JWT secret 首次启动后保存在 `SystemSetting`。 |
| [deviceauth](../internal/deviceauth/) | 无 HTTP 依赖的设备登记、状态、Vault 授权和 cursor 更新逻辑，供 auth/devices/syncapi 共用。 |
| [vaultaccess](../internal/vaultaccess/) | 集中解析 owner/manager/participant/admin 权限；无权限时隐藏 Vault 是否存在。 |
| [vaults](../internal/vaults/) | Vault、成员和删除流程；永久删除前通过 [vaultbackup](../internal/vaultbackup/) 生成 ZIP 备份。 |
| [settingspolicy](../internal/settingspolicy/) | 合并系统设置、用户偏好和部署上限，计算同步模式、等待时间、容量与保留策略。 |
| [filestore](../internal/filestore/) | 标准正文键 `vaults/<vault>/files/<path>`，兼容旧用户目录。 |
| [history](../internal/history/) | `vaults/<vault>/history/<hash>.gz` 快照、元数据记录、文本 diff 和过期清理。 |
| [recycle](../internal/recycle/) | `vaults/<vault>/recycle/<file-id>` 软删除正文、恢复和保留期。 |
| [reconcile](../internal/reconcile/) | 对比数据库与磁盘，修复可识别路径，隔离孤立对象并维护 `StorageIssue`。 |
| [cron](../internal/cron/) | 墓碑压缩、孤立附件、临时文件、历史和存储对账任务。 |
| [markdown](../internal/markdown/) | Goldmark 扩展：Obsidian 双链、高亮和图片/附件解析。 |
| [shares](../internal/shares/) | 公开分享、短 ID、复制权限和双链反向引用。 |
| [blog](../internal/blog/) | 公开渲染、内置/自定义博客模板、资源解析和安全自定义片段。 |
| [consoletheme](../internal/consoletheme/) | 控制台主题 ZIP、文件管理与静态资源；与博客模板相互独立。 |
| [collaboration](../internal/collaboration/) | 邀请/接受/撤销业务和进程内事件 Broker；HTTP 适配位于 syncapi。 |
| [webui](../internal/webui/) | Go `embed` 模板和资源、Cookie 会话、double-submit CSRF、页面级服务组合。 |
| [version](../internal/version/) | 构建版本及严格 SemVer；Release 通过 `ldflags` 注入版本、commit 和构建时间。 |

## 数据模型与存储布局

- 账户与运行设置：`User`、`SystemSetting`、兼容旧配置的 `UserSetting`。
- Vault 与权限：`Vault`、`VaultMember`、`VaultSetting`、`VaultBackup`、`VaultSyncState`。
- 设备：`ClientDevice` 表达 pending/approved/revoked；`DeviceVaultAccess` 表达授权；`DeviceVault` 保存同步 cursor，两者不能混用。
- 同步文件：`File` 按 `(user_id, vault_id, path)` 唯一，保存 hash、size、mtime、revision、墓碑、存储键及最后一次设备/operation。
- 内容衍生数据：`FileHistory`、`Share`、`Collaboration`、`StorageIssue`。
- 服务插件：`ServerPlugin`、`ServerPluginAssociation`、`ServerPluginMigration`、`VaultPluginSetting`。
- 标准数据目录含 `vaults/<vault>/files`、`history`、`recycle`，以及 Vault 备份、服务插件包、更新状态等宿主持久数据；数据库和文件系统不是同一事务资源，写路径依赖临时文件、rename、备份恢复和 reconcile 做补偿一致性。

## Obsidian 插件组织

- [main.ts](../plugin/src/main.ts) 负责插件生命周期、命令、Ribbon、侧边栏、文件事件、登录会话和各管理弹窗装配；[settings-tab.ts](../plugin/src/settings-tab.ts) 渲染设置界面。
- [api.ts](../plugin/src/api.ts) 是后端 wire DTO 与 HTTP 传输边界；后端错误经 [localized-error.ts](../plugin/src/localized-error.ts) 和 [i18n.ts](../plugin/src/i18n.ts) 转成界面文本。
- [baseline.ts](../plugin/src/baseline.ts) 把普通同步基线、cursor、pending operation、冲突和协作状态持久化到 Vault 根目录 `.oss-sync-state.json` v3；[blacklist.ts](../plugin/src/blacklist.ts) 确保该文件不上传。
- [sync-engine.ts](../plugin/src/sync-engine.ts) 负责编排；[sync-run-coordinator.ts](../plugin/src/sync-run-coordinator.ts) 合并重叠触发；[task-pool.ts](../plugin/src/task-pool.ts) 限制传输并发并重试。
- 普通同步决策拆到 `ordinary-sync-*`：action planner 生成上传/下载/删除/重命名/冲突动作，file access 以期望 hash 防止异步覆盖本地新修改，conflict resolver 处理服务端 409 和二次冲突。
- [text-merge.ts](../plugin/src/text-merge.ts) 使用 diff3 做受大小和扩展名限制的三方文本合并；二进制或不可合并内容走保留双方；[conflict-modal.ts](../plugin/src/conflict-modal.ts) 提供人工处理。
- 协作文件位于本地 `协作oss/<owner>/<path>`；`collaboration-file-sync` 处理本地写入，`collaboration-remote-sync` 处理远端变化，`collaboration-cas-resolver` 处理 revision CAS，`collaboration-sync-coordinator` 串行化状态变更。
- [collaboration-transport.ts](../plugin/src/collaboration-transport.ts) 管理账户级 SSE 与长轮询；协作 baseline 与普通 Vault baseline 共用状态文件但采用独立数据结构。
- `history-*`、`recycle-manager-modal.ts`、`share-*`、`sidebar-view.ts` 是 Obsidian UI；`plugin-update.ts` 每次启动和运行期间每隔 30 分钟读取官方仓库根目录清单并提示通过社区插件市场更新，不下载或替换插件文件，`server-update.ts` 只负责轮询后端更新状态。
- [esbuild.config.mjs](../plugin/esbuild.config.mjs) 从 `src/main.ts` 构建 CommonJS `main.js`；`main.js` 是忽略的构建产物，源码修改只落在 `plugin/src/`。

## 服务端扩展系统

- [serverplugin](../internal/serverplugin/) 同时支持无 WASI、无 host import 的 WASM ABI 和管理员信任的持久 executable 进程；后者使用 stdin/stdout JSON Lines 并拥有服务账户的系统权限，不是沙箱。
- `Manager` 负责包校验、安装、启停、升级、依赖、迁移和运行时恢复；动态注册项包含 hook、route、middleware、admin page、task、migration、dependency、asset 和 Vault 级设置。
- executable 插件可通过双向 Host RPC 调用模型、Vault、文件、分享、博客、插件列表、设置和 hook 服务；公开 Go SDK 位于 [pkg/ossplugin](../pkg/ossplugin/)，示例位于 [examples/server-plugin-echo](../examples/server-plugin-echo/)。
- 博客模板和控制台主题只拥有展示资源；功能、数据、设置、路由与任务应放在服务插件中。完整包格式和信任边界见 [server-plugins.md](../docs/server-plugins.md)。

## 同步与存储边界

- [syncapi](../internal/syncapi/) 包含普通上传、V2 同步、协作上传和历史恢复；相关修改必须兼顾设备授权、Vault 隔离、revision、`operation_id` 重试处理、数据库事务和文件失败恢复。
- [deviceauth](../internal/deviceauth/deviceauth.go) 的 `CheckVaultAccess` 要求设备已批准、未吊销，并具备对应 Vault 授权。不要把登录成功当成已获得同步权限。
- [synclock](../internal/synclock/locks.go) 的 Vault/路径锁和 [storagequota](../internal/storagequota/quota.go) 的容量锁均为进程内锁，不能据此声称支持多实例共享目录的并发协调。
- `storage.max_total_size_mb` / `OSS_STORAGE_MAX_TOTAL_SIZE_MB` 是数据目录的应用层容量上限，`0` 表示不限。用量统计累加目录内普通文件的逻辑大小，不是文件系统硬配额。
- 普通上传、V2 上传、协作上传和历史恢复在提交前调用 `WithinLimit`；容量超限返回 HTTP 507，错误码为 `project_storage_quota_exceeded`。插件通过 [localized-error.ts](../plugin/src/localized-error.ts) 和 [i18n.ts](../plugin/src/i18n.ts) 提供对应提示。
- V2 上传、协作上传和历史恢复使用 `historySnapshotReserve` 预留旧正文快照空间；普通上传传入的预留值是 `0`，不能概括为所有覆盖写都预留历史快照。
- 上传临时文件先写入再检查容量；临时写入、数据库增长和进程外写入不受硬性空间预留控制。不能把提交前校验描述为任何时刻均不超限。
- `/readyz` 检查数据库连接以及未解决的 `missing` / `hash_mismatch` 存储问题，并返回版本；`/healthz` 只返回基础状态。

## 认证配置

- [auth_config.go](../internal/config/auth_config.go) 定义网页会话默认 24 小时、设备 JWT 默认 720 小时；分别由 `auth.web_session_ttl_hours`、`auth.device_jwt_ttl_hours` 控制，值为 `0` 时使用默认值。
- 对应环境变量为 `OSS_WEB_SESSION_TTL_HOURS`、`OSS_DEVICE_JWT_TTL_HOURS`。令牌签发见 [accounts.go](../internal/auth/accounts.go)，不要用旧的 `jwt_ttl_hours` 统一解释这两种有效期。

## 安装与服务管理

- [oss.sh](../oss.sh) 面向 Linux amd64/arm64 + systemd，从固定 Release 下载并校验平台运行包和独立脚本；运行包包含 bin 程序、生产 YAML 和 VERSION。首次安装默认 `/opt/oss-sync`，不安装 Docker 或 Go，不支持架构不会自动编译。
- 运行目录包括 `bin/oss-server`、`configs/config.prod.yaml`、`data/`、生成的 `service.env` 和 root 专用 `deployment.env`。新配置模板另存为 config.prod.yaml.dist，现有 YAML 保留；程序与配置由 root 管理，data 由 `oss-sync` 服务账户写入。
- [oss.sh](../oss.sh) 通过 systemd 提供更新、保留数据卸载、状态、启停重启、日志、容量和端口修改。更新保留生产 YAML，端口与容量由生成的环境文件覆盖。
- 更新前校验全部产物，停机替换后检查 `/readyz` 和版本；失败尝试恢复程序和配置，不回滚数据库迁移。旧 Docker 数据不自动接管；迁移步骤见 [deployment.md](../docs/deployment.md)。
- `OSS_UPDATE_MANAGER=systemd` 标记宿主机管理的部署，网页保留检查版本并提示运行 `sudo oss`，不启动 helper 自更新。
- [Dockerfile](../Dockerfile) 和 [docker-compose.yml](../docker-compose.yml) 保留为可选容器路径，不再由一键安装调用；容器数据仍使用 SQLite 命名卷。

## 服务端更新

- 网页链路为 [admin_update.go](../internal/webui/admin_update.go) → [Service](../internal/update/service.go) → [handoff](../internal/update/handoff.go) → [helper](../internal/update/helper.go)。不要用旧 `Updater` 中的兼容流程代替实际网页调用链。
- [app.js](../internal/webui/assets/app.js) 在检查到更高版本、具备更新能力且取得候选 `check_id` 后显示确认按钮；服务端也校验候选、版本和确认字段。
- 官方、代理、自定义源同时作用于 GitHub API 版本检查和 Release 文件下载，见 [github.go](../internal/update/github.go) 与 [download_source.go](../internal/update/download_source.go)。仍依赖 GitHub API 的 latest-release 元数据，没有非 API 回退；非官方源不携带 `OSS_GITHUB_TOKEN`。
- 候选需匹配平台资产名、有效大小和 SHA-256 digest。下载暂存后还校验二进制及版本，再备份并启动 helper。helper 等主进程退出后替换二进制、启动新进程、检查 `/readyz` 与目标版本；失败路径包含回滚处理。
- 未设置 systemd 管理标记时，能力检查要求受支持平台、有效发布版本、非软链的常规可执行文件及可写目录；目前没有直接拒绝容器运行。允许显示按钮、目录可写和容器可重启，均不足以证明容器网页更新端到端成功。
- 容器内更新涉及主进程退出、helper 存活和重启策略的配合，仍需真实验证。`/app/runtime` 不在默认数据卷内；容器重建使用所选镜像的二进制，不保留旧容器可写层中的更新。

## 构建、发布与验证入口

- [release.yml](../.github/workflows/release.yml) 接受 `X.Y.Z` 或 `X.Y.Z-rc.N`（可带 `v` 前缀）的标签，并检查提交属于远程 `main` 历史；不能概括为支持全部 SemVer 标签。
- 工作流配置构建 Linux amd64/arm64、macOS amd64/arm64、Windows amd64 后端，以及插件、独立 Linux oss.sh 和校验文件，平台运行包包含程序与配置；发布 GHCR 多架构镜像，正式版本另带 `latest` 标签。配置存在不代表远程发布成功。
- 服务端发布版本由构建参数注入；插件 Release 清单直接取自标签。正式标签的两份清单须一致，公开根清单在资源核验后通过合入发布提交同步。
- 后端修改的最小检查为 `gofmt`、`go test ./...`、`go vet ./...`；涉及同步并发时追加 `go test -race ./...`。插件使用 Node.js 20+，在 `plugin/` 执行 `npm ci`、`npm exec tsc -- --noEmit`、`npm test`、`npm run build`。CI 配置见 [ci.yml](../.github/workflows/ci.yml)。
- 文档修改至少核对路径、命令和配置名；任务记录只写实际执行的检查。源码中的测试和 CI 定义不等于已通过的验收结果。
