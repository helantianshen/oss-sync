# 开发与发布指南

本文记录当前仓库可用的工具链、开发命令和发布边界；部署配置见[部署指南](deployment.md)，服务端扩展见[插件开发指南](server-plugins.md)，完整导航见[文档首页](README.md)。历史方案不代表现版本行为。

## 工具链

| 用途 | 实际要求与依据 |
| --- | --- |
| 服务端 | Go 1.25.0 或兼容的更新版本，以根目录 `go.mod` 为准；CI 使用 `go-version-file: go.mod`。 |
| Obsidian 客户端开发 | Node.js 与 npm，推荐 Node 24 LTS。锁定的 ESLint 10.13.0 要求 `^20.19.0 \|\| ^22.13.0 \|\| >=24`，并非任意 Node 20+；`plugin/package.json` 当前未声明 `engines`。 |
| 客户端运行宿主 | Obsidian 1.4.0+；`manifest.json` 的 `isDesktopOnly: false` 表示支持移动端。开发机的 Node 要求不等于用户运行 Obsidian 时需要另装 Node。 |
| 打包工具 | `scripts/package-release.py` 和 Shirone 示例使用 Python 3 标准库；完整 Release 工作流使用 Ubuntu runner 提供的工具。容器相关操作另需 Docker/Compose。 |

当前 CI 和 Release 工作流仍配置 `node-version: 20`，没有写明补丁下限。Node 20 系列中须至少为 20.19.0；Node 21、23 也不在 ESLint 的支持范围内。旧的「Node 20」徽章不足以表达这些限制。依赖版本以 `plugin/package-lock.json` 为准，使用 `npm ci`，不要凭 `@types/node` 版本推断工具链下限。

## 本地开发

以下服务端命令均从项目根目录执行：

```sh
# 默认加载 configs/config.dev.yaml，使用 SQLite data/oss.db
# 开发配置监听 http://localhost:9090
go run ./cmd/server

# 版本检查、编译与回归（与后端 CI 一致）
python3 scripts/check-versions.py
go build ./...
go vet ./...
go test -race ./...

# 生成可独立启动的本地程序；仍须从含 configs/ 的目录启动
go build -o bin/oss-server ./cmd/server
./bin/oss-server --version
```

`internal/config.Load` 只接受 `OSS_ENV=dev`（默认）或 `OSS_ENV=prod`，分别读取相对当前工作目录的 `configs/config.<env>.yaml`，再应用环境变量覆盖。生产配置默认端口为 8080，不能将它当作开发配置的默认端口。正式部署和覆盖项见[部署指南](deployment.md)。

客户端命令从 `plugin/` 执行：

```sh
cd plugin
npm ci
npm run dev                 # esbuild watch，输出 plugin/main.js
```

交付前按 CI 顺序检查：

```sh
# 仍在 plugin/ 内
npm run lint
npm exec tsc -- --noEmit
npm test
npm run build               # 生产压缩构建，不带 source map
```

`build` 本身仅执行 esbuild 打包，不替代 TypeScript 类型检查、lint 或测试。源码位于 `plugin/src/`，入口为 `src/main.ts`；开发与生产构建都写入 `plugin/main.js`，没有额外的 `dist/` 安装包。

### 在测试 Vault 安装客户端

插件 ID 是 **`oss-sync`**，显示名称是 **OSS Sync and Share**；不要把 npm 包名 `oss-sync-plugin` 当作安装目录名。手动安装只需把以下三个文件放到同一目录：

```text
<vault>/.obsidian/plugins/oss-sync/
├── main.js
├── manifest.json
└── styles.css
```

文件来自 `plugin/` 的构建结果和随附资源，或同一 GitHub Release 的三个独立资产。使用自定义 Obsidian 配置目录时，将 `.obsidian` 换成该 Vault 的实际配置目录。然后在 Obsidian 社区插件设置中启用或重新加载插件。无需复制 `src/`、`node_modules/`、`package.json` 或 `versions.json`；也不要覆盖已有 `data.json`，它保存当前实例的插件设置。使用独立测试 Vault 验证同步、冲突、删除和更新，避免覆盖日常数据。

## 测试现状与覆盖边界

持久化回归测试按行为划分，不以历史任务中的测试数量作为当前覆盖依据：

