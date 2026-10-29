import tseslint from "typescript-eslint";
import obsidianmd from "eslint-plugin-obsidianmd";

// 覆盖官方审核报告中的源码规则，类型正确性由 tsc 单独检查
export default [{
  files: ["src/**/*.ts"],
  languageOptions: { parser: tseslint.parser, parserOptions: { projectService: true } },
  plugins: { "@typescript-eslint": tseslint.plugin, obsidianmd },
  rules: {
    "@typescript-eslint/no-explicit-any": "error",
    "@typescript-eslint/no-unused-vars": ["error", { args: "none", caughtErrors: "none", ignoreRestSiblings: true }],
    "no-empty": "error",
    "no-alert": "error",
    "no-useless-escape": "error",
    "no-console": ["error", { allow: ["debug", "warn", "error"] }],
    "obsidianmd/settings-tab/no-manual-html-headings": "error",
    "obsidianmd/settings-tab/no-problematic-settings-headings": "error",
    "obsidianmd/settings-tab/prefer-setting-definitions": "warn",
    "obsidianmd/hardcoded-config-path": "error",
    "obsidianmd/no-global-this": "error",
    "obsidianmd/prefer-create-el": "error",
    "obsidianmd/prefer-window-timers": "error",
    "obsidianmd/no-nodejs-modules": "error",
  },
}];
