# OSS Sync

OSS Sync 是一个自托管的 Obsidian 同步与分享项目，由 Gin 后端和 Obsidian 插件组成。后端保存账户、Vault、设备游标和文件元数据，插件负责监听本地文件变化、维护同步基线并处理冲突。

## 功能

- Markdown、附件和可选的 `.obsidian` 配置同步
- 文件新增、修改、删除和重命名
- 全量清单校验与基于 revision 的增量同步
- 多 Vault 隔离，一个账户可管理多个笔记仓库
- 多设备游标、设备重命名和吊销
- 冲突检测及远端覆盖、本地覆盖、保留双方三种处理方式
- 文件和文件夹公开分享
- Markdown、Obsidian 双链和本地图片渲染
- SQLite 默认存储，可切换 PostgreSQL
- 启动及定时存储对账

同步只使用 HTTP API。插件默认定时轮询远端 revision，服务端变更接口也支持最长 30 秒的等待参数，不依赖 WebSocket。

## 项目结构

```text
cmd/server/          后端入口
configs/             开发和生产配置
internal/            认证、同步、Vault、设备、分享和存储逻辑
plugin/src/          Obsidian 插件源码
plugin/tests/        插件同步逻辑测试
```

## 环境要求

- Go 1.25+
- Node.js 20+
- npm
- Obsidian 1.4+

## 启动后端

项目默认使用开发配置和 SQLite：

```bash
go run ./cmd/server
```

默认监听 `http://localhost:8080`，数据写入项目下的 `data/` 目录。

健康检查：

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

`/readyz` 除了检查数据库连接，还会在存在未解决的文件缺失或哈希不一致时返回 `503`。

### 配置环境

通过 `OSS_ENV` 选择配置文件：

```bash
OSS_ENV=dev go run ./cmd/server
OSS_ENV=prod go run ./cmd/server
```

对应文件：

- `configs/config.dev.yaml`
- `configs/config.prod.yaml`

以下环境变量会覆盖 YAML 配置：

| 环境变量 | 说明 |
| --- | --- |
| `OSS_JWT_SECRET` | JWT 签名密钥 |
| `OSS_ALLOW_ANONYMOUS_REGISTRATION` | 是否允许匿名注册普通用户 |
| `OSS_DB_DRIVER` | `sqlite` 或 `postgres` |
| `OSS_DB_DSN` | 数据库连接字符串 |
| `OSS_SERVER_HOST` | HTTP 监听地址 |
| `OSS_SERVER_PORT` | HTTP 监听端口 |
| `OSS_STORAGE_DIR` | 文件存储目录 |
| `OSS_DEVICE_STALE_DAYS` | 设备失效天数 |
| `OSS_RECONCILE_INTERVAL_HOURS` | 存储对账周期 |

生产环境至少应覆盖 JWT 密钥：

```bash
export OSS_ENV=prod
export OSS_JWT_SECRET='replace-with-a-random-secret'
go run ./cmd/server
```

### 用户注册

开发环境的用户表为空时，第一次注册会创建管理员。管理员建立后，默认只有已登录管理员可以创建其他用户。

匿名注册默认关闭，可通过配置开启：

```yaml
auth:
  allow_anonymous_registration: true
```

也可以使用环境变量：

```bash
export OSS_ALLOW_ANONYMOUS_REGISTRATION=true
```

匿名请求只能创建普通用户，即使请求中提交 `role: admin` 也不会获得管理员权限。生产环境建议先完成管理员初始化，再切换到 `OSS_ENV=prod`。

## 构建插件

```bash
cd plugin
npm ci
npm run build
```

构建后需要将以下文件放入 Obsidian Vault 的 `.obsidian/plugins/oss-sync/`：

```text
plugin/manifest.json
plugin/main.js
plugin/styles.css
```

在 Obsidian 中重新加载第三方插件并启用 **Obsidian Sync & Share**。随后在插件设置中填写：

1. 后端地址
2. 用户名和密码
3. 注册或登录
4. 当前本地 Vault 要绑定的服务端 Vault

插件会在本地 Vault 根目录维护 `.oss-sync-state.json`。该文件保存服务端 revision、待处理操作和冲突状态，不会上传到服务端。

## 同步机制

每个服务端 Vault 维护独立、单调递增的 revision。客户端提交修改时必须携带本地基线中的 `base_revision`：

- revision 一致时写入新版本
- revision 不一致时返回 `409`，由插件记录冲突
- 删除会立即移除服务端正文并保留墓碑
- 设备确认游标后，定时任务才能压缩对应墓碑
- 客户端游标落后于已压缩 revision 时，服务端返回 `410`，插件重新获取完整清单

每个修改请求还带有稳定的 operation ID，用于避免重试造成重复写入。服务端按 Vault 和路径加锁，保证并发修改时 revision 和磁盘内容一致。

插件支持两种同步入口：

- 全量同步：读取完整服务端清单，同时扫描本地文件
- 增量同步：读取上次游标之后的服务端变化，并处理本地待提交操作

## 文件存储

默认数据目录结构：

```text
data/
├── oss.db
└── vaults/
    └── <vault-id>/
        ├── files/
        ├── tmp/
        └── quarantine/
```

服务启动时会执行一次存储对账，之后按配置周期重复执行。对账会：

- 校验数据库哈希与磁盘内容
- 恢复可验证的上传或重命名备份
- 清理过期临时文件
- 隔离无数据库记录的文件
- 记录无法自动修复的存储问题

## 分享

登录用户可以从 Obsidian 文件菜单创建文章或文件夹分享。公开地址格式为：

```text
http://<server>/p/<share-id>
```

分享页面支持 GFM、Obsidian 双链、本地图片和默认主题。只有被分享内容实际引用的附件才能通过公开资源接口访问。

## 测试

后端：

```bash
go test ./...
go test -race ./...
go vet ./...
```

插件：

```bash
cd plugin
npm exec tsc -- --noEmit
npm test
npm run build
```

## 生产部署建议

- 使用反向代理提供 HTTPS
- 使用随机且长期稳定的 `OSS_JWT_SECRET`
- 默认关闭匿名注册
- 定期备份数据库和存储目录
- 监控 `/readyz` 和服务日志
- 升级前先备份 SQLite 数据库
