# HANDOFF — OSS Sync 当前工作状态

## 当前目标

维护已公开发布的 `0.1.17`，准备发布容器内网页自更新改动为 `0.1.18`。

## 当前状态

- `0.1.17` 已公开发布，Release 工作流 `33617267604` 成功。
- Release 包含服务端多平台二进制、Obsidian 插件、`install.sh`、`manage.sh`、amd64/arm64 离线 Docker 镜像归档和覆盖全部资产的 `checksums.txt`。
- GHCR 已发布 amd64/arm64 多架构镜像及 `latest` 标签。
- 容器内网页自更新改动已完成，尚未提交或发布；`0.1.17` 仍是当前公开版本。
- `.agent/参考脚本/` 是用户提供的 1Panel 参考资料，不属于项目产物；除非用户明确要求，不修改、不提交。

## 当前能力

### 一键部署与管理

- 官方入口保持为仓库中的 `install.sh`，安装器按架构下载 Release 镜像归档并执行 SHA-256 校验、`docker load`、容器启动和健康检查。
- 安装时支持：
  - `gh-proxy.com` 文件加速（默认）；
  - GitHub 官方下载；
  - 自定义 HTTPS 文件加速前缀；
  - 自定义端口，留空时在 `10000-25565` 中随机选择可用端口；
  - 自定义绝对部署路径；
  - 项目总容量上限（GiB，`0` 表示不限）。
- 新安装使用 `<部署路径>/data` 绑定挂载；升级会复用已有端口、数据路径、容量设置及旧版命名卷，失败时恢复旧容器。
- 安装后提供全局命令 `oss` / `oss-sync`，管理菜单支持更新、保留数据卸载、状态查看、启动、停止、重启、修改容量和修改端口。
- Docker 部署的服务端二进制位于镜像内可写运行目录，网页更新通过校验后原子替换并退出进程，由 Docker 重启策略启动新版本；`oss` / `oss-sync` 仍可用于宿主机重建镜像更新。

### 项目总容量限制

- `storage.max_total_size_mb` / `OSS_STORAGE_MAX_TOTAL_SIZE_MB` 限制整个数据目录的应用层总容量。
- 普通同步、V2 同步、协作上传和历史恢复均在提交前检查容量；覆盖写会预留历史快照空间。
- 超限返回 HTTP 507 和稳定错误码 `project_storage_quota_exceeded`，插件提供中英文提示。
- 管理后台显示项目数据目录实际用量、容量上限、监听端口和运行状态。

### 服务端在线更新

- 检查更新在页面内完成；仅检查到更高版本后显示“确认更新”。
- 原生二进制部署可选择 `gh-proxy.com`、GitHub 官方或自定义 HTTPS 前缀下载 Release 文件。
- 下载源只影响 Release 文件；版本检查仍访问 GitHub API。下载后继续校验候选资产大小与 SHA-256。
- Docker 部署同样显示更新源和网页确认更新按钮；更新完成后由 Docker 重启策略拉起新二进制。镜像级变更仍通过 `oss` / `oss-sync` 或重新执行安装脚本应用。
- 更新页逻辑位于外部静态资源，符合当前 `script-src 'self'` / `style-src 'self'` CSP。

### 发布流程

- 推送符合 SemVer（如 `0.1.17` / `v0.1.17`）格式的标签会触发 `.github/workflows/release.yml`。
- 工作流构建 Linux amd64/arm64、macOS amd64/arm64、Windows amd64 服务端包和插件资产。
- 同时发布 GHCR 多架构镜像、两个可供 `docker load` 的镜像归档、安装与管理脚本及 `checksums.txt`。

## 重要决策

- GitHub Release 文件代理不能代理 `docker pull`；因此发布离线镜像归档，安装器下载后使用 `docker load`。
- 选择代理时，`checksums.txt` 与目标资产走同一来源；SHA-256 可检测传输损坏，但不能防止代理同时替换二者。如需更强供应链保证，应增加独立签名。
- 当前一键部署使用 SQLite，没有 PostgreSQL 等额外 Docker Hub 依赖，因此不修改宿主机全局 Docker 镜像配置。
- 容量限制采用跨发行版的应用层实现，不创建 loop 文件系统，也不依赖 XFS project quota。
- 容量锁为单进程锁；当前产品按单实例部署。未来若支持多实例共享数据目录，需改为跨进程协调。
- 默认公开监听存在首个注册者成为管理员的抢注窗口，这是用户已明确接受的部署取舍。

## 重要文件

- 部署与管理：`install.sh`、`manage.sh`、`docker-compose.yml`、`Dockerfile`
- 发布：`.github/workflows/ci.yml`、`.github/workflows/release.yml`
- 容量：`internal/storagequota/`、`internal/syncapi/`、`internal/config/config.go`
- 在线更新：`internal/update/`、`internal/webui/admin_update.go`、`internal/webui/templates/admin_system.html`、`internal/webui/assets/app.js`
- 插件错误提示：`plugin/src/i18n.ts`、`plugin/src/localized-error.ts`
- 产品说明：`README.md`、`README_zh.md`

## 已完成验证

- 后端：`go test ./... -count=1`、`go vet ./...`、关键并发路径 race 测试通过。
- 插件：Node.js 26 临时环境下 `npm ci`、TypeScript 检查、253 项测试和构建通过。
- 安装器：Shell 语法、Compose 配置、CI 本地代理隔离测试通过。
- Docker：真实安装、升级、数据保留、容量与端口修改、启停重启、状态查看及保留数据卸载通过；新容器镜像已实际启动，`/readyz` 通过，`/app/runtime/oss-server` 对容器用户可写，选择自定义更新源时页面显示输入框，进程退出后 Docker 自动重启通过。
- 更新页：Go/WebUI 定向测试、JavaScript 语法、CSP、三种下载源解析和隔离 Docker 页面验证通过。
- `0.1.17`：公开 Release，非草稿、非预发布，15 个资产均已上传。

## 已知问题 / 风险

- 应用层容量不是文件系统硬配额；进程外写入、SQLite/WAL 增长及少量元数据开销可能造成瞬时超出。
- 内置第三方文件代理的可用性可能变化，用户可切换官方源或自定义 HTTPS 前缀。
- GitHub 未认证 REST API 按出口 IP 每小时仅 60 次；共享代理出口可能返回 `403 rate limit exceeded`。
- `0.1.17` 发布前的网页更新真实下载测试因 Docker 与宿主代理共用出口且匿名额度耗尽，在版本检查阶段被 403 阻断；新镜像的容器页面、可写运行目录和重启行为已验证，真实 Release 下载替换仍未验证。
- 旧版 `oss-data` 命名卷不会主动迁移到新绑定目录；升级时优先继续使用旧卷以避免数据丢失。

## 剩余工作

- 容器内网页自更新的真实 Release 下载、二进制替换和重启后版本确认验证。

## 推荐下一步

- 在 GitHub API 额度可用的环境中补一次 `0.1.18` 网页更新真实下载端到端验证，包含 Docker 容器内更新后自动重启和版本确认。
- 若匿名 API 限流在真实部署中频繁出现，再实现 latest-release 非 API 回退；当前不预先增加复杂度。
