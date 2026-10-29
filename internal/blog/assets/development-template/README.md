# OSS Blog 自定义主题开发模板

本目录是模板源码参考。自定义模板必须作为服务端插件的 `blog_themes` 资源安装，不能独立上传。

## 文件

- `template.html`：页面结构，使用 Go `html/template` 语法。
- `style.css`：通过 `/themes/<主题名称>/style.css` 提供。
- `theme.js`：通过 `/themes/<主题名称>/theme.js` 提供。
- 不要添加 `settings.json`：模板只负责样式，功能设置由关联插件提供。

在插件管理中保存模板文本资源后，刷新公开页面即可看到结果。发布时更新插件包；不要直接编辑 `data/themes` 内的派生副本，插件启用或升级会重新生成它。

## 模板字段

> 自定义模板使用 Go `html/template` 和 `missingkey=zero`。缺少 map 键时得到零值；不存在的结构体字段、未保护的空值链式访问或模板语法错误仍会失败。服务端回退到 `default`，并通过响应头 `X-Theme-Fallback` 提供原因。完整说明见“插件管理 → 插件指南 → 博客模板契约”。

| 字段 | 说明 |
| --- | --- |
| `.Title` | 页面标题 |
| `.ThemeName` | 当前主题名称 |
| `.ThemeBaseURL` | 当前主题静态资源地址，例如 `/themes/my-theme` |
| `.ThemeConfigJS` | 可安全插入 `<script>` 的 JSON 配置 |
| `.ContentHTML` | Markdown 或目录索引渲染出的 HTML |
| `.IsHome` | 当前是否为 Vault 博客首页 |
| `.IsFolder` | 当前是否为文件夹分享 |
| `.FolderTitle` | 文件夹分享标题 |
| `.ArticleTitle` | 文章标题（文章页） |
| `.AllowCopy` | 当前分享是否允许显示一键复制 |
| `.ShareID` | 当前分享 ID |
| `.BlogHomeURL` | 已开启公开博客时的 `/b/<vault-id>` 地址，否则为空 |
| `.CustomHeader` / `.CustomFooter` | 仓库自定义片段（若存在） |
| `.FooterNotice` | 服务端提示内容 |
| `.BlogName` / `.Description` | 博客名称与介绍 |
| `.MetaDescription` | 当前页面描述，用于 meta description；文章摘要优先，无摘要时回退文章标题，首页与目录页使用博客介绍 |
| `.LogoURL` / `.LogoSize` / `.LogoShape` | Logo 地址、像素尺寸、`square`/`circle` |
| `.BannerURL` / `.MobileBannerURL` | 桌面端与移动端横幅地址 |
| `.Buttons` | 自定义链接列表，元素含 `.Label` / `.URL` / `.IconURL` |
| `.HomePosts` | 首页文章列表；元素含 `.Title` / `.Summary` / `.URL` / `.Date` / `.Category` / `.Tags` / `.CoverURL` / `.WordCount` |
| `.ArticlePost` | 文章元数据（文章页）；含 `.Summary` / `.Date` / `.Category` / `.Tags` / `.CoverURL` / `.WordCount` / `.ReadingMinutes` |

文章 Markdown 顶部完整闭合的 `---` frontmatter（`title` / `description` / `published` / `category` / `tags` / `image`）会作为上表元数据并从正文隐藏；未闭合或损坏的块保留原文。`image` 支持仓库附件路径与 `http(s)` 地址，附件封面自动纳入分享鉴权。

模板字段使用 `{{.Title}}`。普通字段会自动 HTML 转义；`ContentHTML` 已由服务端 Markdown 渲染器生成。不要把不受信任的文本标记为 HTML。

主题名只能使用字母、数字、`-` 与 `_`，长度最多 64。请仅让受信任的服务器管理员编辑此目录。

## 模板专属设置
如果需要设置项，请创建服务端插件，在插件的 `settings` 中声明字段。插件可在 `manifest.json` 的 `blog_themes` 中声明包含本模板文件的资源目录，并在资源目录的 `theme.json` 中用 `public_settings` 列出允许公开模板使用的设置键；管理员从插件管理页上传插件，启用后模板才会出现在仓库设置选择框。

## 发布检查

- 使用 `.ThemeBaseURL` 引用包内 CSS、脚本、图片和字体，不要写死域名或其他模板名。
- 文章、文件夹和博客首页分别检查 `.ContentHTML`、`.IsFolder` 与 `.IsHome` 分支。
- 保留键盘可达的控件、可见焦点和 `prefers-reduced-motion`；复制控件只在 `.AllowCopy` 为真时显示。
- 不要在模板中拼接未经信任的 HTML 或脚本。完整字段和插件资源契约见网页内置「插件指南」。

## 页面与内容契约

- `/b/<vault-id>` 是动态博客首页，使用 `.IsHome` 和 `.HomePosts`，此时 `.ContentHTML` 为空。首页包含全部有效的单篇 Markdown 分享，按分享创建时间倒序排列；不自动列出仅通过文件夹分享的文章。
- `/p/<share-id>` 为单篇文章；文件夹分享会跳转到带尾斜线的目录页。目录页 `.IsFolder=true`；目录下文章的 `.IsFolder=false`。正文和目录 HTML 通过 `.ContentHTML` 输出。
- `default` 只支持分享阅读；`papertrail` 支持博客首页。自定义模板必须实现 `.IsHome` 分支，并建议提供 `theme.json` 的 `supports_public_blog: true`。未提供元数据文件的旧模板按支持首页处理；提供文件后，该字段省略或为 false 均不支持首页。
- `.VaultID`、`.PluginData` 也可使用。插件数据通过 `pluginField`、`hasPlugin` 或带 `with` 守卫的 `index` 读取；没有 `.ThemeConfig` 字段，配置使用 `.ThemeConfigJS`。
- `public_settings` 生效需同时声明 `supports_public_blog: true`，且键存在于所属已启用插件的 settings schema 中。不要将密钥列入公开白名单。
- Markdown YAML 摘要支持 `|`、`>` 多行写法，标签支持 YAML 列表。`summary`、`date`、`cover` 分别为 `description`、`published`、`image` 的备用字段。
- 图片 `./`、`../` 相对文章目录解析，不能越出仓库；其他路径先匹配仓库根路径，再匹配文章相对路径。裸文件名最后按同仓库附件名查找。`http(s)` 图片与封面直接使用外部地址；以 `/` 开头的封面是站点 URL。
- 文件夹分享的资源 URL 可带 `source` 指定文章；来源必须仍在该分享内且确实引用该资源。请使用服务端生成的资源 URL。
