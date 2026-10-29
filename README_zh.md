# OSS Sync

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="internal/webui/assets/oss-logo-light.png">
    <img src="internal/webui/assets/oss-logo.png" alt="OSS Sync" width="240" height="95">
  </picture>
</p>

  

> 自托管的 Obsidian 同步与分享服务：笔记、附件、协作与公开博客，一个二进制完成部署。

  

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)

[![Node](https://img.shields.io/badge/Node-20-339933?logo=node.js)](https://nodejs.org)

[![Obsidian](https://img.shields.io/badge/Obsidian-1.4+-7C3AED)](https://obsidian.md)

[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

  

[English](./README.md) | 中文

  

数据需配合 Obsidian 客户端插件使用：在 Obsidian 社区插件市场搜索 **OSS Sync and Share** 安装，或按本文「构建插件」一节从源码构建。

  

使用中遇到问题，请新建 [issue](https://github.com/helantianshen/oss-sync/issues) 。

  

---

  

## 核心功能

  

- **多仓库隔离**：一个账户可拥有多个 Vault，同步 revision、文件路径、成员与权限完全独立。

  - 支持仓库成员（manager / participant），并可按设备授权仓库。

- **多端同步**：支持新增、修改、删除、重命名与目录移动。

  - 变更实时分发，短轮询与长轮询可按仓库策略或用户偏好切换。

- **离线优先与队列持久化**：

  - 本地修改先写入 `.oss-sync-state.json` 持久化队列，再执行传输。

  - 关闭并重新打开 Obsidian 后自动续传，不会静默丢失。

- **冲突处理**：

  - 基于 revision 的 CAS 检测，提供保留本地、保留远端、保留双方与有序合并。

  - Markdown 支持三方合并；附件等二进制文件保留双方副本。

- **设备管理**：

  - 每个客户端以 `client_id` 标识，状态为待批准 / 已批准 / 已吊销。

  - 首次使用先设置设备名称；设备批准与仓库授权在服务端分离。

- **附件与配置同步**：

  - 支持图片、PDF 等非笔记文件；`.obsidian` 配置同步默认为关闭，可按需开启。

  - 网页控制台支持预览 Markdown、图片、PDF、HTML 和 SVG；HTML 与 SVG 在禁用脚本的隔离沙箱中渲染。

  - 无需服务端插件即可在控制台编辑文本文件；保存进入同步写入管线，内容不变不会产生新修订。

- **回收站与文件历史**：

  - 删除自动进入回收站，可查看剩余保留时间、恢复文件或按保留期自动清理。

  - 历史支持版本查看、逐行 diff 与回退到任意版本。

- **分享与公开博客**：

  - 支持单篇或文件夹公开链接，可设置允许复制。

  - 内置 `default` 分享阅读模板与支持公开博客首页的 `papertrail` 模板，按仓库选择。

- **Markdown 协作**：

  - 支持邀请、接受与撤销协作关系，SSE 实时推送，SSE失败自动转长轮询。

- **服务端插件扩展**：

  - 兼容 WASM，并支持管理员信任的可执行插件。

  - 可动态注册路由、Hook、中间件、后台页面、定时任务、数据库迁移、依赖与宿主 RPC。

  - 管理页面返回 HTML 片段时自动套用控制台外壳与当前主题；完整 HTML 文档由插件自行控制。

  - 公开 Go SDK 提供宿主文件读写接口；插件写入参与同步修订与文件历史。

- **数据与部署**：

  - 默认 SQLite，可选 PostgreSQL；定时执行存储对账。

  - 一键脚本完成二进制安装与 systemd 注册，也可使用 Docker。

  

---

  

## 架构

  

```

cmd/server        HTTP 入口

configs/          dev / prod YAML

internal/

  auth            注册、登录、JWT、设备鉴权

  syncapi          Vault revision、上传下载、重命名删除

  vaults           Vault 增删改查、成员、设置

  devices          设备状态、仓库授权、游标

  collaboration    邀请、接受、正文写入、事件

  history/recycle  快照、恢复、保留

  blog             博客主题与公开页

  serverplugin     WASM 与可执行插件运行时

  webui            网页控制台与管理后台

pkg/ossplugin      插件公开 Go SDK

plugin/src          Obsidian 插件

```

  

同步仅走 HTTP。短轮询 `wait=0` 或长轮询 `wait=30` 按 Vault 独立。协作推送按账号进行：HTTPS 下优先 SSE（`app://obsidian.md` 放行 CORS），局域网明文 HTTP 用长轮询。

  

---

  

## 快速部署

  

推荐使用一键脚本；也可使用 Docker 或手动二进制。

  

### 方式一：一键脚本（推荐）

  

自动下载对应架构的官方二进制，校验 SHA-256 与版本号，并注册 `oss-sync.service`：

  

```bash

curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/oss.sh | sudo bash

```

  

要求：运行中的 systemd、root 权限、Python 3、curl、coreutils、util-linux（`flock`）与 `useradd`/`getent`。脚本不安装 Docker、Go 或其他依赖；不支持的架构会明确退出并提示手动构建，下载或校验失败不会回退到编译源码。

  

脚本行为：

  

- 依次询问部署目录、端口（默认 `8080`）、存储上限 GiB（`0` 表示不限）与 Release 更新源（默认加速地址，可选 GitHub 官方或自定义 HTTPS 前缀）。

- 默认安装至 `/opt/oss-sync`，可用 `OSS_INSTALL_DIR` 指定。

- 在 `/usr/local/bin` 创建全局命令 `oss` 与 `oss-sync`。

- 服务以专用系统账户 `oss-sync` 运行，不授予多余 Linux capability。

  

安装完成后运行 `sudo oss` 打开管理菜单：

  

```text

┌──────────────────────────┐

│ OSS Sync 0.1.22          │

│ 状态：运行中             │

│ 地址：http://0.0.0.0:8080 │

│ 存储：000 KB / 不限      │

└──────────────────────────┘

  

1 更新        先选更新源：默认加速 / 官方 / 自定义 / 0 返回

2 停止        服务运行时为“停止”，停止时为“启动”

3 重启

4 修改        1 修改容量、2 修改端口、0 返回

5 日志

6 卸载        1 卸载全部（含数据）、2 保留数据、0 返回

0 退出

```

  

也可直接传编号：

  

```bash

sudo oss 1        # 更新

sudo oss 2        # 启动或停止

sudo oss 3        # 重启

sudo oss 4        # 修改容量或端口

sudo oss 5        # 持续查看日志

sudo oss status   # 详细 systemd 状态

```

  

更新会在停止服务前完成全部资产校验，替换二进制并核对 `/readyz` 与预期版本；失败时尝试恢复此前程序与配置，但不回退数据库迁移，升级前请备份数据。

  

非交互安装可设置 `OSS_PORT`、`OSS_STORAGE_LIMIT_GB`、`OSS_INSTALL_DIR`、`OSS_RELEASE_PROXY=official`（或 HTTPS 前缀）；`OSS_VERSION` 指定精确 Release 标签。详见[二进制与 systemd 部署](docs/deployment.md)。

  

### 方式二：Docker

  

Docker 仍用于源码开发与既有部署，不再由一键安装器使用：

  

```bash

docker compose up -d --build

docker compose logs -f backend

```

  

Compose 默认暴露 `8080`，数据保存在 `oss-data` 命名卷。该部署通过重建或替换镜像更新。不要在已有 Docker 数据目录上直接运行二进制安装器，请按[迁移指南](docs/deployment.md)操作。`docker compose down -v` 会删除数据卷。

  

### 方式三：手动二进制

  

从 [Releases](https://github.com/helantianshen/oss-sync/releases) 下载对应系统的运行包，解压后以该目录为工作目录运行：

  

```bash

./bin/oss-server          # Linux / macOS

./bin/oss-server.exe      # Windows

```

  

需设置 `OSS_ENV=prod` 以加载包内生产配置；数据与 SQLite 相对当前目录解析。

  

### 源码运行后端

  

适用于开发调试：

  

```bash

go run ./cmd/server

```

  

默认监听 `http://localhost:8080`，数据在 `data/`。健康检查：

  

```bash

curl http://localhost:8080/healthz

curl http://localhost:8080/readyz

```

  

通过 `OSS_ENV=dev|prod` 选择配置文件，可用环境变量覆盖 `OSS_SERVER_HOST`、`OSS_SERVER_PORT`、`OSS_DB_DRIVER`、`OSS_DB_DSN`、`OSS_STORAGE_DIR` 等。使用 PostgreSQL：

  

```bash

export OSS_DB_DRIVER=postgres

export OSS_DB_DSN='postgres://user:pass@127.0.0.1:5432/oss?sslmode=disable'

go run ./cmd/server

```

  

---

  

## 使用指南

  

1. **注册管理员**：浏览器打开 `http://{服务器IP}:8080`，首个注册用户为管理员。如需关闭注册，在管理后台系统设置中关闭。

2. **安装插件**：Obsidian 社区插件市场安装 **OSS Sync and Share**，或从源码构建后复制到 `<vault>/.obsidian/plugins/oss-sync/`。

3. **设置设备名称**：首次打开插件设置，先填写设备名称并保存，之后将显示登录表单。

4. **登录**：填写包含 `http://` 或 `https://` 的服务端地址、用户名与密码。

5. **批准设备**：在网页控制台设备管理中批准该设备。

6. **绑定仓库**：在插件设置的仓库区域选择已有仓库，或创建新仓库并立即全量同步。设置页保持打开时每 3 秒刷新一次已授权仓库列表。

7. **开始同步**：本地修改自动进入持久化队列并上传；其他设备的变更轮询到达后自动下载。

  

插件在 Vault 根目录维护 `.oss-sync-state.json`（v3），记录基线、待传输队列与冲突，该文件不会上传。

  

---

  

## 插件、博客模板与控制台主题

插件是唯一可安装的扩展包。插件负责功能、设置、路由、Hook、数据、后台页面、任务与外部集成，也可以在 `manifest.json` 中声明博客模板和控制台主题。

内置博客模板 `default`、`papertrail` 与内置控制台主题 `default` 始终显示在对应选择框中，不能删除。启用插件声明的资源后，展示名称会追加到对应选择框。模板和控制台主题不再作为独立列表显示，也不再提供独立上传、脚手架、删除或管理页面。

### 创建插件

1. 复制 [`examples/server-plugin-echo`](examples/server-plugin-echo)，修改插件 ID 与处理函数。
2. 在与 `manifest.json` 同级目录构建可执行文件。
3. 按需添加 `blog_themes`、`console_themes`，目录分别必须包含 `template.html` 或 `theme.css`。
4. 打包为 ZIP，在 **管理后台 → 插件管理** 上传。

```powershell
cd examples/server-plugin-echo
go build -o plugin.exe .
Compress-Archive manifest.json,plugin.exe my-plugin.zip
```

包含博客或控制台资源时，ZIP 还必须加入声明的资源目录，例如 `Compress-Archive manifest.json,plugin.exe,blog,console my-plugin.zip`（仅列出实际存在的目录）。博客字段、首页分支、附件路径与排错见[博客模板契约](internal/webui/assets/plugin-guide.zh.md#博客模板契约)。

资源声明示例：

```json
{
  "blog_themes": [{"id":"clean","name":"Clean reading","path":"blog/clean"}],
  "console_themes": [{"id":"clean","name":"Clean console","path":"console/clean"}]
}
```

插件使用公开 Go SDK `github.com/helantianshen/oss-sync/pkg/ossplugin`，不需要手写 JSON Lines 协议。服务端直接运行预编译的 WASM 或可执行插件；插件页的在线编辑只允许校验通过的文本资源，二进制入口只读。AdminPage 返回片段时自动套用控制台外壳与当前主题，返回完整 HTML 文档则由插件自行控制。包结构、callback、鉴权、宿主服务、打包测试和 AI 辅助开发见[服务端插件指南](docs/server-plugins.md)与网页内置「插件指南」。

  

---

  

## 配置说明

  

| 环境变量 | 说明 |

|---|---|

| `OSS_ENV` | `dev` 或 `prod` |

| `OSS_SERVER_HOST` / `PORT` | 监听地址与端口 |

| `OSS_DB_DRIVER` / `DSN` | sqlite 或 postgres |

| `OSS_STORAGE_DIR` | 文件存储根目录 |

| `OSS_ALLOW_ANONYMOUS_REGISTRATION` | 初始注册开关 |

| `OSS_WEB_SESSION_TTL_HOURS` | 网页会话有效小时数，默认 `24` |

| `OSS_DEVICE_JWT_TTL_HOURS` | 插件设备令牌有效小时数，默认 `720` |

| `OSS_DEVICE_STALE_DAYS` | 设备过期阈值 |

| `OSS_RECONCILE_INTERVAL_HOURS` | 存储对账周期 |

| `OSS_UPDATE_DOWNLOAD_SOURCE` | 更新源：`official`、`proxy` 或 `custom` |

| `OSS_UPDATE_DOWNLOAD_PROXY` | `custom` 源使用的 HTTPS 前缀 |

  

按仓库设置：`sync_mode`（`user_choice` / `short_poll` / `long_poll`）、回收站保留天数、存储配额与上传大小限制。

  

更新使用的 `download_source` 与 `download_proxy` 写在 `configs/config.dev.yaml` 或 `configs/config.prod.yaml` 的 update 段；管理后台的更新面板可为本次检查临时覆盖。所选地址同时用于获取版本信息与下载文件，服务器无法直连 GitHub 时仍可更新。

  

---

  

## 开发与构建

  

```bash

# 后端

go build ./...


go vet ./...

  

# 插件

cd plugin

npm ci

npm exec tsc -- --noEmit

npm run lint
npm test

npm run build

```

  

约定：Go 使用 `gofumpt` + `golangci-lint`，TypeScript 严格模式，界面不使用 emoji，样式统一走 `console.css` 变量，不写内联样式。

  

---

  

## 部署注意事项

  

- 在 Go 服务前放置支持 HTTPS 的反向代理。

- 备份 `data/`（SQLite 文件或 Postgres 导出）以及存于数据库的 JWT 密钥。

- 初始用户创建后，在管理后台系统设置中关闭开放注册。

- 监控 `/readyz`，非 200 或对账持续失败时告警。

- Nginx 反代示例见 [scripts/https-nginx-example.conf](scripts/https-nginx-example.conf)。

  

---

  

## 安全说明

  

- 密码 bcrypt 存储，不写入日志。

- JWT 为 HS256，密钥按部署随机生成并落库。

- 网页会话使用 24 小时有效的 HttpOnly Secure SameSite Cookie 并校验 CSRF；插件使用 30 天有效的设备绑定 Bearer JWT，过期后从本地移除并要求重新登录。

- 所有变更接口校验 CSRF；所有同步与协作接口校验已批准设备与仓库授权。

- 服务端插件支持 WASM 包与管理员信任的可执行包。WASM 模块不提供 WASI、文件、网络、数据库或环境变量访问；可执行包可以读写服务器文件、访问数据库、使用网络、读取环境变量并执行系统命令，权限与服务端账号一致。插件仅由管理员上传，请只安装经过审核的代码。

- 插件可声明由服务端渲染的设置字段，不允许注入 HTML 或 JavaScript。

  

---

  

  

## 许可证

  

MIT，见 [LICENSE](LICENSE)。