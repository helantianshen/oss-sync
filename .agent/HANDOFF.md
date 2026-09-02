# HANDOFF — OSS Sync 当前工作状态

## 当前目标

维护已发布的 `0.1.16` 部署与更新流程，完成更新源统一，并拆分网页与插件的认证会话有效期。

## 当前状态

- `0.1.16` 已公开发布；网页更新下载源选择已包含在该版本中，并通过本地与 Docker 验证。
- 原生二进制网页更新和全局 `oss` 更新均可选择 `gh-proxy.com`、GitHub 官方或自定义 HTTPS 加速前缀；版本检查与文件下载使用同一来源，全量 Go、WebUI、插件和脚本检查通过。
- 本轮来源统一改动仍在工作区中，尚未提交或发布新版本。
- 网页会话默认 24 小时，插件设备令牌默认 30 天；插件令牌过期后会清除本地认证并提示重新登录。

## 已完成工作

- `install.sh`：
  - 官方 `curl | bash` 地址保持不变，安装时可选内置 `gh-proxy.com` 文件加速、GitHub 官方下载或自定义文件加速前缀；
  - 按主机架构下载 Release 中的 `oss-sync-image_linux_amd64.tar.gz` / `arm64.tar.gz`，`checksums.txt` 和镜像归档均使用用户所选下载源，SHA-256 校验通过后执行 `docker load`；
  - `OSS_RELEASE_PROXY=official` 支持非交互官方直连，自定义前缀可通过同名变量传入；`OSS_IMAGE` 仅作为高级 Registry 镜像覆盖保留；
  - 支持自定义端口、绝对部署路径和项目总容量上限（GiB，`0` 不限）；
  - 新安装使用 `<部署路径>/data` 绑定挂载，容器内继续使用 `/app/data`；
  - 重复执行会下载最新 Release，并从现有容器复用端口、部署路径和容量标签；
  - 检测到旧版命名卷时继续使用原卷，避免升级后出现空数据库；
  - 仅在目录属主不符合容器 UID/GID 时请求提权；
  - 保留健康检查、失败恢复旧容器和首次管理员安全提示。
- 应用层项目总容量：
  - `storage.max_total_size_mb` / `OSS_STORAGE_MAX_TOTAL_SIZE_MB` 配置整个数据目录上限；
  - 新增 `internal/storagequota`，统计数据目录内全部常规文件并串行化扩容写入；
  - 普通同步、V2 同步、协作上传和历史恢复在提交前检查总容量；覆盖写会为历史快照预留旧文件空间；
  - 超限返回 HTTP 507 和稳定代码 `project_storage_quota_exceeded`；插件提供中英文提示；
  - 管理后台数据页显示项目数据目录实际用量与部署上限，实时指标同步刷新。
- 在线更新页：
  - 检查和触发按钮改为纯页内按钮，不再依赖表单提交；
  - Release URL 改为只读文本，不再打开外部页面；
  - 检查期间显示提示，仅当接口明确返回有新版本时才显示确认更新按钮；
  - 新版本、确认重启和更新已开始均提供中英文提示；已是最新版或检查失败时清空候选并隐藏确认按钮；
  - 修复 `script-src 'self'` / `style-src 'self'` CSP 拦截内联更新脚本和样式的问题：逻辑迁入已有 `app.js`，模板通过 `data-*` 提供本地化文案，并用 `hidden` 控制确认按钮；
  - 网页更新增加 Release 来源选择，默认使用 `gh-proxy.com`，可切换 GitHub 官方或输入自定义 HTTPS 加速前缀；版本元数据检查与文件下载使用同一来源，代理下载后继续校验 GitHub 提供的文件大小与 SHA-256；
  - Docker 镜像通过 `OSS_DEPLOYMENT_MODE=container` 明确标记为宿主机管理；后台仍可检查新版本，但不会尝试改写容器内 `/app/oss-server`，也不显示确认更新按钮；发现新版本后提示在服务器运行 `oss` / `oss-sync` 并选择 1 更新；
  - 容器内触发更新时返回稳定代码 `external_update_required`，页面不再展示 `/app` 不可写的底层权限错误；
  - 保留原有 CSRF、确认、状态轮询和错误展示。
