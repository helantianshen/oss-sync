# OSS Sync

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/webui/assets/oss-logo-light.png">
    <img src="internal/webui/assets/oss-logo.png" alt="OSS Sync" width="240" height="95">
  </picture>
</p>

> 自托管的 Obsidian 同步与分享服务：笔记、附件、协作与公开博客，一个二进制完成服务端部署。

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![Obsidian](https://img.shields.io/badge/Obsidian-1.4+-7C3AED)](https://obsidian.md)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[English](README.md) | 中文 · [文档与项目现状](docs/README.md) · [问题反馈](https://github.com/helantianshen/oss-sync/issues)

Obsidian 端配合 **OSS Sync and Share** 社区插件使用。服务端控制台、公开阅读页与博客资源随 Go 程序内嵌，无需单独部署前端。

## 核心功能

- **多仓库与设备授权**：账户可拥有多个 Vault；成员角色、文件路径与修订独立。设备批准与仓库授权分开管理。
- **多端同步**：文件新增、修改、删除、重命名及目录内文件移动；HTTP 短轮询或长轮询按仓库策略运行。空目录不作为独立对象同步。
- **离线队列与冲突处理**：本地待办持久化，重启后续传；基于 revision 的 CAS 检测，支持保留本地、远端、双方或合并。Markdown 支持三方合并，二进制保留副本。
- **附件与配置**：图片、PDF 等附件；可选同步 `.obsidian` 配置，默认关闭。工作区状态、缓存和插件 `data.json` 不参与同步。
- **网页文件管理**：Markdown、图片、PDF、HTML、SVG 预览，HTML/SVG 预览禁用脚本；文本编辑使用同步写入管线。提供历史版本、diff、恢复与回收站。
- **分享与公开博客**：单篇和文件夹链接、允许复制开关；博客按请求动态渲染。开启公开博客不会自动公开未分享的笔记。
- **Markdown 协作**：邀请、接受、撤销，HTTPS 下优先 SSE，明文 HTTP 或 SSE 不可用时走长轮询。
- **服务端扩展**：WASM 与受信任的可执行插件；可注册路由、Hook、中间件、后台页、任务、迁移、依赖和宿主 RPC，携带博客模板与控制台主题。
- **存储与部署**：默认 SQLite，可选 PostgreSQL；定时存储对账。支持 Linux 二进制/systemd、Docker 和手动运行。

## 快速部署

### Linux 二进制与 systemd（推荐）

支持 Linux amd64/arm64，要求 root、运行中的 systemd，以及 Python 3、curl、coreutils、`flock`、`useradd`/`getent`。脚本仅检查依赖，不安装工具链或 Docker。

```bash
curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/oss.sh -o oss.sh
# 执行前先审阅下载的脚本
sudo bash oss.sh
```

默认安装到 `/opt/oss-sync`，默认端口 `8080`，以专用 `oss-sync` 用户运行。安装时选择部署目录、端口、容量和下载源；安装后用 `sudo oss` 管理服务。

```bash
sudo oss status   # 查看 systemd 状态
sudo oss         # 更新、启停、重启、修改配置、日志和卸载菜单
```

完整依赖、非交互变量、更新回滚、备份与迁移见[部署指南](docs/deployment.md)。升级失败可恢复程序与配置，但不会回退数据库迁移，升级前必须备份。

### Docker

```bash
docker compose up -d --build
docker compose logs -f backend
```

默认端口 `8080`，数据保存在 `oss-data` 命名卷。通过重建或替换镜像更新；`docker compose down -v` 会删除数据。不要用二进制安装器覆盖已有 Docker 数据目录，迁移步骤见[部署指南](docs/deployment.md)。

### 手动二进制与源码

从 [Releases](https://github.com/helantianshen/oss-sync/releases) 下载运行包，解压后以包根目录为工作目录，设置 `OSS_ENV=prod` 再运行 `bin/oss-server`（Windows 为 `bin/oss-server.exe`）。

源码开发在仓库根目录执行：

```bash
go run ./cmd/server
```

默认加载 `configs/config.dev.yaml`，监听 `http://localhost:9090`，SQLite 和文件存放于 `data/`。

```bash
curl -fsS http://localhost:9090/healthz
curl -fsS http://localhost:9090/readyz
```

生产配置默认端口为 `8080`。环境变量覆盖和 PostgreSQL 配置见[部署指南](docs/deployment.md)；开发、客户端构建与发布见[开发指南](docs/development.md)。

## 首次使用

1. 打开实际服务地址，注册首个用户（自动成为管理员）；创建初始用户后可在后台关闭注册。
2. 在 Obsidian 社区插件市场安装 **OSS Sync and Share**；手动安装需将 `main.js`、`manifest.json`、`styles.css` 放到 `<vault>/.obsidian/plugins/oss-sync/`。
3. 插件首次打开设置时先保存设备名称，再填写包含 `http://` 或 `https://` 的服务地址及账户信息。
4. 在网页控制台批准设备，并为设备授权目标 Vault。批准设备并不等于授权所有仓库。
5. 在插件设置中选择已有 Vault，或创建新 Vault 后执行首次全量同步。
6. 后续本地修改进入持久化队列，远端变更经轮询下载；先确认同步范围再开启 `.obsidian` 配置同步。

Vault 根目录的 `.oss-sync-state.json`（v3）保存基线、队列与冲突，不上传。客户端只检查新版并提示到社区插件市场更新，不自行下载或替换插件文件。

## 插件、博客模板与控制台主题

可安装扩展统一为插件 ZIP，模板和控制台主题作为插件资源声明，不单独上传管理。内置资源不可删除；启用插件后，其资源显示在相应选择框。

- 博客内置 `default` 分享阅读模板，以及支持博客首页的 `papertrail`、`sakura`、`glass`、`starry-night`。
- 扩展入口：[服务端插件协议与 SDK](docs/server-plugins.md)、[中文完整插件指南](internal/webui/assets/plugin-guide.zh.md)。
- 示例：[Echo](examples/server-plugin-echo)、[Sections](examples/server-plugin-sections)、[Shirone](examples/server-plugin-shirone)。应为服务端操作系统与架构构建可执行入口，不能直接在 Linux 安装 Windows 示例产物。

## 安全与运维边界

- 公网部署必须使用 HTTPS，并限制后端端口只允许可信反向代理访问；配置转发协议头与 SSE 超时，见[部署指南](docs/deployment.md)。
- 密码使用 bcrypt；JWT 密钥随机生成并存入数据库，备份必须同时覆盖数据库和文件存储。
- 网页会话默认 24 小时，使用 HttpOnly、SameSite Cookie；HTTPS 请求才设置 Secure。网页 Cookie 写请求校验 CSRF，Bearer API 请求不采用相同机制。
- 插件设备令牌默认 720 小时（30 天），过期后需重新登录；文件同步需要设备批准及目标仓库授权。
- WASM 不提供 WASI/文件系统/网络/数据库/环境访问，但仍能接收宿主传入的 Hook 数据，不能据此视为可运行任意不可信插件。可执行插件拥有服务端账户权限；只安装审核过的扩展。
- 监控 `/readyz`：数据库不可用或存在未解决的文件缺失/哈希异常时返回非 200；这不等于完整的多端同步健康检查。

当前测试覆盖缺口、工具链差异和插件权限风险见[项目现状](docs/README.md#项目现状)。

## 许可证

MIT，见 [LICENSE](LICENSE)。