| 范围 | 测试入口 | 覆盖的关键行为 |
| --- | --- | --- |
| Go 同步接口 | [`internal/syncapi/`](../internal/syncapi) 中的 `v2_test.go`、`access_test.go` | 真实 SQLite 与临时正文存储；上传下载、旧 revision CAS 冲突、相同正文空操作、覆盖前历史快照、重命名墓碑与分页游标、删除及回收站恢复；待批准、已吊销、无目标授权、跨用户和伪造设备身份的访问拒绝及无副作用 |
| Go 版本与更新准入 | [`internal/version/semver_test.go`](../internal/version/semver_test.go)、[`internal/update/capability_test.go`](../internal/update/capability_test.go) | 版本解析和优先级、正式版/RC/构建元数据比较、开发构建禁止自更新、不安全程序入口拒绝和更新候选选择 |
| 客户端持久化 | [`baseline.test.mjs`](../plugin/tests/baseline.test.mjs) | 真实临时磁盘上的待办、基线和冲突重启恢复，落盘失败后重试、移动折叠、扫描删除依赖与账户隔离 |
| 客户端同步规划 | [`ordinary-sync-action-planner.test.mjs`](../plugin/tests/ordinary-sync-action-planner.test.mjs) | 增量缺失不误删、本地修改与远端删除冲突、保护移动两端和已有冲突 |
| 客户端合并与兼容 | [`text-merge.test.mjs`](../plugin/tests/text-merge.test.mjs)、[`release-compatibility.test.mjs`](../plugin/tests/release-compatibility.test.mjs) | 独立修改合并、重叠冲突保留双方正文、有序合并、UTF-8/二进制/字节上限；设备状态排除、移动端请求错误传播和确认操作 |

当前为 3 个 Go 测试包、11 个顶层测试函数（另有子场景），以及 15 项客户端测试。后端 CI 和 Release 均执行 `go test -race ./...`，客户端沿用 `npm test`。Go fixture 使用标准 `testing`、Gin/httptest、独立临时 SQLite 和文件；客户端通过 Node 内建测试运行器与共享 esbuild loader 执行实际 TypeScript 模块，仅模拟必要的 Obsidian 宿主。

