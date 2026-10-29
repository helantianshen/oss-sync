# 部署与运维

本文覆盖安装、运行配置、HTTPS 反向代理、备份、更新和旧部署迁移。项目概览见 [README](../README_zh.md)，其他文档见[文档索引](README.md)。

| 部署方式 | 适用范围 | 默认端口 | 更新方式 |
| --- | --- | --- | --- |
| `oss.sh` + systemd | Linux amd64 / arm64，推荐长期运行 | `8080` | `sudo oss update`，或网页触发宿主机托管更新 |
| Docker | Linux amd64 / arm64 发布镜像，或自行从源码构建 | 容器 `8080` | 替换或重新构建镜像并重建容器 |
| 手动运行包 | Linux、macOS、Windows 的已发布平台 | `8080`，必须设置 `OSS_ENV=prod` | 手动替换程序，或满足权限条件时使用应用自更新 |
| 源码运行 | 开发调试或自行维护的平台 | 未设置 `OSS_ENV` 时为 `9090` | 自行构建与部署 |

无论采用哪种方式，都应先完成[安全初始化与反向代理](#安全初始化与反向代理)，再公开访问。更新前先做[一致性备份](#备份恢复与数据库迁移)。

## systemd 安装支持范围

一键安装支持 Linux amd64 和 arm64，要求 systemd 正在运行及 root 权限。依赖 Bash、Python 3、curl、coreutils、findutils、util-linux 的 flock，以及 useradd/getent；脚本只检查依赖，不安装工具链或 Docker。不支持的系统/架构明确退出，不自动构建源码。

Release 独立发布 `oss.sh` 和各平台运行包，并在 `checksums.txt` 中记录脚本和包的 SHA-256：

```text
oss-sync_<版本>_linux_amd64.tar.gz
oss-sync_<版本>_linux_arm64.tar.gz
oss-sync_<版本>_darwin_amd64.tar.gz
oss-sync_<版本>_darwin_arm64.tar.gz
oss-sync_<版本>_windows_amd64.zip
oss.sh
checksums.txt
```

每个包只包含 `bin/oss-server`（Windows 为 `bin/oss-server.exe`）、`configs/config.prod.yaml` 和 `VERSION`，不包含脚本或 data。`scripts/package-release.py` 统一生成该布局；Windows 和 macOS 暂仅提供运行包，没有自动部署脚本。手动运行时以解压目录为工作目录，并设置 `OSS_ENV=prod`。

旧格式资产名 `oss-server_<版本>_<系统>_<架构>` 不再由当前发布流程生成，新的后端更新器严格识别 `oss-sync_...`；不能识别新资产名的旧版后端，需要先通过宿主机脚本或手动替换升级。GHCR 镜像发布保留，但不再发布 Docker 离线镜像归档附件。具体版本是否可下载，以对应 Release 的实际附件为准。

## 安装与目录

```bash
curl -fsSL https://raw.githubusercontent.com/helantianshen/oss-sync/main/oss.sh -o oss.sh
sudo bash oss.sh install
```

执行前应审阅脚本，或从已确认的固定 Release 获取脚本。上面的 `main` 地址会随源码更新；安装阶段对 Release 包及配套脚本的 SHA-256 检查，不等于对首次执行的引导脚本进行了独立签名验证。

不带参数时，未安装自动进入安装流程，已安装进入管理菜单；`install` 显式安装或重装。脚本会从自身所在目录或已有 unit 识别部署位置。

默认目录 `/opt/oss-sync`，可通过 `sudo env OSS_INSTALL_DIR=/srv/oss-sync bash oss.sh install` 指定。路径只允许字母、数字、下划线、点、短横线和斜杠，不能是符号链接。由于服务的 `ProtectHome=true`，不要部署到 `/home`、`/root` 或 `/run/user` 下。

```text
/opt/oss-sync/
  bin/oss-server             程序，root 所有
  configs/config.prod.yaml   应用配置，root 可写、服务组可读
  configs/config.prod.yaml.dist  当前版本的默认模板
  service.env                管理脚本生成的环境覆盖
  deployment.env             root 专用部署元数据
  .binary-deployment         二进制部署标识
  data/                      服务账户可写，默认包含 SQLite 与正文
  VERSION                    当前安装版本
  oss.sh                     独立发布的统一安装管理脚本
/etc/systemd/system/oss-sync.service
/etc/systemd/system/oss-sync-update.path
/etc/systemd/system/oss-sync-update.service
/usr/local/bin/oss           指向 oss.sh
/usr/local/bin/oss-sync      指向 oss.sh
```

服务账户和组为 `oss-sync`，不以 root 运行。unit 仅授予绑定低端口所需的 `CAP_NET_BIND_SERVICE`，不授予其他 Linux capability。程序和配置不允许服务账户写入。systemd 设置 `WorkingDirectory` 为部署目录，使用 `OSS_ENV=prod` 加载生产 YAML；默认数据和 SQLite 路径相对该目录解析。日志写入 journal，不创建单独的日志轮转服务。

默认监听 `0.0.0.0:8080`，不会自动配置防火墙、TLS 或反向代理。健康检查访问本机 `127.0.0.1:<端口>/readyz`，YAML 中的监听地址须允许此访问。受 `ProtectHome` 与文件权限约束，外部数据库/存储目录需要自行配置合适权限；推荐使用默认 data 目录。

端口和容量保存在生成的 `service.env`，会覆盖 YAML 同名设置，通过管理命令 `sudo oss 4` 修改。其他配置直接编辑 YAML 后执行 `sudo oss 3` 重启生效。`service.env` 会在安装、更新、修改端口或容量时重写，不适合作为自定义环境变量的长期配置入口；额外覆盖应由管理员配置 systemd drop-in，并检查是否覆盖了管理菜单中的设置。更新保留现有 YAML，将新版本模板保存为 `configs/config.prod.yaml.dist`，管理员可比较并按发布说明合并新增配置项。不要手工编辑或 source 来历不明的 `deployment.env`。

## 下载与版本

首次安装可交互选择默认加速、官方直连、自定义 HTTPS 前缀；重复执行安装器默认复用已保存前缀。管理菜单中的更新会让用户重新选择更新源，也支持环境变量：

```bash
sudo env OSS_RELEASE_PROXY=official oss 1
sudo env OSS_RELEASE_PROXY=https://gh-proxy.com/ oss 1
sudo env OSS_RELEASE_PROXY=https://mirror.example.com/ oss 1
sudo env OSS_RELEASE_PROXY=official OSS_VERSION=v1.2.3 oss 1
```

`OSS_VERSION` 必须与远端 tag 一致；资产名去除其 `v` 前缀。未指定时只查询一次 latest-release API，再从固定 tag 下载全部文件。代理同时覆盖 API、校验文件、运行包和统一脚本；不会失败后自动直连。代理和校验文件来自同一源，SHA-256 不是独立发布签名。

`OSS_RELEASE_BASE_URL` 是高级离线/测试入口，必须同时设置不带 v 的 `OSS_VERSION`，例如固定版本的 HTTPS 目录或 `file:///绝对目录`。本地文件源同时设置 `OSS_RELEASE_PROXY=official`。部署目录和全局命令位置分别由 `OSS_INSTALL_DIR`、`OSS_GLOBAL_BIN_DIR` 指定；后者默认 `/usr/local/bin`。

## 服务管理与更新

`sudo oss` 或 `sudo oss-sync` 打开菜单；菜单头部显示已安装版本、服务状态、探测到的主机 IP 地址和 `data/` 目录存储用量。显示地址不是自动配置的公网域名，也不代表防火墙或 HTTPS 已就绪。

主菜单及二级菜单：

```text
1 更新        ← 先选更新源：默认加速 / 官方 / 自定义 / 0 返回
2 停止        ← 服务运行时为“停止”，停止时为“启动”
3 重启
4 修改        ← 1 修改容量、2 修改端口、3 切换 SSE 明文令牌、0 返回
5 日志
6 卸载        ← 1 卸载全部（含数据）、2 保留数据、0 返回
0 退出
```

所有二级菜单均支持 `0` 返回主菜单，只有主菜单的 `0` 退出。也可直接传递编号或名称：

| 操作 | 命令 |
| --- | --- |
| 更新 | `sudo oss 1` / `sudo oss update` |
| 启动或停止（切换） | `sudo oss 2` / `sudo oss start` / `sudo oss stop` |
| 重启 | `sudo oss 3` / `sudo oss restart` |
| 修改容量或端口 | `sudo oss 4`，非交互可用 `sudo oss 4 1 <GiB>`、`sudo oss 4 2 <端口>` |
| 切换 SSE 明文令牌 | `sudo oss 4 3`，需交互确认，修改后另行重启 |
| 持续查看日志 | `sudo oss 5` |
| 详细状态 | `sudo oss status` |
| 卸载 | `sudo oss 6` / `sudo oss uninstall`，非交互可用 `sudo oss 6 1 yes`（全部）、`sudo oss 6 2 yes`（保留数据） |

修改容量/端口和卸载都支持直接把子菜单编号与数值作为参数传入，不传时逐级交互询问。注意 `oss start`、`oss stop` 与菜单 `2` 的实现相同，都是切换当前状态；需要幂等的明确操作时用 `sudo systemctl start oss-sync` 或 `sudo systemctl stop oss-sync`。SSE 开关只是修改 YAML，不自动重启；是否需要开启见下文的传输安全条件。

更新和修改端口/容量需要短暂停机。更新的下载、SHA-256、归档结构、程序版本和脚本语法检查均在停机前完成；下载失败不停止旧服务。包内路径、文件类型、VERSION 和二进制版本都通过校验后，备份旧程序、配置、脚本和主服务 unit，再停止服务、替换文件、启动并核对就绪版本。健康检查约等待 60 轮，每轮 HTTP 超时上限 2 秒，轮间等待 1 秒。失败时尝试恢复这些文件以及主服务的原启停状态；这不是整个部署目录或所有 systemd unit 的事务快照。

回退不恢复数据库、正文和数据库迁移。更新前应停止服务并做一致的数据备份；若旧程序不兼容新数据库，回退程序可能仍不能恢复业务。脚本不声称提供零停机更新，也不自动降级数据。断电或 SIGKILL 不属于 shell trap 可保证恢复的范围；保留的数据和配置可用于人工恢复。

systemd 使用 `Restart=on-failure`，异常退出由 systemd 重启。当前安装器同时设置 `OSS_UPDATE_MANAGER=systemd` 与 `OSS_UPDATE_VIA_PATH_UNIT=1`，并启用 `oss-sync-update.path`：管理员在网页触发更新后，应用只把目标版本请求写入 `data/.update/request`，由 root 身份的 `oss-sync-update.service` 调用 `oss.sh apply-web-update` 执行下载、校验、安装与重启。请求不能指定下载 URL，脚本拒绝无效版本或降级。更新执行日志见 `sudo journalctl -u oss-sync-update -u oss-sync`。

网页检查更新使用应用的 `update.download_source` / `download_proxy` 或本次检查选择的源；宿主机实际安装仍使用 `deployment.env` 保存的 `DEPLOY_PROXY`，不会自动沿用网页临时选择。两者均需能够访问目标 Release。若只设置 `OSS_UPDATE_MANAGER=systemd` 而没有 path-unit 标记，应用要求外部更新，不启动进程内 helper。

当前网页托管路径会去掉目标版本的 `v` 前缀，再把结果用作下载时的 Release tag。对于仅存在 `vX.Y.Z` 标签的 Release，[INFERENCE] 按 `oss.sh` 的目标版本处理与下载 URL 拼接逻辑静态推导，它会请求不匹配的无前缀下载路径；此处未执行真实更新复现。遇到此情形应改用 `sudo env OSS_RELEASE_PROXY=official OSS_VERSION=vX.Y.Z oss update`（替换为准确 tag 和所需下载源）。这是现有脚本的限制，不是网页能保证处理的标签兼容性。

托管更新请求目录由服务账户写入，必须保持部署目录、脚本、元数据与 unit 的 root 所有权；不要给其他账户写入 `data/.update` 的权限。若修改 `storage.data_dir`，应用会在新目录写请求，但安装器仍固定监听部署目录下的 `data/.update/request`，应由管理员同步调整托管更新配置，否则使用宿主机命令更新。

卸载分两种，均需二次确认：

- `1 卸载全部`：移除 unit、全局命令和整个部署目录，包括 `data/` 中的数据，不可恢复。
- `2 保留数据`：只移除 unit 和全局命令，运行目录、数据、配置及服务账户保留。

两种方式都会先 `systemctl disable --now` 停止服务。执行 `sudo bash /opt/oss-sync/oss.sh install` 可以重新注册服务；自定义路径请相应替换。保留原目录才能识别旧设置。

升级时一并下载、校验并原子替换同一 Release 的 `oss.sh`。本次流程继续执行已加载的脚本，新脚本从下次调用生效；升级失败时脚本与 VERSION、配置模板一起回退。

## Docker

### 从源码使用 Compose

仓库的 `docker-compose.yml` 从当前工作区构建 `oss-sync-backend:local`，不是自动拉取 GHCR 发布镜像。在仓库根目录执行：

```bash
docker compose up -d --build
docker compose logs -f backend
docker compose ps
```

默认映射宿主机 `8080` 到容器 `8080`，绑定所有宿主机接口。反代与容器位于同一主机时，建议首次启动就只绑定回环：

```bash
OSS_PORT=127.0.0.1:8080 docker compose up -d --build
```

`OSS_PORT` 是 Compose 的宿主端口映射变量，不是应用的监听变量；后续每次重建应保持同一设置，可放入 Compose 的 `.env`。Compose 只把 `OSS_STORAGE_MAX_TOTAL_SIZE_MB`（默认 `0`，不限）传入容器；宿主机设置 `OSS_DB_DSN` 等变量不会自动传入，须在 Compose `environment` 中显式增加。

Compose 使用逻辑名为 `oss-data` 的命名卷挂载 `/app/data`，实际卷名通常带 Compose 项目前缀，以 `docker inspect` 为准。`docker compose down` 保留卷，**`docker compose down -v` 会删除卷及其数据**；改变项目名或工作目录可能创建不同的卷，不要将空卷误认为数据丢失。健康检查由 Compose 定义，每 30 秒请求容器内的 `/readyz`；镜像本身没有 Docker `HEALTHCHECK`。

### 使用发布镜像

发布流程面向 Linux amd64 / arm64 推送 `ghcr.io/helantianshen/oss-sync-server:<版本>`；稳定版同时更新 `latest`，RC 不更新 `latest`。生产建议固定版本或镜像 digest。以下 `1.2.3` 仅示意版本格式，须替换为实际已发布版本：

```bash
VERSION=1.2.3
IMAGE="ghcr.io/helantianshen/oss-sync-server:$VERSION"
docker pull "$IMAGE"
docker volume create oss-sync-data
docker run -d --name oss-sync \
  --restart unless-stopped --stop-timeout 20 \
  -p 127.0.0.1:8080:8080 \
  -v oss-sync-data:/app/data \
  "$IMAGE"
docker logs -f oss-sync
```

镜像以 UID/GID `10001:10001` 运行，工作目录 `/app`，入口为 `/app/runtime/oss-server`，内置 `OSS_ENV=prod`、`OSS_SERVER_PORT=8080`、`OSS_STORAGE_DIR=/app/data` 与 SQLite DSN `/app/data/oss.db`。若改用绑定目录，宿主机必须提前赋予该 UID/GID 对数据目录的读写权限；不要以改成 root 运行代替权限配置。使用自定义生产 YAML 时，可只读挂载到 `/app/configs/config.prod.yaml`，但镜像已有的环境变量仍优先于 YAML，尤其是数据库、存储路径与端口。

默认构建参数 `VERSION=dev`，因此直接 Compose 构建通常显示开发版本，不能据此获得发布版本自更新能力。镜像中程序目录虽可写，也不应在容器内用应用自更新代替镜像更新：该改动位于容器可写层，重建即丢失，且不会升级镜像中的配置模板。不要向容器设置 systemd 托管更新标记。

### Docker 更新

先备份数据和部署配置。源码 Compose 部署应切换到目标源码版本并运行 `docker compose up -d --build`；发布镜像部署应拉取固定新版本，停止并移除旧容器，再用**相同数据卷、端口和环境配置**创建新容器，不要删除卷。仅运行 `docker restart` 不会使用新镜像。

保存旧镜像版本与部署参数以便人工回退。无论更新是否由镜像重建完成，新程序启动都会执行数据库迁移；回退镜像不会回退数据。若新数据格式与旧版不兼容，必须恢复同一备份点的数据库和正文。

## 手动运行包

从 [Releases](https://github.com/helantianshen/oss-sync/releases) 下载对应平台运行包及 `checksums.txt`，核对包的 SHA-256，再解压到独立目录。Linux/macOS 在解压目录运行：

```bash
OSS_ENV=prod ./bin/oss-server
```

Windows PowerShell 在解压目录运行：

```powershell
$env:OSS_ENV = "prod"
.\bin\oss-server.exe
```

运行包只有生产配置，遗漏 `OSS_ENV=prod` 会尝试读取不存在的 `configs/config.dev.yaml` 并失败。不要从任意目录直接调用程序绝对路径；配置文件、相对 SQLite DSN 和正文路径都依据**进程当前工作目录**，不是程序所在目录。需要非默认监听地址时可设置 `OSS_SERVER_HOST` / `OSS_SERVER_PORT`。为生产服务使用非 root 专用账户，并由管理员自行配置进程守护与开机启动；关闭终端不等于已配置长期服务。

Linux/macOS 可另开终端检查生产端口：

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Windows 可使用 `curl.exe` 执行相同请求。`/healthz` 返回环境与版本；`/readyz` 还检查数据库连通性，以及未解决的正文缺失/哈希不一致记录。健康检查通过不代替同步、权限和数据恢复验收。

普通手动部署未设置 systemd 更新标记时，发布版本在受支持平台上可使用应用自更新 helper；当前程序必须是非符号链接的普通文件，运行账户必须能写程序目录。开发版本、未支持平台或只读程序目录不能自更新。该方式通过 helper 替换程序、启动新进程并检查就绪，不是 systemd 托管更新；若已有外部进程管理器，应由它统一管理生命周期，避免和 helper 同时拉起实例。

手动替换更新时：先保存旧程序及配置并做一致性备份，停止服务；从新包取出程序和 `VERSION`，比较新生产模板后合并配置，不要覆盖已有配置或 data；保持工作目录、运行账户和环境配置一致，再启动并检查版本与业务。自动更新同样不能代替数据库/正文备份。

## 配置加载与优先级

配置实现以 `internal/config/config.go` 为准：

1. `OSS_ENV` 只接受 `dev`、`prod`，未设置时为 `dev`。
2. 必须存在当前工作目录下的 `configs/config.<环境>.yaml`；不是只靠环境变量即可免配置文件启动。
3. 读取 YAML 后，再应用实现中支持的环境变量覆盖；不是任意 YAML 字段都可自动映射为 `OSS_*`。
4. 配置在启动时加载，修改后需重启。数据库中保存的注册策略、账号、Vault 等不是每次从 YAML 重置。

| 配置项 | 开发配置 | 生产配置 / 镜像 | 可用环境覆盖 |
| --- | --- | --- | --- |
| 监听地址与端口 | `0.0.0.0:9090` | `0.0.0.0:8080` | `OSS_SERVER_HOST`、`OSS_SERVER_PORT` |
| 数据库 | SQLite，`data/oss.db` | YAML 同开发；镜像为 `/app/data/oss.db` | `OSS_DB_DRIVER`、`OSS_DB_DSN` |
| 正文根目录 | `data` | YAML 为 `data`；镜像为 `/app/data` | `OSS_STORAGE_DIR` |
| 数据目录容量上限 | `0`，不限 | `0`，不限 | `OSS_STORAGE_MAX_TOTAL_SIZE_MB` |
| 新数据库初始注册开关 | `true` | `true` | `OSS_ALLOW_ANONYMOUS_REGISTRATION` |
| Web / 设备令牌有效期 | `24` / `720` 小时 | 同开发 | `OSS_WEB_SESSION_TTL_HOURS`、`OSS_DEVICE_JWT_TTL_HOURS` |
| 设备过期 / 对账周期 | `90` 天 / `24` 小时 | 同开发 | `OSS_DEVICE_STALE_DAYS`、`OSS_RECONCILE_INTERVAL_HOURS` |
| 允许不安全查询令牌 | `false` | `false` | `OSS_ALLOW_INSECURE_QUERY_TOKEN` |
| 应用更新源 | `proxy`，默认加速源 | 同开发 | `OSS_UPDATE_GITHUB_REPO`、`OSS_UPDATE_DOWNLOAD_SOURCE`、`OSS_UPDATE_DOWNLOAD_PROXY` |

`server.max_file_size_mb` 默认 `100`，`max_multipart_memory_mb` 默认 `8`；后者只是 multipart 内存阈值，超出部分使用临时文件，并不意味着只允许上传 8 MiB。容量变量名称虽为 MB，转换使用 `<< 20`，实际单位为 MiB；安装器的容量输入是 GiB。应用层容量限制不能替代磁盘监控，也不预留磁盘空间。

JWT 签名密钥首次启动随机生成并保存在数据库，配置中的 `auth.jwt_secret` 只是兼容字段，运行时不会采用；没有用 `OSS_JWT_SECRET` 设置签名密钥的配置入口。已有数据库的公开注册开关以数据库记录为准，之后应在管理后台修改，重复设置初始环境变量不会将它重置。

数据库后台保存功能只是写当前环境的 YAML，重启后才生效，既不会迁移数据，也不能覆盖 `OSS_DB_DRIVER` / `OSS_DB_DSN`。systemd 安装的配置对服务账户只读，Docker 默认配置同样不是服务账户可写；这两种部署应由管理员改 YAML 或部署环境变量，而不是依赖网页保存数据库启动配置。

## 安全初始化与反向代理

### 首次初始化

- 服务本身启动的是 HTTP，不直接加载 TLS 证书。对外提供服务应使用 HTTPS 反向代理，并将后端监听限制在回环或受控私网，防火墙禁止用户绕过代理直连后端。
- 首个成功注册账户成为管理员，默认初始注册开放。首次启动前先用监听地址、防火墙或私网限制访问，由自己创建首个账户，再在管理后台按需关闭公开注册；默认不生成管理员账号密码。
- 登录会话 Cookie 为 `HttpOnly`、`SameSite=Lax`；CSRF Cookie 为 `SameSite=Lax`，但不是 `HttpOnly`。两者的 `Secure` **有条件设置**：请求存在 TLS，或 `X-Forwarded-Proto` 去空白后不区分大小写等于 `https`。普通 HTTP 请求不会得到 Secure Cookie。
- HTTPS 判断直接读取转发头，未验证该头是否来自可信代理。边界代理必须覆盖客户端提交的 `X-Forwarded-Proto`，不能透传；也应覆盖来源 IP 头，避免来源 IP 限流被伪造。不要把公开 HTTP 后端加上伪造的 `https` 头当作加密。
- 仅安装受信任的服务端插件。可执行插件运行在服务进程的账户权限边界内，不能把上传插件视作无风险的主题操作。

### Nginx 示例与 SSE

仓库没有独立的 Nginx 配置文件。以下是同一主机终止 TLS、转发到回环 `8080` 的最小示意，需自行替换域名、证书路径并安装管理 Nginx；不是安装器自动执行的步骤：

```nginx
server {
    listen 80;
    server_name sync.example.com;
    access_log off;
    return 301 https://sync.example.com$request_uri;
}

server {
    listen 443 ssl;
    server_name sync.example.com;
    ssl_certificate /etc/letsencrypt/live/sync.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sync.example.com/privkey.pem;

    # 应容纳单文件上限及 multipart 开销；按实际上传限制调整。
    client_max_body_size 110m;
    # SSE 的 URL 可能含 token，勿将完整查询串写入访问日志。
    access_log off;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
    }
}
```

使用此例前，systemd/手动运行可将生产 YAML 的 `server.host` 设置为 `127.0.0.1`；Docker 则使用上述回环端口映射。如果代理位于其他容器或主机，应使用可达的私网地址，并只允许代理来源访问后端。不要在 Docker 内将应用监听地址改为 `127.0.0.1` 后仍期望通过普通端口映射访问。

协作事件流使用 `/api/collaborations/stream`，还保留按 Vault 的 `/api/vaults/:vault_id/collaborations/stream`。代理须关闭响应缓冲与缓存，允许长连接；长轮询也需要足够的读取超时。上例对整个服务采用这些设置，无需 WebSocket Upgrade 配置。若还有 CDN、负载均衡器或其他代理层，也须核实其 SSE 缓冲、连接超时和日志策略。

SSE 查询参数令牌的实际放行条件是以下任一项：

1. 后端请求存在 TLS，或收到 `X-Forwarded-Proto: https`；
2. `sync.allow_insecure_query_token=true`（或对应环境变量）；
3. 请求的 Host 和对端地址**同时**为回环地址。

因此 HTTPS 在 Nginx 终止、内部以 HTTP 转发，但正确设置 `X-Forwarded-Proto: https` 时，**不必打开**不安全令牌开关。该头只描述原始协议，不加密代理到后端的链路；JWT 仍可能经过内部明文链路，须依赖本机/可信网络隔离。确需非回环明文 HTTP SSE 时才考虑该开关，它放宽传输限制，不会加密 URL；也可保留关闭并使用客户端的长轮询回退。不要在公开 HTTP 上发送登录凭据或查询令牌，且应在所有代理、监控、浏览器调试导出中避免记录完整带令牌 URL。

## 备份、恢复与数据库迁移

### 备份范围与一致性

至少保存以下内容，并将备份放在独立于运行盘的位置、限制读取权限：

- 有效数据库：默认 SQLite 及同目录可能存在的 WAL/SHM；使用 PostgreSQL 时对实际外部数据库单独备份。
- 完整 `storage.data_dir`，包括正文、历史、回收站、隔离对象、删除仓库归档和服务端插件等，不只是当前笔记文件。
- 生效配置、环境覆盖、systemd unit/drop-in 或 Compose 参数、运行版本与镜像 digest。JWT 签名密钥、账号和其他元数据在数据库中，不能只备份 YAML。
- 旧版本还可能把删除仓库归档保存在工作目录的 `backups/vaults`，当前代码保留兼容读取；若它在 data 之外，也需单独保存，尤其不要在容器迁移时遗漏未挂载到卷的归档。

数据库记录与磁盘对象共同组成服务状态；最简单可靠的方案是停止服务后备份同一时间点的两者。仅在运行中复制 SQLite 单文件、仅做 PostgreSQL 在线转储而同时继续修改正文，都不能保证应用层一致性。网页中的“已删除仓库备份”是删除前生成的单仓库归档，不是整个服务的灾难恢复备份。

默认 systemd + SQLite 可参考以下停机备份步骤。先确认没有进行中的更新，再停止更新监听与主服务，归档整个默认部署目录：

```bash
sudo systemctl stop oss-sync-update.path oss-sync &&
  sudo sh -c 'umask 077; tar -czpf "/var/backups/oss-sync-$(date +%Y%m%d-%H%M%S).tar.gz" -C /opt oss-sync' &&
  sudo systemctl start oss-sync &&
  sudo systemctl start oss-sync-update.path
```

示例要求 `/var/backups` 已存在且空间充足；自定义部署目录、部署目录外的 SQLite/正文目录和 systemd drop-in 要另行纳入。仅在归档成功后恢复服务；如果原先有意停止，应保持原状态。Docker 需先 `docker compose stop backend`（或停止对应容器），从 `docker inspect` 确认的卷/绑定目录取得完整副本，并保存 Compose 配置，再启动原容器。PostgreSQL 需在服务停写期间完成数据库转储与正文备份。

### 恢复与更换数据库

恢复时停止全部写入实例及托管更新监听，使用与备份匹配的程序版本、数据库和完整正文目录；恢复相应运行账户的文件权限及配置后再启动。不要只恢复程序，或把不同时间点的数据库和正文混用。恢复旧部署前检查 `data/.update/request` 是否残留待执行的更新请求，避免一启用 path unit 就立即再次升级；原有更新 unit 和服务配置也应由管理员核对。

启动会自动执行 GORM 表结构迁移和兼容数据回填，还会立即执行一次存储对账；不是无副作用的只读数据检查。应先保留原备份，再在隔离副本上确认目标版本兼容性。启动后核实 `/readyz` 的版本与状态，再验收账号、Vault、设备授权、上传下载、历史、回收站和分享。

支持把驱动配置为 `postgres` 并提供可连接的 DSN，但服务不负责安装 PostgreSQL，也不提供 SQLite → PostgreSQL 的数据搬迁命令。修改驱动/DSN 或在网页保存配置，只会让下次启动连接指定数据库；连接空数据库会初始化新库，不会导入原账号和笔记元数据。跨数据库迁移需管理员安排停写、完整数据转换与验证，并保留原数据库及正文备份；没有可直接承诺的一条命令无损切换。

## 旧 Docker 部署迁移

安装器不提供自动迁移。遇到未标记的已有 data 目录会退出；不要覆盖旧部署目录，先使用新的空目录安装。

1. 记录旧版本、配置、端口、数据库类型和 Docker 数据挂载位置。命名卷和绑定挂载均应以 `docker inspect` 为准。
2. 停止旧容器，备份数据库与完整正文目录，以及可能位于卷外的旧 `backups/vaults` 归档；SQLite 数据库及可能存在的 WAL/SHM 应作为整体在停机后复制。PostgreSQL 使用独立数据库备份流程。
3. 在独立目录安装新服务，可先使用另一个端口。停止新服务，再将备份数据复制到新 data 目录，合并原配置，保留新服务生成的 systemd 管理标记。
4. 将新 data 目录及复制的数据所有权设为新 `oss-sync` 账户和组；不要更改旧数据副本。确认数据库 DSN 与正文目录指向新位置。
5. 启动新服务，验证账号、Vault、同步和历史数据。若要沿用原端口，先确保旧容器停止，再通过管理命令改端口。
6. 验证后禁用旧容器自动启动；旧容器和备份的删除由管理员另行决定。不要让两个实例同时访问同一份 SQLite/正文目录。

全局 `oss` 命令如仍属于旧部署，新安装器会拒绝覆盖。可指定独立 `OSS_GLOBAL_BIN_DIR` 安装，迁移完成后再由管理员调整链接。新的 Release 不再提供旧 Docker 安装器所需的镜像归档和同名脚本，因此不要再用旧一键更新命令。继续使用 Docker 时应手动替换镜像，或先按上述步骤迁移。仓库代码变更本身不会修改正在运行的容器。

## 不支持架构的源码构建

管理员可以手动下载固定发布标签的源码，按该版本 `go.mod` 的工具链要求构建，并自行验证平台兼容性：

```bash
go build -trimpath -ldflags '-X github.com/helantianshen/oss-sync/internal/version.Version=1.2.3' -o oss-server ./cmd/server
```

其中版本必须与实际检出的发布源码一致。这不是一键安装器的自动回退路径，也不保证所有 Go 支持平台均兼容本项目。源码部署需要自行管理版本、服务和回滚；后续如增加受支持架构，应先加入发布及部署验证。