- 全局管理命令：
  - 安装完成后写入部署目录中的 `install.sh`、`manage.sh`，并安装 `/usr/local/bin/oss` 与 `/usr/local/bin/oss-sync` 两个入口；
  - 管理菜单支持更新、保留数据卸载、状态/版本/端口/空间查看、启动、停止、重启、修改项目容量和修改端口；菜单编号为 `0-8`；
  - 更新、容量和端口调整复用原安装器的容器重建、健康检查与失败恢复，不维护第二套 Docker 创建参数；
  - 非交互数字选项成功后明确返回状态码 0；`OSS_GLOBAL_BIN_DIR` 可覆盖命令目录用于无特权测试，默认仍为 `/usr/local/bin`；
  - 全局命令若已被其他程序占用会停止安装，不覆盖无关文件。
- Release 发布：
  - 继续发布 GHCR amd64/arm64 多架构镜像；
  - 额外导出两个可由 `docker load` 导入的架构镜像归档，归档内统一标记为 `oss-sync-server:release`；
  - 对服务端二进制、插件文件和容器归档等全部 Release 资产生成 `checksums.txt`；
  - 新 Release 同时发布 `install.sh`、`manage.sh` 并纳入 `checksums.txt`；安装器兼容当前尚无管理资产的 `0.1.13`，过渡期回退官方源码地址；
  - CI 一键安装冒烟测试通过仅存在于代理路径的本地 Release 资产执行下载、校验和导入，防止校验文件重新绕过代理。
  - `0.1.16` 已公开发布：包含网页更新下载源选择、GHCR amd64/arm64 多架构镜像、两个架构镜像归档、服务端/插件资产、`install.sh`、`manage.sh` 和覆盖全部资产的 `checksums.txt`。
- README、Compose 和 CI 已同步新增配置及安装器验证。
- CI 稳定性：
  - WebUI 测试通过独立网页会话有效期签发 Cookie，避免零秒测试会话跨秒后被重定向登录；
  - 后端 race 测试串行执行包并将单包超时设为 15 分钟，适配包含 bcrypt 的慢速 GitHub runner。
- 认证会话：
  - 保留普通 API `jwt_ttl_hours` 的 72 小时默认值，网页会话和插件设备令牌分别使用 `web_session_ttl_hours: 24` 与 `device_jwt_ttl_hours: 720`；旧配置缺少新字段时自动使用这两个默认值；
  - `OSS_WEB_SESSION_TTL_HOURS` 与 `OSS_DEVICE_JWT_TTL_HOURS` 可分别覆盖网页和设备有效期；
  - 只有真实 JWT 过期才返回稳定代码 `token_expired`，其他 401/403 不会触发插件自动登出；
  - 插件收到过期响应后删除内存与持久 Token、停止同步和协作并提示重新登录，同时保留设备 ID 与 Vault 绑定。

## 重要决策

- GitHub Release 文件代理不用于 `docker pull`；发布流程先把 OCI 镜像导出为压缩归档，安装器再通过文件代理下载并执行 `docker load`。
- 国内网络无法保证访问 GitHub Release，即使小体积 `checksums.txt` 也会超时，因此选择加速源后校验文件与归档走同一代理；SHA-256 可检测传输损坏，但不能防御代理同时替换两者，若该信任边界不可接受应升级为签名校验。
- 当前 SQLite 一键部署没有 PostgreSQL 等外部 Docker Hub 依赖，因此不修改全局 `/etc/docker/daemon.json`；未来引入依赖容器时可直接使用 1Panel 的 `docker.1panel.live/library/<image>:<tag>` 完整地址。
- 项目容量采用跨发行版的应用层限制，不修改 Docker daemon、不创建 loop 文件系统，也不依赖 XFS project quota。
- 配额统计以 `storage.data_dir` 为边界。同步临时文件会先写入该目录，再在提交前计入检查；SQLite/WAL 等内部增长仍可能造成少量瞬时超出，因此它不是文件系统硬配额。
- 全局配额写锁是单进程上限；当前产品为单实例部署。若未来支持多实例写同一数据目录，需要升级为跨进程锁或共享配额服务。
- 修改端口和容量时使用当前本地镜像重建容器；“更新”选项才下载最新 Release。卸载不删除绑定目录或旧命名卷。
- 容器镜像是不可变部署单元，更新由宿主机管理脚本替换镜像和容器；不向容器内 `/app` 授予写权限。原生二进制部署继续使用进程内更新能力。

## 修改的重要文件

