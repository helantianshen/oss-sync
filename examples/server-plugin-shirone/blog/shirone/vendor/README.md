# Material Color Utilities

`../material-color.js` 包含 Google 的 `@material/material-color-utilities` **0.4.0**，采用 Apache-2.0 许可；完整许可位于 [material-color-utilities.LICENSE](material-color-utilities.LICENSE)。打包文件保留第三方版权和许可声明。

`../material-color.source.js` 是浏览器适配入口，沿用 Shirone 的 HCT 种子参数（chroma 60 / tone 50）、9 种方案、2021 / 2025 规范与 51 个颜色角色。浏览器不需要 npm、CDN 或其他运行时，加载打包文件后通过 `ShironeColor.resolveScheme(hue, dark, style, spec)` 获取配色。

维护时，在临时构建目录安装固定版本 `@material/material-color-utilities@0.4.0` 与 `esbuild@0.28.2`，将入口复制到该目录后执行：

```sh
esbuild material-color.source.js --bundle --minify --format=iife --global-name=ShironeColor --target=es2020 --legal-comments=inline --outfile=material-color.js
```

将输出复制回模板目录，并保留文件顶部的依赖版本与许可证说明。验证命令（项目根目录）：

```sh
node --check examples/server-plugin-shirone/blog/shirone/theme.js
```
