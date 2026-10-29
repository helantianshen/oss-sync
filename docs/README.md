# 文档导航与项目现状

[中文项目入口](../README_zh.md) · [English](../README.md)

## 按用途阅读

| 需求 | 文档 | 职责 |
| --- | --- | --- |
| 了解功能、部署入口和首次同步 | [中文 README](../README_zh.md) / [English README](../README.md) | 两种语言保持相同范围，不重复完整运维与开发手册 |
| 安装、配置、反代、备份与升级 | [部署指南](deployment.md) | systemd、Docker、手动运行、迁移和故障边界 |
| 构建、验证、安装客户端与发布 | [开发指南](development.md) | 工具链、命令、测试覆盖和版本元数据 |
| 实现服务端扩展 | [服务端插件协议与 SDK](server-plugins.md) | ZIP、manifest、运行时协议、宿主服务与模板契约 |
| 完整插件开发教程 | [中文](../internal/webui/assets/plugin-guide.zh.md) / [English](../internal/webui/assets/plugin-guide.md) | 随服务端内嵌，在控制台「插件指南」中展示；包含示例和排错 |
| 参考可执行插件 | [Echo](../examples/server-plugin-echo/README.md)、[Shirone](../examples/server-plugin-shirone/README.md)、[Sections 源码](../examples/server-plugin-sections) | 可运行示例；Shirone 另含模板资源说明 |

## 项目现状

核实日期：2026-10-29。以下为当前工作区的源码与配置事实，不代表远端 Release、生产部署或真实 Obsidian 双端已验收。

### 组成与能力边界

| 部分 | 当前实现 | 主要入口 |
| --- | --- | --- |
| 服务端 | Go 单进程，Gin HTTP，GORM；SQLite 默认、PostgreSQL 可选；启动迁移、对账、加载插件并启动后台任务 | [`cmd/server/main.go`](../cmd/server/main.go)、[`internal/transport/http/router/router.go`](../internal/transport/http/router/router.go) |
| 网页控制台 | Go 模板与静态资源内嵌；账户、设备、仓库、文件、历史、回收站、分享、备份和管理功能 | [`internal/webui/`](../internal/webui) |
| Obsidian 客户端 | TypeScript，事件队列与本地发现，修订冲突处理、普通同步与协作分开协调 | [`plugin/src/main.ts`](../plugin/src/main.ts)、[`plugin/src/sync-engine.ts`](../plugin/src/sync-engine.ts) |
| 分享与博客 | 按请求动态渲染，不导出静态站点；公开博客与单篇分享分别启用，未分享文章不会因开启博客自动公开 | [`internal/blog/`](../internal/blog)、[`internal/shares/`](../internal/shares) |
| 服务端扩展 | WASM 与受信任可执行进程；公开 Go SDK；插件可携带博客模板、控制台主题 | [`internal/serverplugin/`](../internal/serverplugin)、[`pkg/ossplugin/`](../pkg/ossplugin) |
| 部署与发布 | Linux amd64/arm64 安装器、systemd 托管升级、Docker；工作流打包跨平台二进制与客户端资产 | [`oss.sh`](../oss.sh)、[工作流](../.github/workflows)、[`scripts/package-release.py`](../scripts/package-release.py) |

同步基于 HTTP 短/长轮询，不是 WebSocket 同步；协作另有 SSE 通道。普通同步以文件为对象，目录移动转换为文件路径变更，不承诺独立空目录同步。`.obsidian` 同步默认关闭，工作区、缓存和插件 `data.json` 属于设备本地状态，始终排除。

### 回归、版本与运维边界

