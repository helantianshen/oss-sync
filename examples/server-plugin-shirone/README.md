# Shirone 博客模板插件

适配 OSS Sync 当前 `0.2.6`（`246a261`）的模板资源接口。源码、设置声明和打包工具全部位于本目录；安装不需要修改服务端代码。

## 构建与安装

在项目根目录执行：

```sh
python3 examples/server-plugin-shirone/build.py
```

得到 `bin/themes/shirone.zip` 和 `shirone.zip.sha256`。生成过程仅依赖 Python 3 标准库，时间戳固定，相同源码产生相同 ZIP。

1. 管理员打开「插件管理」，上传 `shirone.zip`。上传流程会安装并启用插件。
2. 仓库设置选择 **Shirone**（资源名 `shirone--blog`），需要首页时启用公开博客。
3. 打开「插件设置 → Shirone 博客模板」，选择对应仓库，设置名称、介绍、头像、桌面/移动横幅和自定义链接。
4. 访问 `/b/<vault-id>` 查看首页。文章仍需建立单篇分享；只通过文件夹分享的文章不会出现在首页索引中。

当前服务端的模板属于插件资源，没有独立的模板上传入口。旧 `shirone-custom.zip` 的根目录格式不适用于当前流程；本包以 `manifest.json`、`plugin.wasm` 和 `blog/shirone/` 组成，不嵌套 `plugin.zip`。

插件 ID 为 `shirone`，模板资源 ID 为 `blog`。同 ID 再次上传走现有插件升级流程；发布资源修改时先提升 `manifest.json.version`，再重新构建上传。当前宿主用 manifest 与 WASM 内容判断重复包，版本不变且仅修改 CSS/JS 时可能仍保留旧资源。不要编辑 `data/themes/shirone--blog` 派生目录，启用或升级插件时它会重新生成。禁用插件后该模板不可用。

## 接口适配

- 首页、单篇文章、文件夹目录和文件夹内文章分别使用当前 `.IsHome`、`.IsFolder`、`.HomePosts`、`.ArticlePost` 与 `.ContentHTML`，保留字体、动态配色及响应式布局。
- `.HomePosts` 仅在首页提供。公开分享页从服务端给出的同源 `.BlogHomeURL` 获取当前 Vault 首页，仅抽取文章索引文本，补全搜索、分类、标签、日历、统计与推荐；不会执行返回页面的脚本。独立私有分享不请求首页，也不暴露其他文章。
- 索引加载失败时显示明确状态，不把失败当成零篇文章；文章阅读和显示设置仍可使用。大仓库每次阅读页需要额外读取一次公开首页。
- 博客名称未配置时首页使用 `.Title`（仓库名），分享页成功读取首页后使用相同名称。未公开的独立分享未配置名称时显示 `OSS Blog`。介绍按已有接口返回值展示。
- 元数据、复制权限、附件授权与编码均交给现有服务端。模板直接使用 `.CoverURL` 和正文生成的附件 URL，保留文件夹 `source`、查询参数与片段语义。
- `manifest.json.settings` 声明 8 个展示字段，`theme.json.public_settings` 显式允许这些字段公开；宿主按已启用所属插件、当前 Vault 和白名单读取。设置型 WASM 无外部导入、路由、Hook、数据库或网络操作。
- CSS、脚本、字体与图片通过 `.ThemeBaseURL` 加载，资源名和域名不写死；完整中文字体约 6 MiB，无 CDN 依赖。

可选 Markdown 元数据、许可与页面说明见 [模板 README](blog/shirone/README.md)。

## 验证

```sh
node --check examples/server-plugin-shirone/blog/shirone/theme.js
```

安装验证需通过 Python 3 构建实际 ZIP，使用插件管理器安装并启用，再检查模板渲染、公开配置、资源、分享隔离和分页输入。浏览器交互需另行验证。自定义模板失败时响应可能仍为 200，应同时检查 `X-Theme-Fallback` 缺失和 `data-template="shirone"` 标记存在。