- `install.sh`
- `manage.sh`
- `internal/config/config.go`、`configs/config.*.yaml`
- `internal/config/auth_config.go`、`internal/auth/accounts.go`、`internal/auth/middleware.go`
- `internal/webui/webui.go`、`plugin/src/api.ts`、`plugin/src/main.ts`
- `internal/storagequota/quota.go`
- `internal/syncapi/upload.go`、`v2.go`、`collab_upload.go`、`history.go`
- `internal/webui/system_metrics.go`、模板、指标脚本和语言文件
- `plugin/src/i18n.ts`、`plugin/src/localized-error.ts`
- `.github/workflows/ci.yml`、`.github/workflows/release.yml`、`docker-compose.yml`
- `internal/webui/admin_update_test.go`
- `internal/webui/assets/app.js`、`internal/webui/assets/console.css`、`internal/webui/templates/layout.html`
- `plugin/package.json`、`plugin/package-lock.json`
- `internal/update/capability.go`、`internal/update/errors.go`、`internal/update/handler.go`
- `internal/update/download_source.go`、`internal/update/service.go`
- `internal/update/github.go`、`internal/update/update.go`、`internal/update/update_test.go`
- `Dockerfile`
- `README.md`、`README_zh.md`

## 验证情况

- `bash -n install.sh`、CI/Release Workflow YAML 解析、`docker compose config --quiet` ✅
- `go test ./... -count=1`、`go vet ./...` ✅
- Node.js 26 临时环境：`npm ci`、`npm exec tsc -- --noEmit`、`npm test`（250 项）、`npm run build` ✅
- Docker Desktop 真实验证 ✅：本地构建镜像，以自定义绑定目录、端口和 1 GiB 上限首次启动；重复执行升级后健康检查正常、挂载路径和容量环境变量正确、测试数据保持。
- 新 Release 安装链路真实验证 ✅：生成 Docker 压缩归档和 `checksums.txt`，经 `file://` 下载、SHA-256 校验、`docker load` 后健康启动；截断归档后安装器在导入前以“SHA-256 校验失败”退出。
- 当前 `gh-proxy.com` 对已发布的 0.1.12 Linux amd64 Release 资产 HEAD 请求返回完整文件长度 ✅。
- 本轮 `bash -n install.sh` 与 CI/Release Workflow YAML 解析 ✅。
- 本地代理隔离测试 ✅：官方路径不存在、仅代理路径提供 `checksums.txt` 与归档时，安装、校验、`docker load` 和健康检查通过。
- Release `0.1.13` 工作流 ✅：多架构 GHCR、两个镜像归档、全部资产校验文件和 GitHub Release 发布成功。
- 主分支 CI ✅：Backend `go test -race ./...` / `go vet ./...`、Plugin 全套检查、Container 及一键安装冒烟测试全部通过。
- 公网安装 ✅：从官方 `raw.githubusercontent.com/.../install.sh` 启动，通过 `gh-proxy.com` 下载 947 B 校验文件和 12.3 MiB amd64 归档，SHA-256、导入、健康检查及 `--version=0.1.13` 均通过。
- CI 稳定性本地验证 ✅：`TestAdminUpdate_SuccessfulCheckAndTrigger` 在 race 下连续 20 次通过；`go test -race -p 1 -timeout 15m ./...` 与 `go vet ./...` 通过。
- 最新主分支 CI `33292980542` ✅：Backend、Plugin、Container 及一键安装冒烟测试全部通过。
- 本轮 `go test ./... -count=1`、更新页定向测试、`go vet ./...` ✅。
- 容器更新边界定向测试 `go test ./internal/update ./internal/webui -count=1` ✅；全量 `go test ./... -count=1` 与 `go vet ./...` ✅。
- 真实 Docker 镜像构建与运行检查 ✅：镜像以 `10001:10001` 运行，包含 `OSS_DEPLOYMENT_MODE=container`，且 `/app` 保持不可写；测试镜像与临时 Docker 配置已清理。
- 本轮 `bash -n install.sh manage.sh`、CI/Release Workflow YAML 解析 ✅。
- 使用导出的假 Docker 函数执行 `manage.sh 3` ✅：容器发现、健康状态、版本、端口、数据路径/用量和容量上限输出符合预期。
- 真实 Docker 冒烟 ✅：经隔离文件代理下载镜像、`install.sh`、`manage.sh` 并完成 SHA-256 校验；默认 `/usr/local/bin/oss`、`oss-sync` 可用；状态、更新、容量 1→2 GiB、端口切换、停止、启动、重启、退出和保留数据卸载全部通过。
- 冒烟测试的专用容器、镜像、全局命令和临时目录均已清理，无遗留资源。
- Docker 测试容器、镜像及临时数据已清理。
- Release `0.1.14` 工作流 `33317586142` ✅；主分支 CI `33317582647` ✅，Backend race/vet、Plugin 和 Container/一键安装冒烟全部通过。
- `0.1.14` 公网安装 ✅：官方 `curl | bash` 入口经 `gh-proxy.com` 下载校验文件、amd64 镜像归档、`install.sh` 和 `manage.sh`，SHA-256 校验、`docker load`、健康检查、版本 `0.1.14`、全局 `oss` 状态输出、容器部署标记及保留数据卸载均通过；隔离测试资源已清理。
- 更新页 CSP 修复：`node --check internal/webui/assets/app.js`、`go test ./internal/webui -count=1`、`go test ./... -count=1`、`go vet ./...` ✅。
- Release `0.1.15` 工作流 `33514736600` ✅；主分支 CI `33514723355` ✅，Backend race/vet、Plugin 和 Container/一键安装冒烟全部通过；Release 资产与 `checksums.txt` 完整。
- 网页下载源选择：`go test ./internal/update ./internal/webui -count=1`、`go test ./... -count=1`、`go vet ./...`、`node --check internal/webui/assets/app.js` ✅；模板无 CSP 禁止的内联脚本或样式 ✅。
- 网页下载源 Docker 验证 ✅：实际登录容器后台后，容器部署正确显示“宿主机管理”且不提供无效的容器内更新；以可写原生模式启动的隔离容器正确显示 `gh-proxy.com`、GitHub 官方和自定义 HTTPS 三个下载源，默认项、条件输入框、CSP 与静态资源版本均正确。
- Docker 内运行 `go test ./internal/update -run TestResolveDownloadURL -count=1` ✅，官方、内置代理、自定义代理和无效地址后端解析路径通过。
- Release `0.1.16` 工作流 `33518389958` ✅：服务端与插件资产、多架构 GHCR、两个离线镜像归档、`checksums.txt` 和公开 GitHub Release 均发布成功；13 个资产状态均为 uploaded。
- 本轮统一来源链路验证 ✅：`go test ./... -count=1`、`go vet ./...`、`go build -o server.exe ./cmd/server`、`npm exec tsc -- --noEmit`、`npm test`（250 项）、`npm run build` 全部通过。
- 真实网络验证 ✅：当前环境直连 GitHub API 返回 403，而通过 `gh-proxy.com` 检查公开 Release 成功返回最新版本 `0.1.16`；管理后台自定义来源请求正确携带 `download_source` 与 `download_proxy`。
- 插件构建依赖安全修复 ✅：`esbuild` 从 `0.20.2` 升级至 `0.28.2`，消除 `GHSA-67mh-4wv8-2f99`；全新 `npm ci` 可重现，`npm audit` 为 0，TypeScript 检查、250 项测试和生产构建通过。
- 一键安装 CI 兼容修复 ✅：本地 Release 资产改用 `file://` 基地址，管理脚本更新显式选择 `official`；安装器会在已有镜像的本地配置操作前归一化 `official`，修改容量或端口不再被 HTTPS 校验误拒绝，生产环境的 HTTPS 自定义源要求保持不变。
- 认证会话拆分验证 ✅：网页 Cookie/JWT 为 24 小时、插件设备 JWT 为 30 天；`go test ./... -count=1`、`go vet ./...`、服务端构建、TypeScript 检查、插件 253 项测试及生产构建全部通过。

## 已知问题 / 风险

- 应用层容量不是宿主文件系统硬配额；进程外写入、数据库/WAL 增长以及很小的元数据开销无法做到字节级绝对封顶。
- 内置文件代理属于第三方服务，可用性可能变化；用户可随时选择 GitHub 官方或传入自定义加速前缀。
- 校验文件与归档使用同一第三方代理时，SHA-256 只保证两者一致；它不能替代发布签名。
- 旧版 `oss-data` 命名卷不会自动迁移到新部署目录，升级时优先保证数据安全。
- 默认公开监听仍存在首个注册者成为管理员的抢注窗口，这是用户已明确接受的部署取舍。
- Docker 部署仍由宿主机 `oss` / `oss-sync` 管理，网页下载源选择只在具备进程内更新能力的原生二进制部署中显示。
- 选择 GitHub 官方来源时仍受所在网络和 GitHub 匿名 API 限额影响；无法直连时应使用内置代理或可信的自定义 HTTPS 前缀。

## 剩余工作

- 无必须的未完成工作。

## 推荐下一步

- 审查并提交本轮未提交的统一来源改动，随后按需发布新版本并执行真实 Release 更新验证。