这些回归覆盖关键同步和状态边界，但不是全功能或真实 Obsidian 集成验收；博客、插件运行时、PostgreSQL、真实多设备协作和联网更新等仍需各自验证。版本元数据使用构建检查而不是复制断言充当业务测试，具体命令见[发布与版本元数据](#发布与版本元数据)。

[CI 工作流](../.github/workflows/ci.yml)还包含容器启动与 `/readyz` 检查，以及真实 systemd 安装、端口和容量变更、更新与卸载的 smoke test。这些部署检查不等于 API、同步和权限逻辑的全面回归测试。真实 Obsidian 桌面/移动端、浏览器交互及多设备数据一致性仍需相应环境验证；本文列出的命令本身不构成通过记录。

## 服务端扩展示例

这与上面的 Obsidian 客户端插件是两种不同的安装对象；服务端 ZIP 从管理后台「插件管理」上传。

- [`examples/server-plugin-echo`](../examples/server-plugin-echo/)：可信原生可执行插件，使用 `pkg/ossplugin` SDK，展示路由、Hook、中间件、后台页、任务、迁移和生命周期。按服务端实际 OS/架构编译；清单声明 Windows amd64 的 `plugin.exe`、Linux amd64 的 `plugin`、Linux arm64 的 `plugin-arm64`。例如在 Linux amd64 上从项目根目录执行：

  ```sh
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o examples/server-plugin-echo/plugin ./examples/server-plugin-echo
  ```

  将 `manifest.json` 与对应入口程序置于 ZIP 根目录，按需包含声明的资源目录，再上传。不要把 Windows 示例中的 `plugin.exe` 无条件用于 Linux 主机；原生插件属于受信任进程，不是 WASM 沙箱。
- [`examples/server-plugin-sections/main.go`](../examples/server-plugin-sections/main.go)：设置分组及 `blog.home_posts` Hook 示例。目录没有随附清单或打包脚本，不是可直接上传的完整安装包；集成方法以[服务端插件文档](server-plugins.md)为准。
- [`examples/server-plugin-shirone`](../examples/server-plugin-shirone/)：包含博客资源与设置型 WASM 的完整示例。从项目根目录运行 `python3 examples/server-plugin-shirone/build.py`，生成 `bin/themes/shirone.zip` 和 `.zip.sha256`；不需要额外 WASM 编译器。修改资源后先提升该示例自己的清单版本，再构建上传，详见[示例说明](../examples/server-plugin-shirone/README.md)。这些示例不会由主项目 Release 工作流自动打包发布。

## 发布与版本元数据

[Release 工作流](../.github/workflows/release.yml)由标签推送触发，接受带可选 `v` 前缀的 `X.Y.Z` 或 `X.Y.Z-rc.N`。提交必须属于远端 `main` 历史；工作流执行统一版本检查，并要求标签版本与客户端构建版本一致。预发布使用 GitHub prerelease，容器不更新 `latest` 标签。构建服务端资产前执行 `go test -race ./...`。

| 元数据 | 当前职责与维护边界 |
| --- | --- |
| 根 `manifest.json` | 仓库级 Obsidian 插件清单，也是客户端市场更新提醒实际请求的文件；维护插件 ID、当前版本、最低宿主版本等。 |
| `plugin/manifest.json` | 本地安装及 Release 三文件中的清单。应与根清单保持一致；当前两处均为 0.3.1，ID 均为 `oss-sync`。 |
| `plugin/package.json` / `package-lock.json` | npm 工具链与依赖锁定；发布时一起维护包版本。npm 包名不决定 Obsidian 插件 ID。 |
| 根 `versions.json` | 插件版本到最低 Obsidian 版本的兼容映射，供社区插件版本选择使用；不是服务端或客户端自动更新任务状态。 |
| `plugin/versions.json` | 根兼容映射的完整副本；两者必须一致，由 `scripts/check-versions.py` 在 CI 和 Release 中检查。 |
| `internal/version` | 服务端版本、提交和构建时间；Release 通过 Go `-ldflags` 注入。未注入时 `Version=dev`，明确表示源码构建，不冒充某个正式版本，也不能使用发布版自更新。 |

版本元数据检查命令：

```sh
python3 scripts/check-versions.py
python3 scripts/check-versions.py --release v0.3.1
```

统一检查覆盖客户端清单、npm 包和锁文件的版本一致性、两份兼容表的一致性，以及正式版本的 `minAppVersion` 映射。CI 和 Release 都会执行；它只检查，不自动修改文件。发布前需要同时维护这些文件，不能只修改 `package.json`。根清单直接参与市场更新提醒，允许在发布准备时保留上一正式版，但其版本和最低宿主版本必须已有兼容映射；不能提前公告尚不可获取的市场版本。RC 构建不要求写入正式市场兼容表。

工作流生成的交付物：

- **服务端运行包**：Linux/macOS 的 amd64、arm64，以及 Windows amd64。命名为 `oss-sync_<version>_<os>_<arch>.tar.gz`（Windows 为 `.zip`），由 [`scripts/package-release.py`](../scripts/package-release.py)把已编译程序与默认生产配置组合；包内仅有 `bin/oss-server`（Windows 为 `.exe`）、`configs/config.prod.yaml` 和 `VERSION`，没有用户数据。
- **客户端文件**：`main.js`、`manifest.json`、`styles.css` 三个独立 Release 资产，并生成资产 attestation；不生成客户端 ZIP。
- **部署与校验文件**：`oss.sh`、`checksums.txt`。校验和覆盖该次收集的运行包、客户端文件及脚本。
- **容器镜像**：`ghcr.io/<仓库所有者小写>/oss-sync-server:<version>`，平台为 Linux amd64/arm64；正式版额外更新 `latest`。

仅需打包已有服务端程序时，可从项目根目录运行（示例平台必须与输入程序一致）：

```sh
python3 scripts/package-release.py --binary bin/oss-server \
  --version 0.3.1 --os linux --arch amd64 --output release
```

打包脚本不编译、不注入程序版本、不运行测试，也不发布 GitHub Release。示例中的版本参数不会改变输入二进制的 `--version`；正式发布应遵循工作流的版本注入与资产生成过程。

## 客户端与服务端更新

**客户端是市场更新提醒，不是插件自更新器。** [`plugin/src/plugin-update.ts`](../plugin/src/plugin-update.ts)固定读取 `https://raw.githubusercontent.com/helantianshen/oss-sync/HEAD/manifest.json`，仅接受 ID 为 `oss-sync` 且版本是严格 `X.Y.Z` 的正式版；不使用 latest-release API 或预发布标签作为提醒来源。

[`plugin/src/main.ts`](../plugin/src/main.ts)在 Obsidian 布局就绪后检查一次，此后运行期间每 30 分钟检查，与 OSS 登录或管理员身份无关。发现更新只显示 Notice，让用户到 Obsidian 社区插件市场更新；失败不弹出错误提醒，但会写入诊断事件。插件设置区只展示当前版本与市场更新说明，没有下载源选择、手动下载安装或替换插件文件的按钮。它不会下载 Release 三文件，也不会更新服务端。

**服务端更新由服务端管理。** 管理员可以在后台或客户端「服务端更新」区检查、触发和轮询服务端更新任务；客户端管理员启动后也会延迟自动检查，但不会自动执行升级。服务端的官方/代理/自定义下载源属于服务端配置，与客户端固定的根清单 URL 无关。

更新执行方式取决于部署：`oss.sh` 当前安装的 systemd path/service 由宿主机任务处理换装重启；普通二进制在满足平台、程序文件与目录权限等条件时使用 helper；Docker 应重建/替换镜像，不应依赖容器内替换可执行文件。支持条件、校验和回滚边界见[部署指南](deployment.md)。

旧的[插件更新加速源方案](archive/plugin-update-source-spec.md)仅作历史记录，不能据此配置当前客户端或宣称仍有自动下载、文件替换和回滚功能。