| 事项 | 当前证据与影响 | 建议 |
| --- | --- | --- |
| 关键行为回归 | 已建立同步、更新准入、版本比较三个 Go 测试包，共 11 个顶层测试函数及子场景；客户端现有 15 项测试 | 持续验证 CAS、正文/历史一致性、权限拒绝、队列恢复和冲突保护；详细范围见[开发指南](development.md#测试现状与覆盖边界)，不代表所有功能已全面覆盖 |
| 版本元数据一致性 | 根和客户端兼容表已对齐，含 `0.2.11`、`0.3.1`；客户端清单、npm 包和锁文件均为 `0.3.1`；CI/Release 执行统一版本检查 | 发布准备运行 `python3 scripts/check-versions.py`；允许根清单保留上一正式版公告，但不能破坏其兼容映射 |
| Node 版本要求不精确 | 锁定的 ESLint 10 要求 `^20.19.0 \|\| ^22.13.0 \|\| >=24`；CI 与 Release 仅声明 Node `20` | 本地推荐 Node 24；若调整 CI，显式表达工具链下限，不能假设任意 Node 20 都兼容 |
| 源码构建标识 | 未注入版本的服务端显示 `dev`，禁用发布版自更新；正式构建由 `ldflags` 注入标签版本，CLI、健康接口与管理接口使用同一值 | 不把客户端清单版本硬编码到 Go 源码；正式资产遵循工作流注入与核验，避免源码构建冒充旧正式版 |
| 博客 Hook 的凭据边界 | [`collectPluginData`](../internal/blog/plugin_data.go) 收集完整 Headers 和 Cookies，交给 [`ApplyHookData`](../internal/serverplugin/hooks.go) 的 `blog.data` 插件；包含访问者随请求携带的会话/CSRF 数据，未按敏感字段裁剪 | 只启用受信任插件；WASM 无系统访问能力不等于不接触凭据。后续如需支持不可信插件，必须单独设计 Hook 数据权限；本次未修改权限模型，也未进行利用验证 |
| 自动回滚不含数据库 | 安装器升级失败恢复程序与配置，不撤销已运行的数据库迁移 | 升级前保留数据库与文件存储的一致备份；详见[部署指南](deployment.md) |

### 配置与运行事实

- 配置由当前工作目录下的 `configs/config.<OSS_ENV>.yaml` 加载，再叠加受支持的环境变量；未设置 `OSS_ENV` 时使用 `dev`。
- 仓库开发配置端口为 `9090`，生产配置、安装器与容器默认端口为 `8080`；不要把部署默认值套到源码启动命令。
- `/healthz` 返回进程存活、环境与构建版本；`/readyz` 额外检查数据库和未解决的存储缺失/哈希异常，不覆盖全部插件、任务或客户端同步健康状态。
- 客户端只检查官方根 `manifest.json`，启动布局就绪时及之后每 30 分钟提醒市场更新。客户端更新、Go 服务端升级和服务端扩展更新是不同流程，不能混用配置。
- 本地版本号只表明当前元数据；未经远端查询与资产核验，不能据此认定某版本已发布或生产已升级。

### 本次实际验证

环境：Go `1.27.3-X:nodwarf5`、Node `24.20.0`、npm `11.19.0`。本次补充关键行为回归、对齐兼容映射并明确源码构建标识；升级回退与数据库迁移行为保持不变。

| 检查 | 结果 |
| --- | --- |
| `go test -race ./...`、`go build ./...`、`go vet ./...` | 通过；`syncapi`、`update`、`version` 三个包执行实际回归 |
| 客户端 `npm test`、TypeScript、ESLint、生产构建 | 15 项测试通过；ESLint 无错误，另有 1 条 `prefer-setting-definitions` 警告 |
| 版本元数据检查 | 当前 `0.3.1` 通过；隔离副本验证兼容表漂移、锁文件版本漂移、最低宿主不匹配和错误标签均被拒绝，上一正式版公告与 RC 准备流程仍可通过 |
| 开发与正式构建实跑 | 分别编译未注入版本与注入 `0.3.1` 的二进制；`--version`、`/healthz`、`/readyz`、`/api/admin/version` 分别一致返回 `dev`、`0.3.1` |
| 双设备 HTTP smoke | 真实临时服务、SQLite 与文件存储：设备批准和仓库授权边界、上传下载、旧 revision 冲突不覆盖、相同正文空操作、重命名墓碑分页与新路径正文均通过；两个设备会话由 HTTP 驱动，不是真实 Obsidian 双端 |

临时进程、数据库、存储和 smoke 二进制已清理，没有访问现有 `data/`。未执行 systemd 安装/升级、Docker、PostgreSQL、Nginx 配置加载、远端 Release 资产核验或真实 Obsidian 多端验收。

## 文档分层与历史材料

- 根 README 负责产品入口与首次使用；专题细节放在 `docs/`，示例细节留在对应 `examples/`。
- `docs/server-plugins.md` 是精简协议参考；`internal/webui/assets/plugin-guide*.md` 是面向控制台用户的完整双语教程。修改共同契约时需要同步两处，不直接删除运行时内嵌文档。
- [旧客户端更新加速源方案](archive/plugin-update-source-spec.md)只作历史记录，已被市场更新提醒取代，不应用来实施当前客户端。
- `.agent/` 属于本地协作材料，不是对外使用手册。被忽略的 `.agent/tasks/` 用于临时任务交接；已完成或被取代的任务 Markdown 已清理，不保留为当前需求。目录中的调试脚本、日志、临时数据库等非文档产物不在本次清理范围。
- 文档维护以当前代码、配置与实际验证为准。验证记录必须区分构建、隔离 smoke、真实 Obsidian 与生产环境，不能把历史会话结果当成本次验收。
